package daemon

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetNode returns the task that removes the node runtime from the host.
//
// The prefix is the host install prefix. Helpers the agent library installs
// under it, the nspawn lifecycle helper and the LocalDNS network helper, are
// removed under that prefix and under the default, so a host that changed
// prefix is also cleaned up. The managed agent binaries are intentionally kept.
func ResetNode(log *slog.Logger, prefix string) phases.Task {
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
			// CleanupNetwork also removes the LocalDNS unit, nft table, dummy
			// interface and helper, which reset previously left behind.
			reset.CleanupNetwork(log, prefix),
			reset.RemoveWireGuardKeys(log),
			cleanupLegacyBridgeCNI(log),
		),
		removeNSpawnLifecycleHelpers(log, prefix),
		reset.ReloadSystemd(log),
		config.RemoveRuntimeDirs(log),
	)
}

// nspawnLifecycleHelperPaths returns every location the nspawn lifecycle helper
// may have been installed to for a prefix.
func nspawnLifecycleHelperPaths(prefix string) []string {
	var paths []string
	for _, candidate := range goalstates.MergeHostPrefixes(prefix) {
		paths = append(paths, goalstates.ResolveHostPaths(candidate).NSpawnLifecycleBinary)
	}

	return paths
}

type removeFilesTask struct {
	name  string
	log   *slog.Logger
	paths []string
}

// removeNSpawnLifecycleHelpers removes the nspawn lifecycle helper. The machines
// that invoke it are gone by the time this runs, and the next start installs it
// again.
func removeNSpawnLifecycleHelpers(log *slog.Logger, prefix string) phases.Task {
	return &removeFilesTask{
		name:  "remove-nspawn-lifecycle-helper",
		log:   log,
		paths: nspawnLifecycleHelperPaths(prefix),
	}
}

func (t *removeFilesTask) Name() string { return t.name }

func (t *removeFilesTask) Do(context.Context) error {
	for _, path := range t.paths {
		if err := removeIfPresent(path); err != nil {
			return err
		}
	}

	return nil
}
