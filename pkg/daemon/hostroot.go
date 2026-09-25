package daemon

import (
	"context"
	"log/slog"
	"path/filepath"

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
// root. They are the binary layout only: files the agent library installs, such
// as the nspawn lifecycle helper, can be left behind by an older reset.
func HostRootMarkers() []string {
	return []string{
		filepath.Join("bin", binaryName),
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
