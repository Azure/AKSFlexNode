// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

type stopMachine struct {
	log         *slog.Logger
	machineName string
}

// StopMachine returns a task that stops the nspawn machine, waits for it to
// fully stop (up to 30 seconds), and force-terminates it if necessary.
func StopMachine(log *slog.Logger, machineName string) phases.Task {
	return &stopMachine{log: log, machineName: machineName}
}

func (t *stopMachine) Name() string { return "stop-machine" }

func (t *stopMachine) Do(ctx context.Context) error {
	if err := utilexec.RunCmd(ctx, t.log, utilexec.Machinectl(), "disable", t.machineName); err != nil {
		t.log.Warn("failed to disable machine (may not have been enabled)", "machine", t.machineName, "error", err)
	}

	if !machineExists(ctx, t.log, t.machineName) {
		t.log.Info("machine not running, nothing to stop", "machine", t.machineName)
		return nil
	}

	t.log.Info("stopping nspawn machine", "machine", t.machineName)

	// Stop the systemd service that manages the nspawn container. This
	// properly tears down mount namespaces and cgroups so that
	// machinectl remove can succeed.
	serviceName := fmt.Sprintf("systemd-nspawn@%s.service", t.machineName)

	if !serviceIsActive(ctx, t.log, serviceName) {
		t.log.Info("nspawn service already inactive, skipping stop", "service", serviceName)
	} else if err := utilexec.RunCmd(ctx, t.log, utilexec.Systemctl(), "stop", serviceName); err != nil {
		t.log.Warn("failed to stop nspawn service", "service", serviceName, "error", err)
	}

	// Wait up to 30 seconds for the machine to fully stop.
	if t.waitForGone(ctx, 30*time.Second) {
		return nil
	}

	// Force terminate if still registered.
	if machineExists(ctx, t.log, t.machineName) {
		t.log.Warn("machine did not stop gracefully, terminating", "machine", t.machineName)

		if err := utilexec.RunCmd(ctx, t.log, utilexec.Machinectl(), "terminate", t.machineName); err != nil {
			t.log.Warn("failed to terminate machine", "machine", t.machineName, "error", err)
		}

		// Wait up to 15 seconds for the terminate to take full effect.
		t.waitForGone(ctx, 15*time.Second)
	}

	return ctx.Err()
}

// waitForGone polls machineExists until the machine disappears or the timeout
// elapses. Returns true if the machine is gone.
func (t *stopMachine) waitForGone(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !machineExists(ctx, t.log, t.machineName) {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}

	return !machineExists(ctx, t.log, t.machineName)
}

type removeMachine struct {
	log         *slog.Logger
	machineName string
}

// RemoveMachine returns a task that removes the machine rootfs using
// machinectl and then force-removes the directory.
func RemoveMachine(log *slog.Logger, machineName string) phases.Task {
	return &removeMachine{log: log, machineName: machineName}
}

func (t *removeMachine) Name() string { return "remove-machine" }

func (t *removeMachine) Do(ctx context.Context) error {
	machineDir := fmt.Sprintf("/var/lib/machines/%s", t.machineName)

	// Skip entirely if the machine directory doesn't exist — nothing to remove.
	if _, err := os.Stat(machineDir); errors.Is(err, os.ErrNotExist) {
		t.log.Info("machine rootfs not present, nothing to remove", "machine", t.machineName)
		return nil
	}

	t.log.Info("removing machine rootfs", "machine", t.machineName, "dir", machineDir)

	// Retry machinectl remove with backoff. The image may briefly remain
	// "busy" after the systemd-nspawn service stops while cgroup and mount
	// teardown completes asynchronously.
	const (
		retryTimeout  = 60 * time.Second
		retryInterval = 2 * time.Second
	)

	deadline := time.Now().Add(retryTimeout)
	for time.Now().Before(deadline) {
		err := utilexec.RunCmd(ctx, t.log, utilexec.Machinectl(), "remove", t.machineName)
		if err == nil {
			return nil // machinectl removed both image metadata and directory
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}

	// Fallback: force-remove the directory if machinectl keeps failing.
	t.log.Warn("machinectl remove did not succeed, force-removing directory", "dir", machineDir)
	removeAllIfExists(t.log, machineDir)

	return nil
}

// machineExists checks whether the named nspawn machine is known to machinectl.
func machineExists(ctx context.Context, log *slog.Logger, name string) bool {
	err := utilexec.RunCmd(ctx, log, utilexec.Machinectl(), "show", name)
	return err == nil
}

// serviceIsActive returns true if the named systemd service is currently active.
func serviceIsActive(ctx context.Context, log *slog.Logger, service string) bool {
	err := utilexec.RunCmd(ctx, log, utilexec.Systemctl(), "is-active", "--quiet", service)
	return err == nil
}
