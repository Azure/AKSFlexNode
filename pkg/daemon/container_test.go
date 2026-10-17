package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

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
	if err := ensureHostInstall(t.Context(), discardLogger(), nil, true); err != nil {
		t.Fatalf("ensureHostInstall(containerized=true) = %v, want nil", err)
	}
}

func TestUninstallServiceStopsTheContainerInAContainer(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		containerized bool
		want          string
	}{
		"on the host":    {containerized: false, want: "uninstall-service"},
		"in a container": {containerized: true, want: "stop-agent-container"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := uninstallService(discardLogger(), tc.containerized).Name(); got != tc.want {
				t.Errorf("uninstallService(containerized=%v) = %q, want %q", tc.containerized, got, tc.want)
			}
		})
	}
}

func TestStopContainerService(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		failOn         string
		stopEndsCaller bool
		wantCalls      []string
		wantErr        bool
	}{
		"disables then stops the unit": {
			wantCalls: []string{
				"disable " + ContainerServiceUnitName,
				"stop " + ContainerServiceUnitName,
			},
		},
		// Stopping a unit that would come back at the next boot leaves the agent half stopped.
		"doesn't stop the unit when disabling fails": {
			failOn:    "disable",
			wantCalls: []string{"disable " + ContainerServiceUnitName},
			wantErr:   true,
		},
		"reports a failed stop": {
			failOn: "stop",
			wantCalls: []string{
				"disable " + ContainerServiceUnitName,
				"stop " + ContainerServiceUnitName,
			},
			wantErr: true,
		},
		// As when the daemon stops its own container: the stop's SIGTERM cancels its context,
		// which kills systemctl.
		"a stop that ends the caller isn't a failure": {
			failOn:         "stop",
			stopEndsCaller: true,
			wantCalls: []string{
				"disable " + ContainerServiceUnitName,
				"stop " + ContainerServiceUnitName,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			task := newStopContainerServiceTask(discardLogger())
			task.systemctl = func(_ context.Context, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				if args[0] == "stop" && tc.stopEndsCaller {
					cancel()
				}
				if args[0] == tc.failOn {
					return errors.New("systemctl failed")
				}
				return nil
			}
			err := task.Do(ctx)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Do() error = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("systemctl calls = %q, want %q", calls, tc.wantCalls)
			}
		})
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
