package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// AKS Flex Node keeps its own host-side files under the host root, /opt/unbounded
// resolved through symlinks; see the hostroot package. Releases before the host
// root installed them under /usr/local. On a host installed by one of those, the
// first command that changes the host links /opt/unbounded to /usr/local, and
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

// InstallHostBinary copies the running binary to <host root>/bin/aks-flex-node
// when no usable binary is there, as on a fresh host where an install script put
// it in /usr/local/bin, or one an earlier release installed directly there
// without the blue/green layout. That copy seeds the layout.
func InstallHostBinary(ctx context.Context, log *slog.Logger) error {
	return installHostBinary(
		log,
		agentUpgradePathsUnder(hostroot.Resolve()).BinaryPath,
		func() error { return PrepareHostRoot(ctx, log) },
		runningAgentExecutable,
	)
}

func installHostBinary(log *slog.Logger, target string, prepare func() error, executable func() (string, error)) error {
	if utilio.IsExecutable(target) {
		return nil
	}

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

	return nil
}

// RemoveLegacySeed removes the binary install scripts left in /usr/local/bin on
// a host installed under a real host root, where nothing runs it. Only a regular
// file is removed: a link there is an earlier release's compatibility link, or
// an operator's.
func RemoveLegacySeed(log *slog.Logger) error {
	return removeLegacySeed(log, hostroot.Path, legacySeedPath)
}

func removeLegacySeed(log *slog.Logger, root, seed string) error {
	if info, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect %s: %w", root, err)
	} else if !info.IsDir() {
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

	log.Info("removing the agent binary an install script left for earlier releases", "path", seed)

	if err := os.Remove(seed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", seed, err)
	}

	return nil
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
