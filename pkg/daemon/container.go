package daemon

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
)

// containerServiceUnitContent is the agent's unit in its container: it runs the daemon in a
// container of the agent's image, which the host's provisioning names, or an AgentUpgrade
// selected.
//
//go:embed assets/aks-flex-node-agent-container.service
var containerServiceUnitContent []byte

// containerRecoveryUnitContent is the agent's recovery unit in its container, which restores the
// last-good image when an upgraded agent's unit keeps failing.
//
//go:embed assets/aks-flex-node-agent-recovery-container.service
var containerRecoveryUnitContent []byte

// containerNSpawnPreStartUnitContent and containerNSpawnPostStartUnitContent are the nspawn
// lifecycle units in the agent's container, which the machines' hooks start: they run the agent's
// nspawn-lifecycle phases in containers of its image, so no binary of the agent is on the host.
//
//go:embed assets/aks-flex-node-nspawn-pre-start-container@.service
var containerNSpawnPreStartUnitContent []byte

//go:embed assets/aks-flex-node-nspawn-post-start-container@.service
var containerNSpawnPostStartUnitContent []byte

// containerNSpawnLifecycleUnitAssets returns the container's nspawn lifecycle units, under the
// names the machines' hooks start; see config.NSpawnPreStartUnit.
func containerNSpawnLifecycleUnitAssets(systemdDir string) []agentServiceAsset {
	return []agentServiceAsset{
		{path: filepath.Join(systemdDir, config.NSpawnPreStartUnit), content: containerNSpawnPreStartUnitContent, mode: 0o644},
		{path: filepath.Join(systemdDir, config.NSpawnPostStartUnit), content: containerNSpawnPostStartUnitContent, mode: 0o644},
	}
}

// ensureHostInstall converges the agent's binary layout and units on the host, as
// ensureDaemonServiceAssets does, and returns what writes those it held back while an
// AgentUpgrade was pending, for the daemon to run once the upgrade is judged. In its container
// the agent has no binary layout, the image carries it, and containerAgentConvergence converges
// its units, so it does nothing there.
func ensureHostInstall(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	containerized bool,
) (func(context.Context) error, error) {
	if containerized {
		return noHeldAssets, nil
	}
	held, err := ensureDaemonServiceAssets(ctx, log, cfg)
	if err != nil || !held {
		return noHeldAssets, err
	}
	return func(ctx context.Context) error {
		_, err := ensureDaemonServiceAssets(ctx, log, cfg)
		return err
	}, nil
}

// noHeldAssets is what the daemon runs once an upgrade is judged when it held nothing back.
func noHeldAssets(context.Context) error { return nil }

// installContainerServiceTask installs, enables, and starts the agent's unit in its container,
// with its recovery unit, as installServiceTask does on a host. They have the same names, so
// UninstallService removes them in either case.
type installContainerServiceTask struct {
	log       *slog.Logger
	unitDir   string
	systemctl func(ctx context.Context, args ...string) error
}

func newInstallContainerServiceTask(log *slog.Logger) *installContainerServiceTask {
	return &installContainerServiceTask{
		log:     log,
		unitDir: systemdSystemDir,
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
	}
}

func (t *installContainerServiceTask) Name() string { return "install-service" }

func (t *installContainerServiceTask) Do(ctx context.Context) error {
	if _, err := writeContainerUnits(t.unitDir, false); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", ServiceUnitName},
		{"start", ServiceUnitName},
	} {
		if err := t.systemctl(ctx, args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", args[0], err)
		}
	}
	t.log.Info("systemd service installed and started", "unit", ServiceUnitName)
	return nil
}
