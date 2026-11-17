package agentdeployment

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNoneDaemonUnitDoesNotRequireMachinesTarget(t *testing.T) {
	t.Parallel()

	for _, arc := range []bool{false, true} {
		name := "without ARC"
		if arc {
			name = "with ARC"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			unit, err := renderAgentServiceUnit("/opt/unbounded/agent/bin/aks-flex-node", agentServiceOptions{
				OmitMachinesTarget: true,
				ARCEnabled:         arc,
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(unit), "machines.target") {
				t.Fatal("none daemon unit references machines.target")
			}
			if strings.Contains(string(unit), arcSystemdDependency) != arc {
				t.Fatal("none service lost ARC dependency selection")
			}

			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ServiceUnitName), unit, 0o644); err != nil {
				t.Fatal(err)
			}
			options, err := installedAgentServiceOptions(dir)
			if err != nil || !options.OmitMachinesTarget || options.ARCEnabled != arc {
				t.Fatalf("installed options = %+v, %v", options, err)
			}
		})
	}
}

func TestActivationPlansAppliedWorkerMode(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	dir := t.TempDir()
	recovery := filepath.Join(t.TempDir(), "recovery.sh")
	options := agentServiceOptions{
		OmitMachinesTarget: true,
		ARCEnabled:         true,
	}
	if err := writeAgentServiceAssets(paths, options, dir, recovery, paths.CurrentPath, false); err != nil {
		t.Fatal(err)
	}
	service := &HostActivationService{
		log:            discardLogger(),
		WorkerMode:     isolation.ModeNone,
		paths:          paths,
		systemdDir:     dir,
		recoveryScript: recovery,
		// Simulate older service detection: applied state must override it.
		serviceOptions: agentServiceOptions{ARCEnabled: true},
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
			return false, nil
		},
	}
	plan, err := service.Preflight(t.Context(), paths.CurrentPath)
	if err != nil || plan.UpdateRequired {
		t.Fatalf("none plan = %+v, %v", plan, err)
	}
	service.WorkerMode = isolation.ModeContainer
	plan, err = service.Preflight(t.Context(), paths.CurrentPath)
	if err != nil || plan.UpdateRequired {
		t.Fatalf("OCI plan = %+v, %v", plan, err)
	}

	service.WorkerMode = isolation.ModeNSpawn
	plan, err = service.Preflight(t.Context(), paths.CurrentPath)
	if err != nil || !plan.UpdateRequired {
		t.Fatalf("nspawn plan = %+v, %v", plan, err)
	}
}
