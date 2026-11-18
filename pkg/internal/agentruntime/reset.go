package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

type resetWorker interface {
	Stop() phases.Task
	Destroy() phases.Task
}

type discoveredWorker interface {
	resetWorker
	Present() bool
}

// ResetPlan captures runtime ownership before cleanup removes its evidence.
// The daemon runs Stop, cleans host components, then runs Finish.
type ResetPlan interface {
	// HasHostNativeState includes recovery artifacts without treating them as
	// ownership of a native runtime. Components apply their own ownership checks.
	HasHostNativeState() bool
	// Stop stops every discovered worker before any teardown runs.
	Stop() phases.Task
	// Finish destroys the discovered workers, then cleans shared host resources.
	Finish() phases.Task
}

type resetPlan struct {
	log       *slog.Logger
	nspawn    resetWorker
	none      resetWorker
	container resetWorker
	shared    phases.Task
	artifacts phases.Task
	hostState bool
}

func discoverReset(log *slog.Logger, nspawn resetWorker, native, container discoveredWorker, shared, artifacts phases.Task, hostArtifacts bool) *resetPlan {
	plan := &resetPlan{log: log, nspawn: nspawn, shared: shared, artifacts: artifacts, hostState: hostArtifacts}
	if native.Present() {
		plan.none = native
		plan.hostState = true
	}
	if container.Present() {
		plan.container = container
	}
	return plan
}

// PlanReset discovers all installed runtimes independently of applied state.
func (NSpawn) PlanReset(log *slog.Logger) (ResetPlan, error) { return planReset(log) }

func (noneRuntime) PlanReset(log *slog.Logger) (ResetPlan, error) { return planReset(log) }

func planReset(log *slog.Logger) (ResetPlan, error) {
	hostArtifacts, err := NoneUpgradeArtifactsPresent()
	if err != nil {
		return nil, err
	}
	oci := &ociTeardown{sdk: &containerops.Teardown{Log: log}}
	native := &noneTeardown{
		log: log,
		sdk: &noneops.Teardown{Log: log},
	}
	artifacts := phases.Serial(log, cleanupNoneUpgradeArtifacts(""), CleanupContainerUpgradeArtifact())
	return discoverReset(log, nspawnTeardown{log: log}, native, oci, sharedReset(log), artifacts, hostArtifacts), nil
}

func sharedReset(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log,
			reset.CleanupNetwork(log),
			reset.RemoveWireGuardKeys(log),
			cleanupLegacyBridgeCNI(log),
		),
		removeHostHelpers(log),
		reset.ReloadSystemd(log),
	)
}

func (p *resetPlan) HasHostNativeState() bool { return p.hostState }

func (p *resetPlan) Stop() phases.Task {
	tasks := []phases.Task{p.nspawn.Stop()}
	if p.none != nil {
		tasks = append(tasks, p.none.Stop())
	}
	if p.container != nil {
		tasks = append(tasks, p.container.Stop())
	}
	return phases.Serial(p.log, tasks...)
}

func (p *resetPlan) Finish() phases.Task {
	tasks := []phases.Task{p.nspawn.Destroy()}
	if p.none != nil {
		tasks = append(tasks, p.none.Destroy())
	}
	if p.container != nil {
		tasks = append(tasks, p.container.Destroy())
	}
	// Orphan snapshots may belong to a different mode than the installed worker.
	tasks = append(tasks, p.artifacts, p.shared)
	return phases.Serial(p.log, tasks...)
}

type nspawnTeardown struct{ log *slog.Logger }

func (n nspawnTeardown) Stop() phases.Task {
	return phases.Parallel(n.log,
		nodestop.StopNode(n.log, goalstates.NSpawnMachineKube1),
		nodestop.StopNode(n.log, goalstates.NSpawnMachineKube2),
	)
}

func (n nspawnTeardown) Destroy() phases.Task {
	return phases.Parallel(n.log,
		reset.CleanupMachine(n.log, goalstates.NSpawnMachineKube1),
		reset.CleanupMachine(n.log, goalstates.NSpawnMachineKube2),
	)
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

// On a read-only legacy root, unlinking an absent file can return EROFS.
func removeIfPresent(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
