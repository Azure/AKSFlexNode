package daemon

import (
	"bytes"
	"context"
	"errors"
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

func TestEnsureHostInstallDoesNothingInAContainer(t *testing.T) {
	t.Parallel()

	// A nil config would fail any attempt to converge the host install, and the host install
	// itself writes under /usr/local and /etc/systemd, which a test must not touch.
	if held, err := ensureHostInstall(t.Context(), discardLogger(), nil, true); held || err != nil {
		t.Fatalf("ensureHostInstall(containerized=true) = %v, %v; want nothing held, and nil", held, err)
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
