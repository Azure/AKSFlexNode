// Package yurthub installs, routes and manages OpenYurt YurtHub in a Flex worker.
package yurthub

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
)

const servicePath = "etc/systemd/system/aks-flex-yurthub.service"

//go:embed assets/aks-flex-yurthub.service
var serviceUnit []byte

// ensureStateDirs creates Hub's state in this worker generation; repave starts
// with a new rootfs, so cache and credentials never carry over.
func ensureStateDirs(root *workerRoot) error {
	for _, dir := range []string{"var/lib/yurthub", "var/lib/yurthub/cache", "var/lib/yurthub/pki"} {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create yurthub state directory %s: %w", dir, err)
		}
	}
	return nil
}

// stageService writes the Flex-owned unit when it differs.
func stageService(root *workerRoot, binaryPath string) error {
	unit := bytes.ReplaceAll(serviceUnit, []byte("/"+installedBinary), []byte(binaryPath))
	info, err := root.Lstat(servicePath)
	switch {
	case err == nil:
		// This Flex-prefixed unit is agent-owned, like the NPD service. Preserve
		// masks/symlinks rather than silently overriding administrative intent.
		if !info.Mode().IsRegular() {
			return fmt.Errorf("yurthub service path is not a regular file")
		}
		current, err := root.ReadFile(servicePath)
		if err != nil {
			return fmt.Errorf("read yurthub service: %w", err)
		}
		if bytes.Equal(current, unit) {
			return nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect yurthub service: %w", err)
	}

	if err := root.WriteFile(servicePath, unit, 0o644); err != nil {
		return fmt.Errorf("write yurthub service: %w", err)
	}
	return nil
}
