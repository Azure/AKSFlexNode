package daemon

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/arc"
	"github.com/Azure/AKSFlexNode/pkg/cni"
	"github.com/Azure/AKSFlexNode/pkg/config"
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
		),
	)
}

func StartNode(
	cfg *config.Config,
	log *slog.Logger,
	machineName string,
	gs *goalstates.MachineGoalState,
	store stateStore,
	state *State,
) phases.Task {
	return phases.Serial(log,
		rootfs.Provision(log, gs.RootFS),
		phases.Parallel(log,
			npd.Download(cfg, gs.RootFS.MachineDir),
			InstallBinary(gs.RootFS.MachineDir),
			cni.WriteCNIConfig(gs.RootFS.MachineDir),
		),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, machineName),
		npd.Start(cfg, log, gs.RootFS.MachineDir, machineName),
		saveState(store, state),
	)
}
