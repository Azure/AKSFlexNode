package cni

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	DefaultConfigDir = "/etc/cni/net.d"

	// bridgeConfFile is the filename for the default bridge CNI config.
	bridgeConfFile = "99-bridge.conf"
)

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
	confPath := filepath.Join(t.machineDir, DefaultConfigDir, bridgeConfFile)
	if err := utilio.WriteFile(confPath, defaultBridgeCNIConfig, 0o644); err != nil { //nolint:gosec // CNI config must be world-readable
		return fmt.Errorf("write CNI bridge config: %w", err)
	}
	return nil
}
