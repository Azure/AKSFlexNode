package agentdeployment

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

const (
	// BinaryName is the name installed under the host root and the legacy root.
	BinaryName = "aks-flex-node"
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
