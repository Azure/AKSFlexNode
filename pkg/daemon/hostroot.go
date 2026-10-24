package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// AKS Flex Node keeps its own host-side files under the host root, hostroot.Path
// (/opt/unbounded/agent) resolved through symlinks; see the hostroot package. Its
// parent, /opt/unbounded, belongs to the host. Releases before the host root
// installed the files under /usr/local. On a host installed by one of those, the
// first command that changes the host links hostroot.Path to /usr/local, and
// every path is built from the resolved root, so the paths an older release
// wrote into links and units still compare equal to the ones built here.
const (
	binaryName         = "aks-flex-node"
	managedBinaryDir   = "lib/aks-flex-node"
	recoveryScriptName = "aks-flex-node-recovery.sh"
)

// HostRootMarkers returns the files, relative to the host root, whose presence
// under the legacy root identifies an installation by a release before the host
// root. They are the blue/green binary layout only. The plain binary is left
// out: install scripts put it in /usr/local/bin on fresh hosts too, so on its
// own it is not an installation. Neither are the files the agent library
// installs, such as the nspawn lifecycle helper, which an older reset can leave
// behind.
func HostRootMarkers() []string {
	return []string{
		filepath.Join(managedBinaryDir, "aks-flex-node-blue"),
		filepath.Join(managedBinaryDir, "aks-flex-node-green"),
		filepath.Join(managedBinaryDir, "aks-flex-node-current"),
		filepath.Join(managedBinaryDir, "aks-flex-node-last-good"),
	}
}

// MigrateHostRoot links the host root to the legacy root on a host installed by
// a release before the host root. Commands that change the host call it before
// they resolve any path.
func MigrateHostRoot(log *slog.Logger) error {
	return hostroot.Migrate(log, HostRootMarkers()...)
}

// PlannedHostRoot returns the host root this host will resolve once migrated,
// without migrating it. It is for code that must not change the host.
func PlannedHostRoot() string {
	return hostroot.Planned(HostRootMarkers()...)
}

// PrepareHostRoot creates the directories under the host root that the node
// installs into.
func PrepareHostRoot(ctx context.Context, log *slog.Logger) error {
	return hostroot.Prepare(ctx, log, "bin", managedBinaryDir, "libexec")
}

// legacySeedPath is where install scripts put the binary on a host whose
// /usr/local/bin is writable, as earlier releases were installed, so that such a
// release finds itself there.
var legacySeedPath = filepath.Join(hostroot.LegacyPath, "bin", binaryName)

// InstallHostBinary puts the binary under the host root, <host root>/bin/aks-flex-node, which
// the agent's blue/green layout is seeded from:
//
//   - While the agent's unit is installed, as for the daemon, the binary changes only through an
//     activation, so it is copied only when no usable binary is there, as on a host an earlier
//     release installed directly in /usr/local/bin.
//   - Otherwise, as for start before it installs the agent, it is the running binary. One there
//     that isn't, such as the one a reset left, is replaced with a copy of it, and a layout that
//     doesn't run it, such as one a reset by an earlier release kept, is removed, so the layout
//     start seeds runs the binary doing the start.
//
// It fails, and leaves no copy, when the copy cannot run.
func InstallHostBinary(ctx context.Context, log *slog.Logger) error {
	return installHostBinary(
		ctx,
		log,
		agentUpgradePathsUnder(hostroot.Resolve()),
		ServiceUnitPath,
		agentUpgradeLockPath,
		func() error { return PrepareHostRoot(ctx, log) },
		runningAgentExecutable,
		agentbinary.Verify,
	)
}

func installHostBinary(
	ctx context.Context,
	log *slog.Logger,
	paths agentUpgradePaths,
	unitPath, lockPath string,
	prepare func() error,
	executable func() (string, error),
	verify func(context.Context, string) error,
) error {
	target := paths.BinaryPath
	if _, err := os.Lstat(unitPath); err == nil {
		if utilio.IsExecutable(target) {
			return nil
		}
		return copyHostBinary(ctx, log, target, prepare, executable, verify)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check for the agent's unit: %w", err)
	}

	// Not beside an activation's switch.
	lock, err := agentbinary.AcquireHostActivationLock(lockPath)
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // releasing the lock

	self, err := executable()
	if err != nil {
		return err
	}
	running, err := os.Stat(self)
	if err != nil {
		return fmt.Errorf("inspect the running binary: %w", err)
	}
	// Copied before a layout goes, as the running binary can be one of its slots.
	if installed, err := os.Stat(target); err != nil || !os.SameFile(installed, running) || !utilio.IsExecutable(target) {
		if err := copyHostBinary(ctx, log, target, prepare, func() (string, error) { return self, nil }, verify); err != nil {
			return err
		}
	}
	return removeStaleLayout(log, paths, running)
}

// removeStaleLayout removes the agent's blue/green layout when its current binary isn't the
// running one: seeded again, it would point the binary under the host root back at it.
func removeStaleLayout(log *slog.Logger, paths agentUpgradePaths, running os.FileInfo) error {
	layoutDir := filepath.Dir(paths.CurrentPath)
	if _, err := os.Lstat(layoutDir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect %s: %w", layoutDir, err)
	}
	if current, err := os.Stat(paths.CurrentPath); err == nil && os.SameFile(current, running) {
		return nil
	}
	log.Info("removing the agent's blue/green layout, which doesn't run this binary", "path", layoutDir)
	if err := os.RemoveAll(layoutDir); err != nil {
		return fmt.Errorf("remove %s: %w", layoutDir, err)
	}
	return nil
}

// copyHostBinary copies the running binary to target, and keeps it only if it runs.
func copyHostBinary(
	ctx context.Context,
	log *slog.Logger,
	target string,
	prepare func() error,
	executable func() (string, error),
	verify func(context.Context, string) error,
) error {
	if err := prepare(); err != nil {
		return err
	}

	self, err := executable()
	if err != nil {
		return err
	}

	log.Info("installing the agent binary under the host root", "path", target, "from", self)

	if err := copyExecutable(self, target); err != nil {
		return fmt.Errorf("install the agent binary at %s: %w", target, err)
	}

	// The agent unit is Type=simple, so systemctl start reports success
	// whether or not the binary can run. Where it cannot, such as under /opt
	// mounted noexec, this is the last point that can say so. The copy goes,
	// so the next attempt makes a new one rather than seeding the layout from
	// it.
	if err := verify(ctx, target); err != nil {
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove %s: %w", target, removeErr))
		}

		return fmt.Errorf("the agent cannot run from %s; the filesystem that holds it must allow running programs, so it must not be mounted noexec: %w",
			filepath.Dir(target), err)
	}

	return nil
}

// RemoveLegacySeed removes the binary install scripts left in /usr/local/bin
// once nothing the agent runs is under /usr/local: the host is installed under
// a real host root, or one an operator linked somewhere else. Only a regular
// file is removed: a link there is an earlier release's compatibility link, or
// an operator's.
//
// Not partway through a move from /usr/local: until the move has restarted the
// daemon from the host root, the daemon may still be running from the files
// under /usr/local, and the move removes none of them until then.
//
// It is for start, which runs from that binary on a host whose /usr/local/bin is
// writable, and removes it as its last step, once it has copied itself
// everywhere it is needed and is about to exit. The daemon keeps it while it
// runs from it; see removeDaemonLegacySeed.
func RemoveLegacySeed(log *slog.Logger) error {
	return removeLegacySeed(log, hostroot.LegacyReleased, legacySeedPath, nil)
}

// removeDaemonLegacySeed is RemoveLegacySeed for the daemon, which keeps the
// binary while it runs from it.
//
// On a host a release before the blue/green layout installed directly in
// /usr/local/bin, the unit runs the daemon from that binary. Once this release
// has copied it under the host root and rewritten the units, it is no longer
// needed, but the running daemon's os.Executable still names it, and nothing
// restarts the daemon. Removed, every later copy of the running binary, such as
// the one a repave installs in the machine, fails until the daemon restarts. The
// next start runs from the host root, and removes it then.
func removeDaemonLegacySeed(log *slog.Logger) error {
	return removeLegacySeed(log, hostroot.LegacyReleased, legacySeedPath, runningFrom)
}

// removeLegacySeed removes seed as RemoveLegacySeed describes. When running is
// set, a seed it reports this process runs from is kept.
func removeLegacySeed(log *slog.Logger, released func() (bool, error), seed string, running func(os.FileInfo) (bool, error)) error {
	if done, err := released(); err != nil {
		return fmt.Errorf("inspect the host root: %w", err)
	} else if !done {
		return nil
	}

	info, err := os.Lstat(seed)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("inspect %s: %w", seed, err)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	if running != nil {
		self, err := running(info)
		if err != nil {
			return err
		}

		if self {
			log.Info("keeping the agent binary an install script left for earlier releases while the agent runs from it; "+
				"the next start removes it", "path", seed)

			return nil
		}
	}

	log.Info("removing the agent binary an install script left for earlier releases", "path", seed)

	if err := os.Remove(seed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", seed, err)
	}

	return nil
}

// runningFrom reports whether this process runs the file info describes. It
// compares the files themselves, through /proc/self/exe, so it does not depend
// on the path the process was started by.
func runningFrom(info os.FileInfo) (bool, error) {
	self, err := os.Stat("/proc/self/exe")
	if err != nil {
		return false, fmt.Errorf("inspect the running executable: %w", err)
	}

	return os.SameFile(self, info), nil
}

// runningOrServingFrom is runningFrom for direct activation, which runs beside
// the agent service rather than as it: the file is also in use while the
// service's main process runs it. On a host a release before the blue/green
// layout installed, the service runs /usr/local/bin/aks-flex-node until it
// restarts, even once an activation runs from elsewhere. self is runningFrom,
// mainPID returns the service's MainPID, 0 when it has no process, and
// procRoot is /proc.
func runningOrServingFrom(info os.FileInfo, self func(os.FileInfo) (bool, error), mainPID func() (string, error), procRoot string) (bool, error) {
	if running, err := self(info); err != nil || running {
		return running, err
	}

	output, err := mainPID()
	if err != nil {
		return false, fmt.Errorf("find %s's main process: %w", ServiceUnitName, err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid < 0 {
		return false, fmt.Errorf("invalid %s MainPID %q", ServiceUnitName, output)
	}

	if pid == 0 {
		return false, nil
	}

	exe, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid), "exe"))
	if errors.Is(err, os.ErrNotExist) {
		// It has ended since.
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("inspect %s's running executable: %w", ServiceUnitName, err)
	}

	return os.SameFile(exe, info), nil
}

// guardLegacySeed runs before an AgentUpgrade or a direct activation switches
// the agent binary. It removes the binary install scripts left in
// /usr/local/bin, and refuses the switch while something there would still be
// taken for one, on a host that no longer runs anything from /usr/local.
//
// A release before the host root takes an executable at /usr/local/bin/
// aks-flex-node as its own binary when it starts, and builds its blue/green
// layout under /usr/local from it. After an upgrade to such a release, the
// host would then have an installation under both roots, which this release
// refuses to run on, so a rollback to it would not start. With nothing there,
// such a release fails to start and the agent rolls back to last-good, as for
// any upgrade whose daemon cannot start. The release cannot be told from the
// archive before it is staged, so every switch is guarded.
//
// A seed running reports the agent runs from, this process or, for direct
// activation, the agent service, is kept, as removeDaemonLegacySeed explains, so
// the switch is refused until the agent runs from somewhere else.
//
// So is a switch while unfinished reports a move to the host root that has not
// finished: the files it has yet to remove under /usr/local are a whole
// installation, which such a release would run from rather than fail.
func guardLegacySeed(log *slog.Logger, released, unfinished func() (bool, error), seed string, running func(os.FileInfo) (bool, error)) error {
	if moving, err := unfinished(); err != nil {
		return fmt.Errorf("inspect the host root: %w", err)
	} else if moving {
		return fmt.Errorf("the move of the agent's files to %s has not finished, and a release before %s would take the files still under %s as its own installation after an upgrade to it; "+
			"restart %s, which finishes the move, then retry", hostroot.Path, hostroot.Path, hostroot.LegacyPath, ServiceUnitName)
	}

	if err := removeLegacySeed(log, released, seed, running); err != nil {
		return err
	}

	if done, err := released(); err != nil {
		return fmt.Errorf("inspect the host root: %w", err)
	} else if !done {
		return nil
	}

	if !adoptableByEarlierRelease(seed) {
		return nil
	}

	if info, err := os.Lstat(seed); err == nil && info.Mode().IsRegular() && running != nil {
		if self, err := running(info); err == nil && self {
			return fmt.Errorf("%s is the binary the agent runs from, and a release before %s would take it as its own binary after an upgrade to it; "+
				"if it is the agent service's, restart %s so that the agent runs from %s, otherwise run the new binary from another path, then retry",
				seed, hostroot.Path, ServiceUnitName, hostroot.Path)
		}
	}

	return fmt.Errorf("%s leads to an executable, which a release before %s would take as its own binary after an upgrade to it; "+
		"remove it, or link the agent from /usr/local/sbin instead, then retry", seed, hostroot.Path)
}

// adoptableByEarlierRelease reports whether a release before the host root
// would take the file at path as its binary: a regular executable file, links
// followed. That is the check those releases make before seeding their
// layout from it.
func adoptableByEarlierRelease(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// agentUpgradePathsUnder builds the upgrade layout under a resolved host root.
//
// SignalPath is under /etc rather than the root. It is state about an upgrade
// rather than part of the installed layout, and has to survive a rollback to a
// binary that predates the host root.
func agentUpgradePathsUnder(root string) agentUpgradePaths {
	binaryDir := filepath.Join(root, managedBinaryDir)

	return agentUpgradePaths{
		BinaryPath:   filepath.Join(root, "bin", binaryName),
		BluePath:     filepath.Join(binaryDir, "aks-flex-node-blue"),
		GreenPath:    filepath.Join(binaryDir, "aks-flex-node-green"),
		CurrentPath:  filepath.Join(binaryDir, "aks-flex-node-current"),
		LastGoodPath: filepath.Join(binaryDir, "aks-flex-node-last-good"),
		SignalPath:   "/etc/aks-flex-node/agent-upgrade-signal.json",
	}
}

// recoveryScriptPathUnder returns where the recovery script is installed under a
// resolved host root.
func recoveryScriptPathUnder(root string) string {
	return filepath.Join(root, managedBinaryDir, recoveryScriptName)
}
