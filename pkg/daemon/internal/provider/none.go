package provider

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

type noneProvider struct {
	newNode       func(noneops.Params) *noneops.Node
	stageArchives func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error
}

func newNoneProvider() noneProvider {
	return noneProvider{newNode: noneops.New, stageArchives: func(ctx context.Context, log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) error {
		return rootfs.DownloadContainerImageArchives(log, staging).Do(ctx)
	}}
}
func (p noneProvider) node(log *slog.Logger, node Node, previous Node) *noneops.Node {
	return p.newNode(noneops.Params{Log: log, Config: node.Config, GoalState: node.GoalState, Previous: previous.Config})
}
func (p noneProvider) GoalState(log *slog.Logger, node Node) (*goalstates.MachineGoalState, error) {
	return p.node(log, node, Node{}).GoalState()
}
func (p noneProvider) Start(ctx context.Context, log *slog.Logger, params StartParams) (bool, error) {
	node := p.node(log, params.Node, Node{})
	running, err := node.Running(ctx)
	if err != nil {
		return false, err
	}
	if !running {
		if err := none.NewDefault(log).Preflight(); err != nil {
			return false, err
		}
		if err := p.stageArchives(ctx, log, params.Archives); err != nil {
			return false, err
		}
		if err := node.Provision(ctx); err != nil {
			return false, err
		}
	}
	gs, err := node.GoalState()
	if err != nil {
		return false, err
	}
	if err := npd.Download(log, params.Config, gs.RootFS.Root()).Do(ctx); err != nil {
		return false, err
	}
	cd, kl, err := node.Configure(ctx)
	if err != nil {
		return false, err
	}
	if err := node.Start(ctx, running, cd, kl); err != nil {
		return false, err
	}
	if err := npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx); err != nil {
		return false, err
	}
	return !running, nil
}
func (p noneProvider) Restart(ctx context.Context, log *slog.Logger, params StartParams) error {
	node := p.node(log, params.Node, Node{})
	if err := node.Restart(ctx); err != nil {
		return err
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	return npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx)
}
func (p noneProvider) Recover(ctx context.Context, log *slog.Logger, params StartParams) error {
	node := p.node(log, params.Node, Node{})
	if err := node.RecoverUpgrade(ctx); err != nil {
		return err
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	return npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx)
}
func (p noneProvider) Upgrade(ctx context.Context, log *slog.Logger, params UpgradeParams) error {
	if err := p.stageArchives(ctx, log, params.Archives); err != nil {
		return err
	}
	node := p.node(log, params.Node, params.Previous)
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	if err := npd.Stop(log, gs.RootFS.Root(), gs.NodeStart).Do(ctx); err != nil {
		return err
	}
	if err := node.Upgrade(ctx); err != nil {
		previous, resolveErr := p.GoalState(log, params.Previous)
		if resolveErr != nil {
			return errors.Join(err, resolveErr)
		}
		return errors.Join(err, npd.Start(log, params.Config, previous.RootFS.Root(), previous.NodeStart, isolation.ModeNone).Do(ctx))
	}
	if err := npd.Download(log, params.Config, gs.RootFS.Root()).Do(ctx); err != nil {
		return err
	}
	return npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx)
}
func (p noneProvider) Retire(ctx context.Context, log *slog.Logger, params UpgradeParams) error {
	node := p.node(log, params.Node, params.Previous)
	if err := node.Commit(); err != nil {
		log.Warn("SDK upgrade commit will be retried at startup", "error", err)
	}
	if err := node.PruneVersions(); err != nil {
		log.Warn("could not prune superseded node versions", "error", err)
	}
	return nil
}
func (noneProvider) RewriteUnits(ctx context.Context, log *slog.Logger, node Node) error { return nil }
func (noneProvider) Reset(log *slog.Logger) phases.Task {
	return &noneResetTask{log: log, teardown: &noneops.Teardown{Log: log}}
}

type noneResetTask struct {
	log      *slog.Logger
	teardown *noneops.Teardown
}

func (*noneResetTask) Name() string { return "reset-none-node" }
func (t *noneResetTask) Do(ctx context.Context) error {
	if err := npd.RemoveHost(t.log, none.Root{}).Do(ctx); err != nil {
		return err
	}
	if err := phases.Serial(t.log, t.teardown.Stop(), t.teardown.RemoveConfig(), t.teardown.Destroy()).Do(ctx); err != nil {
		return err
	}
	return hostnode.ResetCNIState("")
}
func (noneProvider) Layout() Layout {
	return Layout{Mode: isolation.ModeNone, BaseDir: "/", FixedSlot: noneops.Slot, KubeletUnit: none.UnitKubelet}
}
