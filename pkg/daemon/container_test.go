package daemon

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
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/preflight"
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

func TestContainerized(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value string
		want  bool
	}{
		"unset":   {value: "", want: false},
		"true":    {value: "true", want: true},
		"one":     {value: "1", want: true},
		"false":   {value: "false", want: false},
		"invalid": {value: "yes-please", want: false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string {
				if key == ContainerizedEnv {
					return tc.value
				}
				return ""
			}
			if got := containerized(getenv); got != tc.want {
				t.Errorf("containerized() with %s=%q = %v, want %v", ContainerizedEnv, tc.value, got, tc.want)
			}
		})
	}
}

func TestCredentialPlugin(t *testing.T) {
	t.Parallel()

	const (
		running = "/opt/aks-flex-node/bin/aks-flex-node"
		host    = "/opt/unbounded/agent/bin/aks-flex-node"
	)
	tests := map[string]struct {
		containerized bool
		executableErr error
		want          string
		wantErr       bool
	}{
		"a host runs the host's agent":               {want: host},
		"the container runs the running binary":      {containerized: true, want: running},
		"the container fails without its own binary": {containerized: true, executableErr: errors.New("no executable"), wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			resolved := false
			executable := func() (string, error) {
				resolved = true
				if tc.executableErr != nil {
					return "", tc.executableErr
				}
				return running, nil
			}
			got, err := credentialPlugin(tc.containerized, executable, func() string { return host })
			if (err != nil) != tc.wantErr {
				t.Fatalf("credentialPlugin(containerized=%v) error = %v, want error %v", tc.containerized, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("credentialPlugin(containerized=%v) = %q, want %q", tc.containerized, got, tc.want)
			}
			if resolved != tc.containerized {
				t.Errorf("credentialPlugin(containerized=%v) resolved the running binary: %v", tc.containerized, resolved)
			}
		})
	}
}

func TestEnsureHostInstallDoesNothingInAContainer(t *testing.T) {
	t.Parallel()

	// A nil config would fail any attempt to converge the host install, and the host install
	// itself writes under /usr/local and /etc/systemd, which a test must not touch.
	convergeHeld, err := ensureHostInstall(t.Context(), discardLogger(), nil, true)
	if err != nil {
		t.Fatalf("ensureHostInstall(containerized=true) = %v, want nil", err)
	}
	// Nothing was held back, so nothing is written once an upgrade is judged either.
	if err := convergeHeld(t.Context()); err != nil {
		t.Fatalf("ensureHostInstall's step after an upgrade's judgment = %v, want nil", err)
	}
}

func TestSetupHostInstallsPackagesOnlyOnTheHost(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		containerized bool
		wantInstall   bool
	}{
		"on the host":    {containerized: false, wantInstall: true},
		"in a container": {containerized: true, wantInstall: false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			task := setupHost(&config.Config{}, discardLogger(), tc.containerized)
			if got := strings.Contains(task.Name(), "install-packages"); got != tc.wantInstall {
				t.Errorf("setupHost(containerized=%v) installs packages = %v, want %v (tasks: %s)",
					tc.containerized, got, tc.wantInstall, task.Name())
			}
			if !strings.Contains(task.Name(), "configure-os") {
				t.Errorf("setupHost(containerized=%v) does not configure the OS (tasks: %s)", tc.containerized, task.Name())
			}
		})
	}
}

type namedCheck string

func (c namedCheck) Name() string                           { return string(c) }
func (namedCheck) Check(context.Context) []preflight.Result { return nil }

func TestPreflightChecksInAContainer(t *testing.T) {
	t.Parallel()

	all := []string{"privileged-user", "host-packages", "containerd-active", "swap-active"}
	tests := map[string]struct {
		containerized bool
		want          []string
	}{
		"on the host, every check":            {containerized: false, want: all},
		"in a container, the ones that apply": {containerized: true, want: []string{"privileged-user", "swap-active"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var checks []preflight.Checker
			for _, name := range all {
				checks = append(checks, namedCheck(name))
			}
			var got []string
			for _, check := range preflightChecks(checks, tc.containerized) {
				got = append(got, check.Name())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("checks = %q, want %q", got, tc.want)
			}
			if len(checks) != len(all) || checks[1].Name() != "host-packages" {
				t.Errorf("preflightChecks changed its argument: %v", checks)
			}
		})
	}
}

// The checks a container leaves out are unbounded's; this notices if they're renamed.
func TestContainerSkippedPreflightChecksExist(t *testing.T) {
	t.Parallel()

	var names []string
	for _, check := range host.Preflight(discardLogger(), agentconfig.AgentConfig{}, nil) {
		names = append(names, check.Name())
	}
	for _, skipped := range containerSkippedPreflightChecks {
		if !slices.Contains(names, skipped) {
			t.Errorf("unbounded's host preflight has no %q check (has %q)", skipped, names)
		}
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

func TestContainerAgentUnitClearsRecoveryAndNeedsNoUsrLocal(t *testing.T) {
	t.Parallel()

	// Its start waits for recovery's, so a recovery that outlived its unit can't run beside it.
	clearRecovery := "-/usr/bin/ctr --namespace aks-flex task delete --force aks-flex-node-recovery"
	if got := unitDirectives(containerServiceUnitContent, "Service", "ExecStartPre"); !strings.Contains(strings.Join(got, " "), clearRecovery) {
		t.Errorf("ExecStartPre= is %q, want it to include %s", got, clearRecovery)
	}
	// The install no longer mounts anything at /usr/local, and nothing of the agent's is there.
	if got := unitDirectives(containerServiceUnitContent, "Unit", "RequiresMountsFor"); slices.Contains(got, "/usr/local") {
		t.Errorf("RequiresMountsFor= is %q, want it not to include /usr/local", got)
	}
}

// TestContainerNSpawnLifecycleUnits pins the container's units the machines' hooks start, as
// TestNSpawnLifecycleUnits pins a host's: each runs the phase in a container of the agent's image,
// which is on the host, so it pulls nothing.
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

// TestRetireNSpawnLifecycleHelper: the helper goes only once the hooks that ran it are rewritten.
func TestRetireNSpawnLifecycleHelper(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		helper      bool
		rewriteErr  error
		wantRewrite bool
		wantHelper  bool
		wantErr     bool
	}{
		"does nothing without a helper": {},
		"rewrites the hooks, then removes the helper": {
			helper:      true,
			wantRewrite: true,
		},
		"keeps the helper when the hooks can't be rewritten": {
			helper:      true,
			rewriteErr:  os.ErrPermission,
			wantRewrite: true,
			wantHelper:  true,
			wantErr:     true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			helper := filepath.Join(t.TempDir(), nspawnLifecycleHelperName)
			if tc.helper {
				writeExecutable(t, helper, "agent")
			}
			rewrote := false
			err := retireNSpawnLifecycleHelperAt(t.Context(), discardLogger(), helper, func(context.Context) error {
				rewrote = true
				if _, err := os.Lstat(helper); err != nil {
					t.Errorf("the helper was gone before the hooks were rewritten: %v", err)
				}
				return tc.rewriteErr
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("retireNSpawnLifecycleHelperAt() = %v, want error %v", err, tc.wantErr)
			}
			if rewrote != tc.wantRewrite {
				t.Errorf("rewrote = %v, want %v", rewrote, tc.wantRewrite)
			}
			if _, err := os.Lstat(helper); (err == nil) != tc.wantHelper {
				t.Errorf("helper present = %v, want %v", err == nil, tc.wantHelper)
			}
		})
	}
}
