package daemon

import (
	"context"
	"log/slog"
	"os"
	"strconv"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// ContainerizedEnv marks the agent as running in its container image, which sets it. There, the
// image and the container's systemd unit stand in for what the agent otherwise installs on the
// host: OS packages, and its own binary layout and units, which start installs and the daemon
// keeps converged.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

// Containerized reports whether the agent runs in its container image.
func Containerized() bool {
	return containerized(os.Getenv)
}

func containerized(getenv func(string) string) bool {
	on, err := strconv.ParseBool(getenv(ContainerizedEnv))
	return err == nil && on
}

// ensureHostInstall converges the agent's binary layout and units on the host. In its container
// the agent has none: the image and the container's own unit run it.
func ensureHostInstall(ctx context.Context, log *slog.Logger, cfg *config.Config, containerized bool) error {
	if containerized {
		return nil
	}
	return ensureAgentUpgradeServiceAssets(ctx, log, cfg)
}
