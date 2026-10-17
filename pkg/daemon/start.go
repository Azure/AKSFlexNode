package daemon

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

func SetupHost(cfg *config.Config, log *slog.Logger) phases.Task {
	return setupHost(cfg, log, Containerized())
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

func stageContainerImageArchiveBindSource(log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) phases.Task {
	// TODO: move this responsibility back into Unbounded. Its nspawn template
	// owns the unconditional container-image archive bind mount, so Unbounded
	// should also ensure the host-side bind source exists for online and offline
	// bootstrap paths.
	return rootfs.DownloadContainerImageArchives(log, staging)
}

func StartNode(
	cfg *config.Config,
	log *slog.Logger,
	machineName string,
	gs *goalstates.MachineGoalState,
	containerImageArchives *goalstates.ContainerImageArchiveStaging,
	store stateStore,
	state *State,
) phases.Task {
	return phases.Serial(log,
		stageContainerImageArchiveBindSource(log, containerImageArchives),
		rootfs.Provision(log, gs.RootFS),
		ConfigureNSpawnDependencies(log, gs.RootFS, cfg.Bootstrap.AdditionalRequiredServices),
		phases.Parallel(log,
			npd.Download(log, cfg, gs.RootFS.MachineDir),
			InstallBinary(gs.RootFS.MachineDir),
		),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, machineName),
		npd.Start(log, cfg, gs.NodeStart),
		saveState(store, state),
	)
}
