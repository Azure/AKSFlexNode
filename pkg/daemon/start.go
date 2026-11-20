package daemon

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
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
		tasks = append(tasks, host.InstallPackagesFor(log, cfg.Bootstrap.Isolation))
	}
	mode := cfg.Bootstrap.Isolation
	switch mode {
	case isolation.ModeContainer:
		tasks = append(tasks,
			noneops.LoadKernelModules(log, nil),
			phases.Parallel(log, host.DisableContainerd(log), host.DisableKubelet(log)),
		)
	case isolation.ModeNSpawn:
		services := []phases.Task{host.DisableKubelet(log)}
		// Host containerd runs the containerized agent and must remain active.
		if !containerized {
			services = append(services, host.DisableContainerd(log))
		}
		tasks = append(tasks, phases.Parallel(log, services...))
	}

	nftables := host.ConfigureNFTables(log)
	apt := host.HardenAPT(log)
	if mode == isolation.ModeContainer {
		nftables = host.ConfigureNFTablesFor(log, mode)
		apt = host.RemoveAPTHardening(log)
	}

	tasks = append(tasks, phases.Parallel(log,
		host.ConfigureOS(log),
		nftables,
		host.DisableDocker(log),
		host.DisableSwap(log),
		apt,
		hostrouting.Configure(cfg, log),
	))
	return phases.Serial(log, tasks...)
}
