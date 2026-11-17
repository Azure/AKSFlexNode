package agentdeployment

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestServiceTargetsFromConfig(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone} {
		for _, arc := range []bool{false, true} {
			name := string(mode) + " without ARC"
			if arc {
				name = string(mode) + " with ARC"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				cfg := &config.Config{
					Azure: config.AzureConfig{
						Arc: &config.ArcConfig{Enabled: arc},
					},
				}
				targets := ServiceTargetsFromConfig(cfg, mode)
				after := []string{"network-online.target", "systemd-udev-settle.service"}
				wants := []string{"network-online.target", "systemd-udev-settle.service"}
				if mode == isolation.ModeNSpawn {
					wants = append(wants, "machines.target")
				}
				if arc {
					after = append(after, "himdsd.service")
					wants = append(wants, "himdsd.service")
				}
				if !slices.Equal(targets.AfterTargets, after) || !slices.Equal(targets.WantsTargets, wants) {
					t.Fatalf("targets = %+v, want After=%q Wants=%q", targets, after, wants)
				}

				unit, err := renderAgentServiceUnit("/opt/unbounded/agent/bin/aks-flex-node", targets)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(unit), "machines.target") != (mode == isolation.ModeNSpawn) {
					t.Fatal("service selected the wrong machines.target dependency")
				}
				if strings.Contains(string(unit), arcSystemdDependency) != arc {
					t.Fatal("service lost ARC dependency selection")
				}
			})
		}
	}
}

func TestActivationPlansExplicitTargets(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	dir := t.TempDir()
	recovery := filepath.Join(t.TempDir(), "recovery.sh")
	cfg := &config.Config{
		Azure: config.AzureConfig{
			Arc: &config.ArcConfig{Enabled: true},
		},
	}
	targets := ServiceTargetsFromConfig(cfg, isolation.ModeNone)
	if err := writeAgentServiceAssets(paths, targets, dir, recovery, paths.CurrentPath, false); err != nil {
		t.Fatal(err)
	}
	service := &HostActivationService{
		log:            discardLogger(),
		paths:          paths,
		systemdDir:     dir,
		recoveryScript: recovery,
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
			return false, nil
		},
	}
	plan, err := service.Preflight(t.Context(), paths.CurrentPath, targets)
	if err != nil || plan.UpdateRequired {
		t.Fatalf("none plan = %+v, %v", plan, err)
	}

	targets = ServiceTargetsFromConfig(cfg, isolation.ModeNSpawn)
	plan, err = service.Preflight(t.Context(), paths.CurrentPath, targets)
	if err != nil || !plan.UpdateRequired {
		t.Fatalf("nspawn plan = %+v, %v", plan, err)
	}
	if err := service.Prepare(t.Context(), paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	plan, err = service.Preflight(t.Context(), paths.CurrentPath, targets)
	if err != nil || plan.UpdateRequired {
		t.Fatalf("prepared nspawn plan = %+v, %v", plan, err)
	}
}
