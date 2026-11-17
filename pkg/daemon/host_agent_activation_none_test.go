package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestHostAgentActivationUsesAppliedIsolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		mode           isolation.Mode
		slot           string
		machinesTarget bool
	}{
		{name: "none legacy unit", mode: isolation.ModeNone},
		{name: "none invalid slot", mode: isolation.ModeNone, slot: "not-an-nspawn-machine"},
		{name: "none stale machines target", mode: isolation.ModeNone, machinesTarget: true},
		{name: "nspawn legacy unit", mode: isolation.ModeNSpawn},
		{name: "legacy state and unit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for name, arc := range map[string]bool{"without Arc": false, "with Arc": true} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					paths := testAgentUpgradePaths(t)
					systemdDir := t.TempDir()
					unitPath := filepath.Join(systemdDir, ServiceUnitName)
					legacyUnit := "[Unit]\nAfter=network-online.target"
					if arc {
						legacyUnit += " " + arcSystemdDependency
					}
					legacyUnit += "\nWants=network-online.target"
					if tc.machinesTarget {
						legacyUnit += " machines.target"
					}
					legacyUnit += "\n"
					if err := os.WriteFile(unitPath, []byte(legacyUnit), 0o644); err != nil {
						t.Fatal(err)
					}
					options, err := installedAgentServiceOptions(systemdDir)
					if err != nil {
						t.Fatal(err)
					}
					service := &flexDaemonActivationService{
						log:            discardLogger(),
						paths:          paths,
						state:          &testStateStore{state: &State{Isolation: tc.mode, ActiveMachine: tc.slot}},
						systemdDir:     systemdDir,
						recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
						serviceOptions: options,
						inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
							return false, nil
						},
					}
					// Rollback prepares the same assets with the last-good binary.
					for _, binary := range []string{"candidate", "last-good"} {
						candidate := filepath.Join(t.TempDir(), binary)
						if err := os.WriteFile(candidate, []byte(binary), 0o750); err != nil {
							t.Fatal(err)
						}
						before, err := os.ReadFile(unitPath)
						if err != nil {
							t.Fatal(err)
						}
						plan, err := service.Preflight(t.Context(), candidate)
						if err != nil || !plan.UpdateRequired {
							t.Fatalf("Preflight(%s) = %+v, %v; want an asset update", binary, plan, err)
						}
						after, err := os.ReadFile(unitPath)
						if err != nil || string(after) != string(before) {
							t.Fatalf("Preflight mutated the installed unit: %v", err)
						}
						if binary == "candidate" {
							if _, err := os.Stat(service.recoveryScript); !errors.Is(err, os.ErrNotExist) {
								t.Fatalf("Preflight wrote recovery assets: %v", err)
							}
						}
						if err := service.Prepare(t.Context(), candidate); err != nil {
							t.Fatal(err)
						}
						unit, err := os.ReadFile(unitPath)
						if err != nil {
							t.Fatal(err)
						}
						none := tc.mode == isolation.ModeNone
						if strings.Contains(string(unit), "# Node isolation:") || strings.Contains(string(unit), "machines.target") == none {
							t.Fatalf("activation rendered incorrect isolation dependencies:\n%s", unit)
						}
						if strings.Contains(string(unit), arcSystemdDependency) != arc {
							t.Fatalf("activation changed installed Arc dependency:\n%s", unit)
						}
						if !strings.Contains(string(unit), "ExecStart="+candidate+" agent ") {
							t.Fatalf("activation did not select %s:\n%s", binary, unit)
						}
						if plan, err := service.Preflight(t.Context(), candidate); err != nil || plan.UpdateRequired {
							t.Fatalf("Preflight after Prepare = %+v, %v; want current assets", plan, err)
						}
						// Invalid nspawn slots fail before touching a worker's filesystem.
						err = service.WaitHealthy(t.Context(), candidate)
						if none && err != nil {
							t.Fatalf("none activation attempted worker binary synchronization: %v", err)
						}
						if !none && (err == nil || !strings.Contains(err.Error(), "no valid active node slot")) {
							t.Fatalf("nspawn activation skipped worker validation: %v", err)
						}
					}
				})
			}
		})
	}
}
