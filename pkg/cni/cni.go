package cni

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// bridgeConfFile is the filename for the default bridge CNI config.
const bridgeConfFile = "99-bridge.conf"

//go:embed assets/99-bridge.conf
var defaultBridgeCNIConfig []byte

type writeCNIConfigTask struct {
	machineDir string
}

// WriteCNIConfig returns a task that writes the default bridge CNI config
// into the nspawn rootfs at /etc/cni/net.d/99-bridge.conf.
func WriteCNIConfig(machineDir string) phases.Task {
	return &writeCNIConfigTask{machineDir: machineDir}
}

func (t *writeCNIConfigTask) Name() string { return "write-cni-config" }

func (t *writeCNIConfigTask) Do(_ context.Context) error {
	confDir := filepath.Join(t.machineDir, config.DefaultCNIConfigDir)
	if err := os.MkdirAll(confDir, 0o750); err != nil { //nolint:gosec // directory needs to be traversable
		return fmt.Errorf("create CNI config directory: %w", err)
	}

	confPath := filepath.Join(confDir, bridgeConfFile)

	current, err := os.ReadFile(confPath) //nolint:gosec // path is constructed, not user input
	if err == nil && string(current) == string(defaultBridgeCNIConfig) {
		return nil
	}

	if err := os.WriteFile(confPath, defaultBridgeCNIConfig, 0o644); err != nil { //nolint:gosec // CNI config must be world-readable
		return fmt.Errorf("write CNI bridge config: %w", err)
	}
	return nil
}
