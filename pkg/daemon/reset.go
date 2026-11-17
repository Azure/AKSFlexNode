package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetNode returns the task that removes the node runtime from the host.
//
// Helpers the agent library installs under the host root, the nspawn lifecycle
// helper and the LocalDNS network helper, are removed under the resolved host
// root and under the legacy root, so reset does not depend on the host root
// having been migrated. The managed agent binaries are intentionally kept.
func ResetNode(ctx context.Context, log *slog.Logger) (phases.Task, error) {
	store, err := NewFileStateStore()
	if err != nil {
		return nil, err
	}
	state, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	mode := isolation.ModeNSpawn
	if state != nil {
		mode = state.Isolation
	} else if noneops.Present() {
		mode = isolation.ModeNone
	}
	return resetNodeFor(mode, log)
}

func resetNodeFor(mode isolation.Mode, log *slog.Logger) (phases.Task, error) {
	runtime, err := provider.New(mode)
	if err != nil {
		return nil, fmt.Errorf("select reset runtime: %w", err)
	}
	var tasks []phases.Task
	if mode == isolation.ModeNone {
		tasks = append(tasks, &clearHostHubTask{log: log})
	}
	tasks = append(tasks, runtime.Reset(log),
		phases.Parallel(log,
			// CleanupNetwork also removes the LocalDNS unit, nft table, dummy
			// interface and helper, which reset previously left behind.
			reset.CleanupNetwork(log),
			reset.RemoveWireGuardKeys(log),
			cleanupLegacyBridgeCNI(log),
		),
		removeHostHelpers(log),
		reset.ReloadSystemd(log),
		config.RemoveRuntimeDirs(log),
	)
	return phases.Serial(log, tasks...), nil
}

// hostHelperPaths returns every location the agent library may have installed
// its host helpers to. CleanupNetwork removes the LocalDNS helper under the
// resolved root only.
func hostHelperPaths() []string {
	return hostHelperPathsFor(goalstates.ResolveHostPaths(), goalstates.LegacyHostPaths())
}

func hostHelperPathsFor(resolved, legacy goalstates.HostPaths) []string {
	paths := []string{resolved.NSpawnLifecycleBinary}
	if legacy.Root != resolved.Root {
		paths = append(paths, legacy.NSpawnLifecycleBinary, legacy.LocalDNSNetworkHelper)
	}

	return paths
}

type removeFilesTask struct {
	name  string
	log   *slog.Logger
	paths []string
}

// removeHostHelpers removes the nspawn lifecycle helper, and the LocalDNS
// helper under the legacy root. The machines and units that invoke them are
// gone by the time this runs, and the next start installs them again.
func removeHostHelpers(log *slog.Logger) phases.Task {
	return &removeFilesTask{
		name:  "remove-host-helpers",
		log:   log,
		paths: hostHelperPaths(),
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
