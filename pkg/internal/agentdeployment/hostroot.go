package agentdeployment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

const (
	// binaryName is the name installed under the host root and the legacy root.
	binaryName = "aks-flex-node"
	// ManagedBinaryDir holds the agent's blue/green binaries and recovery script.
	ManagedBinaryDir = "lib/aks-flex-node"
)

// HostRootMarkers identifies the blue/green binary layout installed by releases
// before the host root. The plain binary and SDK helpers can exist on fresh or
// reset hosts, so they do not establish that an installation exists.
func HostRootMarkers() []string {
	return []string{
		filepath.Join(ManagedBinaryDir, "aks-flex-node-blue"),
		filepath.Join(ManagedBinaryDir, "aks-flex-node-green"),
		filepath.Join(ManagedBinaryDir, "aks-flex-node-current"),
		filepath.Join(ManagedBinaryDir, "aks-flex-node-last-good"),
	}
}

// MigrateHostRoot links the host root to the legacy root for older installs.
// Resolving paths through that link keeps existing unit and binary paths valid.
func MigrateHostRoot(log *slog.Logger) error {
	return hostroot.Migrate(log, HostRootMarkers()...)
}

// PlannedHostRoot reports the root that will be resolved after migration.
func PlannedHostRoot() string {
	return hostroot.Planned(HostRootMarkers()...)
}

// PrepareHostRoot creates the directories used by the agent installation.
func PrepareHostRoot(ctx context.Context, log *slog.Logger) error {
	return hostroot.Prepare(ctx, log, "bin", ManagedBinaryDir, "libexec")
}

// hostRootMovingMarker is the file the agent library keeps in the host root
// until a move has removed the files under the legacy root. The library
// doesn't export it (movingMarker in
// github.com/Azure/unbounded/internal/hostroot); uninstall.sh names it too.
const hostRootMovingMarker = ".moving"

// HostRootMoveUnfinishedHere is hostRootMoveUnfinished for this host's root.
func HostRootMoveUnfinishedHere() (bool, error) {
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

// VerifyMovedAgent runs the current agent binary from a copy of the layout
// under root, before any unit names it. A move whose copy fails this is undone,
// and the host stays linked to /usr/local.
func VerifyMovedAgent(ctx context.Context, root string) error {
	return agentbinary.Verify(ctx, UpgradePathsUnder(root).CurrentPath)
}

// SyncDirs makes the entries in each directory durable: the files renamed into
// it or removed from it. Each directory is synced once.
func SyncDirs(dirs ...string) error {
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
