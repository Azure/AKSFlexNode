package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
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
	own := agentdeployment.UpgradePathsUnder("")
	library := goalstates.LegacyHostPaths()

	files := []string{
		own.BinaryPath,
		own.BluePath,
		own.GreenPath,
		own.CurrentPath,
		own.LastGoodPath,
		agentdeployment.RecoveryScriptPathUnder(""),
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
	return []string{agentdeployment.ManagedBinaryDir}
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
func reconcileHostRootUnderLock(ctx context.Context, log *slog.Logger, cfg *config.Config, state stateStore, upgrades *agentdeployment.HostUpgrade) bool {
	lock, err := upgrades.Acquire()
	if err != nil {
		log.Warn("not moving the agent's files to the host root: an activation holds the lock", "error", err)

		return false
	}
	defer func() { _ = lock.Close() }()

	paths := agentdeployment.DefaultUpgradePaths()
	restarted, err := hostroot.ReconcileMove(ctx, log, hostroot.MoveOptions{
		Files: hostLayout(),
		Dirs:  hostLayoutDirs(),
		// The directories a fresh installation's PrepareHostRoot creates.
		Subdirs:      []string{"bin", agentdeployment.ManagedBinaryDir, "libexec"},
		Record:       hostRootAgentsPath,
		SignalPath:   paths.SignalPath,
		CurrentPath:  paths.CurrentPath,
		LastGoodPath: paths.LastGoodPath,
		RewriteUnits: func(ctx context.Context) error {
			return rewriteHostRootUnits(ctx, log, cfg, state)
		},
		Verify: agentdeployment.VerifyMovedAgent,
		// Restart resets the agent unit's start limit first, which the move's
		// restart needs most; see agentdeployment.HostUpgrade.Restart.
		Restart: upgrades.Restart,
	})
	if err != nil {
		log.Warn("could not move the agent's files to the host root; the next daemon start retries", "error", err)
	}

	return restarted
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
func holdForHostRoot(restarted, unfinished bool, startup, current agentdeployment.UpgradePaths) string {
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
	if err := agentdeployment.EnsureAgentUpgradeServiceAssets(ctx, log, cfg); err != nil {
		return err
	}

	active, err := activeMachineFromStore(ctx, state)
	if err != nil {
		return err
	}
	if active.State.Isolation == isolation.ModeNone {
		if err := utilexec.ReloadSystemd(ctx, log); err != nil {
			return fmt.Errorf("reload systemd after moving to the host root: %w", err)
		}
		return agentdeployment.SyncDirs(agentdeployment.SystemdSystemDir, filepath.Dir(agentdeployment.RecoveryScriptPathUnder(hostroot.Resolve())))
	}

	machineCfg := cfg.DeepCopy()
	if active.State.AppliedKubernetesVersion != "" {
		machineCfg.Components.Kubernetes = active.State.AppliedKubernetesVersion
	}
	agentCfg := config.ToAgentConfig(machineCfg, active.Name)

	rootFS, err := agentruntime.RewriteHostRoot(ctx, log, agentCfg, active.Name)
	if err != nil {
		return err
	}

	if err := utilexec.ReloadSystemd(ctx, log); err != nil {
		return fmt.Errorf("reload systemd after moving to the host root: %w", err)
	}

	// The writers fsync each file before renaming it into place; the renames
	// need their directories synced before the legacy files go.
	return agentdeployment.SyncDirs(
		agentdeployment.SystemdSystemDir,
		filepath.Dir(rootFS.ServiceOverrideFile),
		filepath.Dir(agentdeployment.RecoveryScriptPathUnder(hostroot.Resolve())),
		filepath.Dir(goalstates.ResolveHostPaths().NSpawnLifecycleBinary),
	)
}
