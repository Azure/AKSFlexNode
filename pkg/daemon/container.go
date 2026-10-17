package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
)

// ContainerizedEnv marks the agent as running in its container image, which sets it. There, the
// image and the container's systemd unit stand in for what the agent otherwise installs on the
// host: OS packages, and its own binary layout and units, which start installs and the daemon
// keeps converged.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

// ContainerServiceUnitName is the host's systemd unit that runs the agent's container, as
// ServiceUnitName runs the agent on a host.
const ContainerServiceUnitName = "aks-flex-node-container.service"

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
// its container the agent has none: the image and the container's own unit run it.
func ensureHostInstall(ctx context.Context, log *slog.Logger, cfg *config.Config, containerized bool) (bool, error) {
	if containerized {
		return false, nil
	}
	return ensureDaemonServiceAssets(ctx, log, cfg)
}

// stopContainerServiceTask stops the agent in its container, as uninstallServiceTask does on a
// host: it disables and stops the container's unit. The unit file stays, since the host's
// provisioning wrote it, not start, so enabling the unit again, with a new config, brings the
// agent back.
type stopContainerServiceTask struct {
	log       *slog.Logger
	systemctl func(ctx context.Context, args ...string) error
}

func newStopContainerServiceTask(log *slog.Logger) *stopContainerServiceTask {
	return &stopContainerServiceTask{
		log: log,
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
	}
}

func (t *stopContainerServiceTask) Name() string { return "stop-agent-container" }

func (t *stopContainerServiceTask) Do(ctx context.Context) error {
	// Disabled first, since stopping the unit stops this container, as stopping the agent's own
	// unit stops the agent on a host.
	if err := t.systemctl(ctx, "disable", ContainerServiceUnitName); err != nil {
		return fmt.Errorf("systemctl disable %s: %w", ContainerServiceUnitName, err)
	}
	t.log.Info("stopping the agent container", "unit", ContainerServiceUnitName)
	if err := t.systemctl(ctx, "stop", ContainerServiceUnitName); err != nil {
		// When the stop reaches the agent that asked for it, its SIGTERM cancels ctx, which
		// kills systemctl: the stop is under way.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("systemctl stop %s: %w", ContainerServiceUnitName, err)
	}
	return nil
}
