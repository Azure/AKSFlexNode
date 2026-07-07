package daemon

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/arc"
	"github.com/Azure/AKSFlexNode/pkg/cni"
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
	return phases.Serial(log,
		host.InstallPackages(log),
		phases.Parallel(log,
			host.ConfigureOS(log),
			host.ConfigureNFTables(log),
			host.DisableDocker(log),
			host.DisableSwap(log),
			host.HardenAPT(log),
			arc.InstallArc(cfg, log),
			hostrouting.Configure(cfg, log),
		),
	)
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
		phases.Parallel(log,
			npd.Download(cfg, gs.RootFS.MachineDir),
			InstallBinary(gs.RootFS.MachineDir),
			cni.WriteCNIConfig(gs.RootFS.MachineDir),
		),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, machineName),
		npd.Start(log, gs.NodeStart),
		saveState(store, state),
	)
}
