package daemon

import (
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

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
