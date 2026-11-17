package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

func (nspawnProvider) ResetNode(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log,
			nodestop.StopNode(log, goalstates.NSpawnMachineKube1),
			nodestop.StopNode(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.CleanupMachine(log, goalstates.NSpawnMachineKube1),
			reset.CleanupMachine(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			// CleanupNetwork also removes LocalDNS units, rules, interface and helper.
			reset.CleanupNetwork(log),
			reset.RemoveWireGuardKeys(log),
			cleanupLegacyBridgeCNI(log),
		),
		removeHostHelpers(log),
		reset.ReloadSystemd(log),
		config.RemoveRuntimeDirs(log),
	)
}

func (nspawnProvider) RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, state stateStore) error {
	active, err := activeMachineFromStore(ctx, state)
	if err != nil {
		return err
	}
	machineCfg := cfg.DeepCopy()
	if active.State.AppliedKubernetesVersion != "" {
		machineCfg.Components.Kubernetes = active.State.AppliedKubernetesVersion
	}
	agentCfg := config.ToAgentConfig(machineCfg, active.Name)
	rootFS, err := goalstates.ResolveNSpawnConfig(agentCfg, active.Name)
	if err != nil {
		return fmt.Errorf("resolve machine nspawn config: %w", err)
	}
	if err := phases.Serial(log, nspawnLifecycleTasks(log, rootFS)...).Do(ctx); err != nil {
		return err
	}
	// The network is already up; rewrite LocalDNS assets without running them.
	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); err == nil {
		gs, err := goalstates.ResolveMachine(log, agentCfg, active.Name, nil)
		if err != nil {
			return fmt.Errorf("resolve machine goal state: %w", err)
		}
		if err := nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := utilexec.ReloadSystemd(ctx, log); err != nil {
		return fmt.Errorf("reload systemd after moving to the host root: %w", err)
	}
	// Writers fsync files; their renames must be durable before legacy files go.
	return syncDirs(systemdSystemDir, filepath.Dir(rootFS.ServiceOverrideFile), filepath.Dir(recoveryScriptPathUnder(hostroot.Resolve())), filepath.Dir(goalstates.ResolveHostPaths().NSpawnLifecycleBinary))
}

// A legacy hook can run the helper while regenerating its config. Install the
// current helper first so regeneration cannot restore paths under /usr/local.
func nspawnLifecycleTasks(log *slog.Logger, rootFS *goalstates.RootFS) []phases.Task {
	return []phases.Task{rootfs.EnsureNSpawnLifecycleHelperFor(rootFS), rootfs.EnsureNSpawnConfig(log, rootFS)}
}
