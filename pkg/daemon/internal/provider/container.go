package provider

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

type containerProvider struct{}

func containerNode(log *slog.Logger, node Node) *containerops.Node {
	return containerops.New(containerops.Params{Log: log, Config: node.Config, Slot: node.Slot, GoalState: node.GoalState,
		Overrides: containerops.Overrides{HelperPath: filepath.Join(hostroot.Path, "bin/aks-flex-node")},
	})
}
func (containerProvider) Layout() Layout {
	return Layout{Mode: isolation.ModeContainer, BaseDir: container.DefaultBase, KubeletUnit: "kubelet.service", HelperPath: "/usr/local/bin/aks-flex-node"}
}
func (containerProvider) GoalState(log *slog.Logger, node Node) (*goalstates.MachineGoalState, error) {
	return containerNode(log, node).GoalState()
}
func containerArtifacts(ctx context.Context, log *slog.Logger, cfg *config.Config, gs *goalstates.MachineGoalState) error {
	return phases.Parallel(log, InstallBinary(gs.RootFS.Root()), npd.Download(log, cfg, gs.RootFS.Root())).Do(ctx)
}
func (containerProvider) Start(ctx context.Context, log *slog.Logger, params StartParams) (bool, error) {
	node := containerNode(log, params.Node)
	gs, err := node.GoalState()
	if err != nil {
		return false, err
	}
	running, err := node.Running(ctx)
	if err != nil {
		return false, err
	}
	if !running {
		if err := node.Backend().Preflight(); err != nil {
			return false, err
		}
		if err := rootfs.DownloadContainerImageArchives(log, params.Archives).Do(ctx); err != nil {
			return false, err
		}
		if _, err := node.Prepare(ctx); err != nil {
			return false, err
		}
	}
	if err := containerArtifacts(ctx, log, params.Config, gs); err != nil {
		return false, err
	}
	cd, kl, err := node.Configure(ctx)
	if err != nil {
		return false, err
	}
	if err := node.Start(ctx, running, cd, kl); err != nil {
		return false, err
	}
	if err := npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeContainer).Do(ctx); err != nil {
		return false, err
	}
	return !running, nil
}
func (containerProvider) Restart(ctx context.Context, log *slog.Logger, params StartParams) error {
	node := containerNode(log, params.Node)
	if err := node.Restart(ctx); err != nil {
		return err
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	return npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeContainer).Do(ctx)
}

// bindContainerArtifacts extends the SDK's existing unpack hook: Upgrade owns
// Prepare/Start, so kubelet's credential helper must be installed before it starts.
func bindContainerArtifacts(node *containerops.Node, log *slog.Logger, cfg *config.Config) (*goalstates.MachineGoalState, error) {
	gs, err := node.GoalState()
	if err != nil {
		return nil, err
	}
	backend := node.Backend()
	unpack := backend.Unpack
	backend.Unpack = func(ctx context.Context, spec isolation.NodeSpec, dir string) error {
		if err := unpack(ctx, spec, dir); err != nil {
			return err
		}
		return containerArtifacts(ctx, log, cfg, gs)
	}
	return gs, nil
}
func (containerProvider) Upgrade(ctx context.Context, log *slog.Logger, params UpgradeParams) error {
	if err := rootfs.DownloadContainerImageArchives(log, params.Archives).Do(ctx); err != nil {
		return err
	}
	node := containerNode(log, params.Node)
	gs, err := bindContainerArtifacts(node, log, params.Config)
	if err != nil {
		return err
	}
	if err := node.Upgrade(ctx, containerNode(log, params.Previous)); err != nil {
		return err
	}
	return npd.Start(log, params.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeContainer).Do(ctx)
}
func (containerProvider) Recover(ctx context.Context, log *slog.Logger, params StartParams) error {
	// The pinned OCI SDK reconciles units but has no durable RecoverUpgrade.
	return containerNode(log, params.Node).RewriteUnit(ctx)
}
func (containerProvider) Retire(ctx context.Context, log *slog.Logger, params UpgradeParams) error {
	if err := containerNode(log, params.Previous).Destroy(ctx); err != nil {
		log.Warn("could not remove superseded container slot; next repave retries", "error", err)
	}
	return nil
}
func (containerProvider) RewriteUnits(ctx context.Context, log *slog.Logger, node Node) error {
	return containerNode(log, node).RewriteUnit(ctx)
}
func (containerProvider) Reset(log *slog.Logger) phases.Task {
	teardown := &containerops.Teardown{Log: log}
	return phases.Serial(log, teardown.Stop(), teardown.RemoveConfig(), teardown.Destroy())
}
