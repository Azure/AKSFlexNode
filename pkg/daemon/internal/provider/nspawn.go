package provider

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

type nspawnProvider struct{}

func (nspawnProvider) GoalState(log *slog.Logger, node Node) (*goalstates.MachineGoalState, error) {
	if node.GoalState != nil {
		return node.GoalState, nil
	}
	return goalstates.ResolveMachine(log, node.Config, node.Slot, nil)
}

func (p nspawnProvider) Start(ctx context.Context, log *slog.Logger, params StartParams) (bool, error) {
	gs, err := p.GoalState(log, params.Node)
	if err != nil {
		return false, err
	}
	err = phases.Serial(log,
		rootfs.DownloadContainerImageArchives(log, params.Archives),
		rootfs.Provision(log, gs.RootFS),
		ConfigureNSpawnDependencies(log, gs.RootFS, params.Config.Bootstrap.AdditionalRequiredServices),
		phases.Parallel(log, npd.Download(log, params.Config, gs.RootFS.Root()), InstallBinary(gs.RootFS.Root())),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, params.Node.Slot),
		npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
	).Do(ctx)
	return err == nil, err
}

func (p nspawnProvider) Restart(ctx context.Context, log *slog.Logger, params StartParams) error {
	cfg := params.Config.DeepCopy()
	cfg.Components.Kubernetes = params.Node.Config.Cluster.Version
	_, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, params.Node.Slot)
	if err != nil {
		return err
	}
	return phases.Serial(log,
		rootfs.DownloadContainerImageArchives(log, archives),
		nodestop.StopNode(log, params.Node.Slot),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, params.Node.Slot),
		npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
	).Do(ctx)
}

func (p nspawnProvider) Upgrade(ctx context.Context, log *slog.Logger, params UpgradeParams) error {
	if err := phases.Serial(log, nodestop.StopNode(log, params.Previous.Slot), reset.CleanupNetwork(log)).Do(ctx); err != nil {
		return err
	}
	_, err := p.Start(ctx, log, params.StartParams)
	return err
}
func (nspawnProvider) Recover(ctx context.Context, log *slog.Logger, params StartParams) error {
	return nil
}
func (nspawnProvider) Retire(ctx context.Context, log *slog.Logger, params UpgradeParams) error {
	return reset.CleanupMachine(log, params.Previous.Slot).Do(ctx)
}
func (nspawnProvider) RewriteUnits(ctx context.Context, log *slog.Logger, node Node) error {
	rf, err := goalstates.ResolveNSpawnConfig(node.Config, node.Slot)
	if err != nil {
		return err
	}
	return phases.Serial(log, NSpawnLifecycleTasks(log, rf)...).Do(ctx)
}

// NSpawnLifecycleTasks installs the helper before units can reference it,
// including during the existing host-root migration.
func NSpawnLifecycleTasks(log *slog.Logger, rf *goalstates.RootFS) []phases.Task {
	return []phases.Task{rootfs.EnsureNSpawnLifecycleHelperFor(rf), rootfs.EnsureNSpawnConfig(log, rf)}
}

func (nspawnProvider) Reset(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log, nodestop.StopNode(log, goalstates.NSpawnMachineKube1), nodestop.StopNode(log, goalstates.NSpawnMachineKube2)),
		phases.Parallel(log, reset.CleanupMachine(log, goalstates.NSpawnMachineKube1), reset.CleanupMachine(log, goalstates.NSpawnMachineKube2)),
	)
}
func (nspawnProvider) Layout() Layout {
	return Layout{Mode: isolation.ModeNSpawn, BaseDir: nspawn.MachinesDir, KubeletUnit: "kubelet.service", HelperPath: "/usr/local/bin/aks-flex-node", Targets: []string{"machines.target"}}
}
