package daemon

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

func SetupHost(cfg *config.Config, log *slog.Logger) phases.Task {
	return setupHost(cfg, log, agentdeployment.Containerized())
}

func setupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	var tasks []phases.Task
	// In a container, the package manager on PATH is the image's, so it would install into the
	// container rather than the host.
	if !containerized {
		tasks = append(tasks, host.InstallPackages(log))
	}
	tasks = append(tasks, phases.Parallel(log,
		host.ConfigureOS(log),
		host.ConfigureNFTables(log),
		host.DisableDocker(log),
		host.DisableSwap(log),
		host.HardenAPT(log),
		hostrouting.Configure(cfg, log),
	))
	return phases.Serial(log, tasks...)
}
