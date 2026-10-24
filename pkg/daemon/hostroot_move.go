package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// hostRootAgentsPath records the digest of every binary that has run as the
// daemon on a host linked to the legacy root; see hostroot.ReconcileMove. It is
// under the config directory, which reset removes.
const hostRootAgentsPath = "/etc/aks-flex-node/host-root-agents"

// hostLayout returns every file of the host-side layout, relative to the host
// root: AKS Flex Node's own binaries and recovery script, and the helpers the
// agent library installs there. Moving a linked host copies these from the
// legacy root.
func hostLayout() []string {
	// Built under an empty root, so each path comes out relative to the host
	// root.
	own := agentUpgradePathsUnder("")
	library := goalstates.LegacyHostPaths()

	files := []string{
		own.BinaryPath,
		own.BluePath,
		own.GreenPath,
		own.CurrentPath,
		own.LastGoodPath,
		recoveryScriptPathUnder(""),
	}
	// The library builds its paths only under a root it resolves itself, so
	// its legacy root is trimmed. TestHostLayout checks the result.
	for _, path := range []string{library.NSpawnLifecycleBinary, library.LocalDNSNetworkHelper} {
		files = append(files, strings.TrimPrefix(path, library.Root+string(filepath.Separator)))
	}

	return files
}

// hostLayoutDirs returns the directories, relative to the host root, that hold
// only files of the host-side layout. Earlier releases created them for AKS
// Flex Node alone, so moving a linked host removes them from the legacy root
// once they are empty. bin and libexec are shared, and stay.
func hostLayoutDirs() []string {
	return []string{managedBinaryDir}
}

// hostRootRestartWait bounds how long a daemon that may not take work waits for
// systemd to replace it before it exits; see holdForHostRoot. It is long enough
// that exiting after it, and being restarted RestartSec later, can never reach
// the unit's StartLimitBurst within StartLimitIntervalSec and trip recovery
// into a rollback.
const hostRootRestartWait = 2 * time.Minute

// reconcileHostRootUnderLock moves a host an earlier release installed from
// /usr/local into a real hostroot.Path once neither the current nor the
// last-good binary is from such a release, and finishes a move that was
// interrupted. It reports whether it queued the daemon's restart, which the
// move does from its first pass, before it removes anything under /usr/local;
// the restarted daemon removes the files there.
//
// It holds the activation lock, which keeps an AgentUpgrade or a direct
// activation from changing the layout under it. An AgentReset cannot overlap
// it either: the daemon runs it before it opens the reconcile gate. A local
// reset stops the daemon first, which interrupts a move at worst, and the next
// start finishes it.
//
// A failure is logged, never returned: the daemon is healthy either way, and
// the next start retries; see holdForHostRoot for a failure that leaves the
// layout moved under the running daemon.
func reconcileHostRootUnderLock(ctx context.Context, log *slog.Logger, cfg *config.Config, state stateStore, upgrades *hostAgentUpgradeExecutor) bool {
	lock, err := upgrades.Acquire()
	if err != nil {
		log.Warn("not moving the agent's files to the host root: an activation holds the lock", "error", err)

		return false
	}
	defer func() { _ = lock.Close() }()

	paths := defaultAgentUpgradePaths()
	restarted, err := hostroot.ReconcileMove(ctx, log, hostroot.MoveOptions{
		Files: hostLayout(),
		Dirs:  hostLayoutDirs(),
		// The directories a fresh installation's PrepareHostRoot creates.
		Subdirs:      []string{"bin", managedBinaryDir, "libexec"},
		Record:       hostRootAgentsPath,
		SignalPath:   paths.SignalPath,
		CurrentPath:  paths.CurrentPath,
		LastGoodPath: paths.LastGoodPath,
		RewriteUnits: func(ctx context.Context) error {
			return rewriteHostRootUnits(ctx, log, cfg, state)
		},
		Verify: verifyMovedAgent,
		// Restart resets the agent unit's start limit first, which the move's
		// restart needs most; see hostAgentUpgradeExecutor.Restart.
		Restart: upgrades.Restart,
	})
	if err != nil {
		log.Warn("could not move the agent's files to the host root; the next daemon start retries", "error", err)
	}

	return restarted
}

// verifyMovedAgent runs the current agent binary from a copy of the layout
// under root, before any unit names it. A move whose copy fails this is undone,
// and the host stays linked to /usr/local.
func verifyMovedAgent(ctx context.Context, root string) error {
	return agentbinary.Verify(ctx, agentUpgradePathsUnder(root).CurrentPath)
}

// holdForHostRoot reports why this process may not take work after reconciling
// the host root, or "" if it may. It may not once it has queued its own
// restart from the host root, nor when the host root no longer resolves to
// where it did at startup: the move swapped the copy in and then failed, and
// the AgentUpgrade executor still names the files under /usr/local, which the
// units may no longer run. Nor while a move has not finished: its last step
// failed, and the files under /usr/local it would have removed are what a
// release before the host root would take as its own installation after an
// AgentUpgrade to it. The next start retries that step.
func holdForHostRoot(restarted, unfinished bool, startup, current agentUpgradePaths) string {
	switch {
	case restarted:
		return "the daemon queued its restart from the host root"
	case startup != current:
		return fmt.Sprintf("the host root moved from %s to %s under the running daemon",
			filepath.Dir(startup.CurrentPath), filepath.Dir(current.CurrentPath))
	case unfinished:
		return "the move of the agent's files to the host root has not finished; the next daemon start retries it"
	default:
		return ""
	}
}

// hostRootMovingMarker is the file the agent library keeps in the host root
// until a move has removed the files under the legacy root. The library
// doesn't export it (movingMarker in
// github.com/Azure/unbounded/internal/hostroot); uninstall.sh names it too.
const hostRootMovingMarker = ".moving"

// hostRootMoveUnfinishedHere is hostRootMoveUnfinished for this host's root.
func hostRootMoveUnfinishedHere() (bool, error) {
	return hostRootMoveUnfinished(hostroot.Path)
}

// hostRootMoveUnfinished reports whether a move of the agent's files to root
// has started and not finished: root is a real directory that still holds the
// library's marker. A host linked to the legacy root has none.
func hostRootMoveUnfinished(root string) (bool, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", root, err)
	}
	if !info.IsDir() {
		return false, nil
	}
	if _, err := os.Lstat(filepath.Join(root, hostRootMovingMarker)); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect %s: %w", root, err)
	}
	return true, nil
}

// awaitReplacement keeps a daemon that may not take work idle until systemd
// stops it, which ends ctx, and returns nil then. If that has not happened
// within wait it returns an error, so the daemon exits and Restart= starts it
// from the units as they are now; the next start finishes or retries the move.
func awaitReplacement(ctx context.Context, log *slog.Logger, reason string, wait time.Duration) error {
	log.Info("not taking work until the daemon is restarted", "reason", reason, "timeout", wait)

	select {
	case <-ctx.Done():
		return nil
	case <-time.After(wait):
		return fmt.Errorf("%s, and it was not restarted within %s", reason, wait)
	}
}

// rewriteHostRootUnits points every unit and script that names the host-side
// files at the files under the host root.
func rewriteHostRootUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, state stateStore) error {
	// The agent unit, the recovery unit and the recovery script.
	if err := ensureAgentUpgradeServiceAssets(ctx, log, cfg); err != nil {
		return err
	}

	activeName, agentCfg, rootFS, err := activeMachineConfig(ctx, cfg, state)
	if err != nil {
		return err
	}

	// The nspawn lifecycle helper, the machine's service override and its
	// config regeneration unit.
	if err := phases.Serial(log, nspawnLifecycleTasks(log, rootFS)...).Do(ctx); err != nil {
		return err
	}

	// The LocalDNS network unit and its helper, on a host that has them. The
	// network they configure is already up, so the unit is not run again.
	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); err == nil {
		gs, err := goalstates.ResolveMachine(log, agentCfg, activeName, nil)
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

	// The writers fsync each file before renaming it into place; the renames
	// need their directories synced before the legacy files go.
	return syncDirs(
		systemdSystemDir,
		filepath.Dir(rootFS.ServiceOverrideFile),
		filepath.Dir(recoveryScriptPathUnder(hostroot.Resolve())),
		filepath.Dir(goalstates.ResolveHostPaths().NSpawnLifecycleBinary),
	)
}

// activeMachineConfig returns the active machine's name, its agent config, at the Kubernetes
// version it runs, and its nspawn config, from cfg.
func activeMachineConfig(ctx context.Context, cfg *config.Config, state stateStore) (string, *agentconfig.AgentConfig, *goalstates.RootFS, error) {
	active, err := activeMachineFromStore(ctx, state)
	if err != nil {
		return "", nil, nil, err
	}
	machineCfg := cfg.DeepCopy()
	if active.State.AppliedKubernetesVersion != "" {
		machineCfg.Components.Kubernetes = active.State.AppliedKubernetesVersion
	}
	agentCfg := config.ToAgentConfig(machineCfg, active.Name)
	rootFS, err := goalstates.ResolveNSpawnConfig(agentCfg, active.Name)
	if err != nil {
		return "", nil, nil, fmt.Errorf("resolve machine nspawn config: %w", err)
	}
	return active.Name, agentCfg, rootFS, nil
}

// nspawnLifecycleTasks returns what rewriting the units does for the nspawn
// machine, in order.
//
// This release's config supplies the machine's hooks' commands, which start
// the agent's nspawn lifecycle units, written under the host root by then, so
// the hooks name nothing under it, and unbounded installs no helper for them.
// The helper the earlier release installed, which the move copies, is a link
// to the agent's binary by then too; see ensureNSpawnLifecycleHelperLink.
//
// For hooks that run the helper, it would be installed first, from the running
// binary, before the units name it: the earlier release's copy regenerates the
// units with the helper path that release knows, under /usr/local, which the
// move removes, and the machine would not start again.
func nspawnLifecycleTasks(log *slog.Logger, rootFS *goalstates.RootFS) []phases.Task {
	return []phases.Task{
		rootfs.EnsureNSpawnLifecycleHelperFor(rootFS),
		rootfs.EnsureNSpawnConfig(log, rootFS),
	}
}

// syncDirs makes the entries in each directory durable: the files renamed into
// it or removed from it. Each directory is synced once.
func syncDirs(dirs ...string) error {
	var errs []error
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true

		f, err := os.Open(dir) //nolint:gosec // The agent's own directories.
		if err != nil {
			errs = append(errs, fmt.Errorf("sync %s: %w", dir, err))
			continue
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			errs = append(errs, fmt.Errorf("sync %s: %w", dir, err))
		}
	}

	return errors.Join(errs...)
}
