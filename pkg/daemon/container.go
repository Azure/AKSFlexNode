package daemon

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
)

// ContainerizedEnv marks the agent as running in its container image, which sets it. There, the
// image carries the agent and the host's provisioning its OS packages, so the agent installs
// neither on the host, and the agent's unit runs the image rather than a binary.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

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

// Containerized reports whether the agent runs in its container image.
func Containerized() bool {
	return containerized(os.Getenv)
}

func containerized(getenv func(string) string) bool {
	on, err := strconv.ParseBool(getenv(ContainerizedEnv))
	return err == nil && on
}

// ensureHostInstall converges the agent's binary layout and units on the host, and reports
// whether it held them back while an AgentUpgrade is pending; see ensureDaemonServiceAssets. In
// its container the agent has no binary layout, the image carries it, and
// containerAgentConvergence converges its units.
func ensureHostInstall(ctx context.Context, log *slog.Logger, cfg *config.Config, containerized bool) (bool, error) {
	if containerized {
		return false, nil
	}
	return ensureDaemonServiceAssets(ctx, log, cfg)
}

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
	if _, err := writeContainerUnits(t.unitDir); err != nil {
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
