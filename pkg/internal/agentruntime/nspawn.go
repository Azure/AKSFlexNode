package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// NSpawn executes worker actions using resolved machine configuration.
type NSpawn struct{}

func (NSpawn) Start(cfg *config.Config, log *slog.Logger, machine string, gs *goalstates.MachineGoalState, archives *goalstates.ContainerImageArchiveStaging) phases.Task {
	return phases.Serial(log,
		stageContainerImageArchiveBindSource(log, archives),
		rootfs.Provision(log, gs.RootFS),
		configureNSpawnDependencies(log, gs.RootFS, cfg.Bootstrap.AdditionalRequiredServices),
		phases.Parallel(log,
			npd.Download(log, cfg, gs.RootFS.Root()),
			&installBinaryTask{root: gs.RootFS.Root()},
		),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, machine),
		npd.Start(log, cfg, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
	)
}

func (NSpawn) Restart(cfg *config.Config, log *slog.Logger, machine string, gs *goalstates.MachineGoalState, archives *goalstates.ContainerImageArchiveStaging) phases.Task {
	return phases.Serial(log,
		stageContainerImageArchiveBindSource(log, archives),
		nodestop.StopNode(log, machine),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, machine),
		npd.Start(log, cfg, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
	)
}

func (NSpawn) Stop(log *slog.Logger, machine string) phases.Task {
	return nodestop.StopNode(log, machine)
}

func (NSpawn) CleanupNetwork(log *slog.Logger) phases.Task {
	return reset.CleanupNetwork(log)
}

func (NSpawn) Retire(log *slog.Logger, machine string) phases.Task {
	return reset.CleanupMachine(log, machine)
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
