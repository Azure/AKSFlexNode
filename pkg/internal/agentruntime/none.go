package agentruntime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

type noneRuntime struct {
	newNode func(noneops.Params) *noneops.Node
}

func (r noneRuntime) Worker(log *slog.Logger, input Node) (WorkerContext, error) {
	node := r.newNode(noneops.Params{Log: log, Config: input.Config, GoalState: input.GoalState})
	gs, err := node.GoalState()
	if err != nil {
		return WorkerContext{}, err
	}
	root, ok := gs.RootFS.Root().(none.Root)
	if !ok {
		return WorkerContext{}, fmt.Errorf("SDK returned an unexpected host-native root")
	}
	version, err := root.Layout.Current()
	if err != nil {
		return WorkerContext{}, err
	}
	if version == "" {
		return WorkerContext{}, fmt.Errorf("host-native worker has no active version")
	}
	// Download overrides affect SDK version names but are not persisted; the
	// current link is authoritative for the installed worker's filesystem.
	root.Version = version
	return WorkerContext{
		Root: root,
		Exec: func(ctx context.Context, args ...string) (string, error) {
			return gs.NodeStart.Exec(ctx, log, args...)
		},
		KubeletUnit: none.UnitKubelet,
		Mode:        isolation.ModeNone,
	}, nil
}

func (r noneRuntime) Start(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("start-none-worker", func(ctx context.Context) error {
		if input.Previous == nil {
			if err := none.NewDefault(log).Preflight(); err != nil {
				return fmt.Errorf("preflight host-native worker: %w", err)
			}
		}
		if err := rootfs.DownloadContainerImageArchives(log, input.Archives).Do(ctx); err != nil {
			return fmt.Errorf("stage container image archives: %w", err)
		}

		params := noneops.Params{Log: log, Config: input.Node.Config, GoalState: input.Node.GoalState}
		if input.Previous != nil {
			params.Previous = input.Previous.Config
		}
		node := r.newNode(params)
		if input.Previous == nil {
			if err := node.Provision(ctx); err != nil {
				return fmt.Errorf("provision host-native worker: %w", err)
			}
		} else if err := node.Upgrade(ctx); err != nil {
			return fmt.Errorf("upgrade host-native worker: %w", err)
		}

		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		if err := npd.Download(log, input.Config, gs.RootFS.Root()).Do(ctx); err != nil {
			return err
		}
		if input.Previous == nil {
			containerdChanged, kubeletChanged, err := node.Configure(ctx)
			if err != nil {
				return fmt.Errorf("configure host-native worker: %w", err)
			}
			if err := node.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
				return fmt.Errorf("start host-native worker: %w", err)
			}
		}
		return npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx)
	})
}

func (r noneRuntime) Restart(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("restart-none-worker", func(ctx context.Context) error {
		node := r.newNode(noneops.Params{Log: log, Config: input.Node.Config})
		if err := node.Restart(ctx); err != nil {
			return fmt.Errorf("restart host-native worker: %w", err)
		}
		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		return npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx)
	})
}

func (r noneRuntime) Prepare(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("recover-none-upgrade", func(ctx context.Context) error {
		applied := input.Node
		if input.Previous != nil {
			applied = *input.Previous
		}
		node := r.newNode(noneops.Params{Log: log, Config: applied.Config})
		if err := node.RecoverUpgrade(ctx); err != nil {
			return fmt.Errorf("recover host-native worker upgrade: %w", err)
		}
		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		return npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx)
	})
}

func (r noneRuntime) Retire(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("commit-none-upgrade", func(context.Context) error {
		node := r.newNode(noneops.Params{Log: log, Config: input.Node.Config})
		if err := node.Commit(); err != nil {
			log.Warn("SDK upgrade commit will be retried at startup", "error", err)
			return nil
		}
		if err := node.PruneVersions(); err != nil {
			log.Warn("could not prune superseded node versions", "error", err)
		}
		return nil
	})
}
