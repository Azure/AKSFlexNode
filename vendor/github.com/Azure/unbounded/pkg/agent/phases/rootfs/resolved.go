// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

type disableResolved struct {
	goalState *goalstates.RootFS
}

// DisableResolved returns a task that masks systemd-resolved inside the container
// and writes a static resolv.conf copied from the host.
//
// The debootstrap rootfs includes systemd-resolved which starts on boot and
// overwrites /etc/resolv.conf with "No DNS servers known." since it has no
// upstream configuration inside the container. By masking the service and
// writing a static file we avoid the conflict entirely. With
// VirtualEthernet=no the container shares the host network namespace, so the
// host's systemd-resolved stub at 127.0.0.53 is reachable.
//
// NOTE: If we were building our own rootfs we could just ditch
// systemd-resolved entirely.
func DisableResolved(goalState *goalstates.RootFS) phases.Task {
	return &disableResolved{goalState: goalState}
}

func (d *disableResolved) Name() string { return "disable-resolved" }

func (d *disableResolved) Do(_ context.Context) error {
	// Mask systemd-resolved so it never starts inside the container.
	maskedService := filepath.Join(d.goalState.MachineDir, "etc/systemd/system/systemd-resolved.service")
	if err := os.MkdirAll(filepath.Dir(maskedService), 0o755); err != nil {
		return fmt.Errorf("create directory for masked service: %w", err)
	}
	// Remove any existing file/symlink before creating the mask.
	os.Remove(maskedService) //nolint:errcheck // best-effort removal

	if err := os.Symlink("/dev/null", maskedService); err != nil {
		return fmt.Errorf("mask systemd-resolved: %w", err)
	}

	// Copy the host's resolv.conf into the container rootfs. We read the
	// host file (following symlinks) and write a regular file so the
	// container has a static copy.
	hostResolvConf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return fmt.Errorf("read host /etc/resolv.conf: %w", err)
	}

	dest := filepath.Join(d.goalState.MachineDir, "etc/resolv.conf")
	if err := utilio.WriteFile(dest, hostResolvConf, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}

	return nil
}
