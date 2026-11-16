package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNoneHostAgentActivationUsesOnlyHostBinary(t *testing.T) {
	t.Parallel()
	for _, slot := range []string{"", "not-an-nspawn-machine"} {
		t.Run("slot="+slot, func(t *testing.T) {
			t.Parallel()
			paths := testAgentUpgradePaths(t)
			candidate := filepath.Join(t.TempDir(), "candidate")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o750); err != nil {
				t.Fatal(err)
			}
			service := &flexDaemonActivationService{
				log:            discardLogger(),
				paths:          paths,
				state:          &testStateStore{state: &State{Isolation: isolation.ModeNone, ActiveMachine: slot}},
				startup:        containerStartupStore{path: filepath.Join(t.TempDir(), "startup.json")},
				systemdDir:     t.TempDir(),
				recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
				serviceOptions: agentServiceOptions{NoneIsolation: true},
			}
			for range 2 {
				if err := service.Prepare(t.Context(), candidate); err != nil {
					t.Fatal(err)
				}
				if err := service.WaitHealthy(t.Context(), candidate); err != nil {
					t.Fatalf("none activation attempted worker binary synchronization: %v", err)
				}
			}
			unit, err := os.ReadFile(filepath.Join(service.systemdDir, ServiceUnitName))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(unit), "# Node isolation: none") || strings.Contains(string(unit), "machines.target") {
				t.Fatalf("activation did not preserve none service routing:\n%s", unit)
			}
		})
	}
}
