package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// NSpawn executes worker actions using resolved machine configuration.
type NSpawn struct{}

func (NSpawn) Worker(log *slog.Logger, node Node) (WorkerContext, error) {
	return WorkerContext{
		Root: nspawn.Root(node.Slot),
		Exec: func(ctx context.Context, args ...string) (string, error) {
			return nspawn.Exec(ctx, log, node.Slot, args...)
		},
		KubeletUnit: "kubelet.service",
		Mode:        isolation.ModeNSpawn,
	}, nil
}

func (NSpawn) Start(log *slog.Logger, input Inputs) phases.Task {
	gs := input.Node.GoalState
	cfg := input.Config
	return phases.Serial(log,
		stageContainerImageArchiveBindSource(log, input.Archives),
		rootfs.Provision(log, gs.RootFS),
		configureNSpawnDependencies(log, gs.RootFS, cfg.Bootstrap.AdditionalRequiredServices),
		phases.Parallel(log,
			npd.Download(log, cfg, gs.RootFS.Root()),
			installBinary(gs.RootFS.Root()),
		),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, input.Node.Slot),
		npd.Start(log, cfg, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
	)
}

func (NSpawn) Restart(log *slog.Logger, input Inputs) phases.Task {
	gs := input.Node.GoalState
	return phases.Serial(log,
		stageContainerImageArchiveBindSource(log, input.Archives),
		nodestop.StopNode(log, input.Node.Slot),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, input.Node.Slot),
		npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
	)
}

func (NSpawn) PrepareUpgrade(log *slog.Logger, input Inputs) phases.Task {
	return phases.Serial(log,
		nodestop.StopNode(log, input.Previous.Slot),
		reset.CleanupNetwork(log),
	)
}

func (NSpawn) Recover(log *slog.Logger, _ Inputs) phases.Task {
	return phases.Serial(log)
}

func (NSpawn) Retire(log *slog.Logger, input Inputs) phases.Task {
	return reset.CleanupMachine(log, input.Previous.Slot)
}

func stageContainerImageArchiveBindSource(log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) phases.Task {
	// Unbounded's nspawn template unconditionally binds this host path.
	return rootfs.DownloadContainerImageArchives(log, staging)
}

// Install the helper before rewriting units that may name it; an old helper
// can regenerate paths under the legacy root that the move will remove.
func nspawnLifecycleTasks(log *slog.Logger, rootFS *goalstates.RootFS) []phases.Task {
	return []phases.Task{
		rootfs.EnsureNSpawnLifecycleHelperFor(rootFS),
		rootfs.EnsureNSpawnConfig(log, rootFS),
	}
}

// RewriteHostRoot updates worker assets after a host-root move without starting the network unit.
func RewriteHostRoot(ctx context.Context, log *slog.Logger, cfg *agentconfig.AgentConfig, machine string) (*goalstates.RootFS, error) {
	rootFS, err := goalstates.ResolveNSpawnConfig(cfg, machine)
	if err != nil {
		return nil, fmt.Errorf("resolve machine nspawn config: %w", err)
	}
	if err := phases.Serial(log, nspawnLifecycleTasks(log, rootFS)...).Do(ctx); err != nil {
		return nil, err
	}

	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); err == nil {
		gs, err := goalstates.ResolveMachine(log, cfg, machine, nil)
		if err != nil {
			return nil, fmt.Errorf("resolve machine goal state: %w", err)
		}
		if err := nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return rootFS, nil
}
