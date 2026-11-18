package agentdeployment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestInstallServiceRunsTheImageInAContainer(t *testing.T) {
	t.Parallel()

	if _, ok := installService(&config.Config{}, discardLogger(), true).(*installContainerServiceTask); !ok {
		t.Error("installService(containerized=true) doesn't install the container's unit")
	}
	if _, ok := installService(&config.Config{}, discardLogger(), false).(*installServiceTask); !ok {
		t.Error("installService(containerized=false) doesn't install the host's unit")
	}
}

func TestContainerServiceUnitRunsTheDaemonFromTheImage(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile(filepath.Join("testdata", "aks-flex-node-agent-container.service.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(containerServiceUnitContent, want) {
		t.Errorf("container service unit mismatch (-want +got):\n--- want ---\n%s\n--- got ---\n%s", want, containerServiceUnitContent)
	}

	unit := string(containerServiceUnitContent)
	for _, want := range []string{
		"EnvironmentFile=/etc/sysconfig/aks-flex-node-container",
		"--mount type=bind,src=/,dst=/host,options=rbind:rslave",
		"flex-agent-entrypoint aks-flex-node agent --config /etc/aks-flex-node/config.json",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the container's unit lacks %q", want)
		}
	}
}

func TestInstallContainerService(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failOn    string
		wantCalls []string
		wantErr   bool
	}{
		"writes, enables, and starts the unit": {
			wantCalls: []string{"daemon-reload", "enable " + ServiceUnitName, "start " + ServiceUnitName},
		},
		"doesn't start a unit it couldn't enable": {
			failOn:    "enable",
			wantCalls: []string{"daemon-reload", "enable " + ServiceUnitName},
			wantErr:   true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			task := newInstallContainerServiceTask(discardLogger())
			task.unitDir = filepath.Join(t.TempDir(), "systemd")
			task.systemctl = func(_ context.Context, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				if args[0] == tc.failOn {
					return errors.New("systemctl failed")
				}
				return nil
			}

			// Run twice: start can be run again on a host it already set up.
			for run := 1; run <= 2; run++ {
				calls = nil
				err := task.Do(t.Context())
				if (err != nil) != tc.wantErr {
					t.Fatalf("Do() run %d error = %v, want error %v", run, err, tc.wantErr)
				}
				if !slices.Equal(calls, tc.wantCalls) {
					t.Errorf("run %d: systemctl calls = %q, want %q", run, calls, tc.wantCalls)
				}
			}
			for unit, want := range map[string][]byte{
				ServiceUnitName:         containerServiceUnitContent,
				recoveryServiceUnitName: containerRecoveryUnitContent,
			} {
				path := filepath.Join(task.unitDir, unit)
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", unit, err)
				}
				if !bytes.Equal(content, want) {
					t.Errorf("%s isn't the container's", unit)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat %s: %v", unit, err)
				}
				if info.Mode().Perm() != 0o644 {
					t.Errorf("%s's mode = %v, want 0644", unit, info.Mode().Perm())
				}
			}
		})
	}
}

// unitDirectives returns a unit file's values for key in section, split into words, as systemd
// accumulates list settings such as After=.
func unitDirectives(content []byte, section, key string) []string {
	var values []string
	current := ""
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.Trim(line, "[]")
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if ok && current == section && name == key {
			values = append(values, strings.Fields(value)...)
		}
	}
	return values
}

// A failure starts the recovery unit and, RestartSec later, a restart. Recovery starts a
// container, which takes longer, so the restart has to wait for it, or it runs the image that
// failed again.
func TestContainerAgentUnitRestartsAfterRecovery(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		key  string
		want string
	}{
		"a failure starts recovery":         {key: "OnFailure", want: recoveryServiceUnitName},
		"a restart waits for that recovery": {key: "After", want: recoveryServiceUnitName},
		"it starts once containerd has":     {key: "After", want: "containerd.service"},
		// The agent's credentials may come from Arc's identity service.
		"it wants Arc's identity service":   {key: "Wants", want: "himdsd.service"},
		"it starts once Arc's identity has": {key: "After", want: "himdsd.service"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := unitDirectives(containerServiceUnitContent, "Unit", tc.key); !slices.Contains(got, tc.want) {
				t.Errorf("%s= is %q, want it to include %s", tc.key, got, tc.want)
			}
		})
	}
}

// containerd.service reports started before containerd answers, and a start of a unit that runs
// ctr then fails, which counts toward its start limit and, for the agent's unit, starts recovery.
func TestContainerUnitsWaitForContainerd(t *testing.T) {
	t.Parallel()

	tests := map[string][]byte{
		ServiceUnitName:            containerServiceUnitContent,
		recoveryServiceUnitName:    containerRecoveryUnitContent,
		config.NSpawnPreStartUnit:  containerNSpawnPreStartUnitContent,
		config.NSpawnPostStartUnit: containerNSpawnPostStartUnitContent,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var first string
			for _, line := range strings.Split(string(content), "\n") {
				if strings.HasPrefix(line, "ExecStartPre=") || strings.HasPrefix(line, "ExecStart=") {
					first = line
					break
				}
			}
			if !strings.HasPrefix(first, "ExecStartPre=") || !strings.Contains(first, "until /usr/bin/ctr version") {
				t.Errorf("the unit's first command is %q, want a wait for containerd to answer", first)
			}
		})
	}
}

// Each unit's container runs in a scope set up as AgentUpgrade's check sets up its own: bound to
// the unit, killed within the unit's stop timeout, and unloaded when it fails.
func TestContainerUnitsSetUpTheirScopes(t *testing.T) {
	t.Parallel()

	annotations := agentScopeAnnotations("%n")
	for name, content := range map[string][]byte{
		ServiceUnitName:            containerServiceUnitContent,
		recoveryServiceUnitName:    containerRecoveryUnitContent,
		config.NSpawnPreStartUnit:  containerNSpawnPreStartUnitContent,
		config.NSpawnPostStartUnit: containerNSpawnPostStartUnitContent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for i := 0; i < len(annotations); i += 2 {
				want := fmt.Sprintf("%s %q", annotations[i], annotations[i+1])
				if !strings.Contains(string(content), want) {
					t.Errorf("the unit's ctr run lacks %s", want)
				}
			}
		})
	}
}

// The agent's unit's start waits for recovery's, so a recovery that outlived its unit can't run
// beside it.
func TestContainerAgentUnitClearsRecovery(t *testing.T) {
	t.Parallel()

	clearRecovery := "-/usr/bin/ctr --namespace aks-flex task delete --force aks-flex-node-recovery"
	if got := unitDirectives(containerServiceUnitContent, "Service", "ExecStartPre"); !strings.Contains(strings.Join(got, " "), clearRecovery) {
		t.Errorf("ExecStartPre= is %q, want it to include %s", got, clearRecovery)
	}
}

// The container's lifecycle units run each phase in the agent's image, which
// is already on the host, without pulling it.
func TestContainerNSpawnLifecycleUnits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		unit, phase string
		content     []byte
		want        []string
	}{
		{unit: config.NSpawnPreStartUnit, phase: "pre-start", content: containerNSpawnPreStartUnitContent, want: []string{"TimeoutStartSec=2min"}},
		{unit: config.NSpawnPostStartUnit, phase: "post-start", content: containerNSpawnPostStartUnitContent, want: []string{"BindsTo=systemd-nspawn@%i.service", "TimeoutStartSec=infinity"}},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			t.Parallel()

			unit := string(tc.content)
			lines := strings.Split(unit, "\n")
			for _, want := range append(tc.want, "Type=oneshot", "Requires=containerd.service") {
				if !slices.Contains(lines, want) {
					t.Errorf("%s has no line %q", tc.unit, want)
				}
			}
			var execStart string
			for _, line := range strings.Split(strings.ReplaceAll(unit, "\\\n", ""), "\n") {
				if strings.HasPrefix(line, "ExecStart=") {
					execStart = line
				}
			}
			if !strings.HasPrefix(execStart, "ExecStart=/usr/bin/ctr --namespace aks-flex run --rm ") ||
				!strings.HasSuffix(execStart, "flex-agent-entrypoint aks-flex-node nspawn-lifecycle "+tc.phase+" %i") {
				t.Errorf("%s ExecStart= is %q, want a container of the agent's image running the phase", tc.unit, execStart)
			}
			for _, line := range lines {
				// A unit that stays active wouldn't run at the machine's next start, one ordered
				// after the machine would wait for the start that waits for it, and pulling the
				// image could hold the machine's start on the registry.
				if strings.HasPrefix(line, "RemainAfterExit=") || strings.HasPrefix(line, "[Install]") ||
					(strings.HasPrefix(line, "After=") && strings.Contains(line, "systemd-nspawn@")) ||
					strings.Contains(line, " pull ") {
					t.Errorf("%s has %q", tc.unit, line)
				}
			}
		})
	}
}

func TestDaemonUnitsWantMachinesTarget(t *testing.T) {
	t.Parallel()

	hostUnit, err := renderAgentServiceUnit("/usr/local/bin/aks-flex-node", ServiceTargetsFromConfig(&config.Config{}, config.DefaultIsolationMode))
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"host":      hostUnit,
		"container": containerServiceUnitContent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, line := range strings.Split(string(content), "\n") {
				if wants, ok := strings.CutPrefix(line, "Wants="); ok && slices.Contains(strings.Fields(wants), "machines.target") {
					return
				}
			}
			t.Fatal("daemon unit must want machines.target so workers start even when the target is disabled")
		})
	}
}
