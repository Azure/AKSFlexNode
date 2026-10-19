package daemon

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// ContainerizedEnv marks the agent as running in its container image, which sets it. There, the
// image carries the agent and the host's provisioning its OS packages, so the agent installs
// neither on the host, and the agent's unit runs the image rather than a binary.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

// containerServiceUnitContent is the agent's unit in its container: it runs the daemon in a
// container of the agent's image, which the host's provisioning names.
//
//go:embed assets/aks-flex-node-agent-container.service
var containerServiceUnitContent []byte

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
// its container the agent has no binary layout: the image carries it.
func ensureHostInstall(ctx context.Context, log *slog.Logger, cfg *config.Config, containerized bool) (bool, error) {
	if containerized {
		return false, nil
	}
	return ensureDaemonServiceAssets(ctx, log, cfg)
}

// installContainerServiceTask installs, enables, and starts the agent's unit in its container,
// as installServiceTask does on a host. The unit has the same name, so UninstallService removes
// it in either case.
type installContainerServiceTask struct {
	log       *slog.Logger
	unitPath  string
	systemctl func(ctx context.Context, args ...string) error
}

func newInstallContainerServiceTask(log *slog.Logger) *installContainerServiceTask {
	return &installContainerServiceTask{
		log:      log,
		unitPath: filepath.Join(systemdSystemDir, ServiceUnitName),
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
	}
}

func (t *installContainerServiceTask) Name() string { return "install-service" }

func (t *installContainerServiceTask) Do(ctx context.Context) error {
	if err := utilio.WriteFile(t.unitPath, containerServiceUnitContent, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", t.unitPath, err)
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
