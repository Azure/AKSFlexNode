// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

const fstabPath = "/etc/fstab"

type disableSwap struct {
	log *slog.Logger
}

// DisableSwap returns a task that disables swap on the host. Kubernetes
// requires swap to be off so the kubelet memory management and pod QoS
// guarantees work correctly. The task runs swapoff -a and comments out any
// swap entries in /etc/fstab so swap stays disabled across reboots.
func DisableSwap(log *slog.Logger) phases.Task {
	return &disableSwap{log: log}
}

func (d *disableSwap) Name() string { return "disable-swap" }

func (d *disableSwap) Do(ctx context.Context) error {
	if err := utilexec.RunCmd(ctx, d.log, swapoff(), "-a"); err != nil {
		return fmt.Errorf("swapoff -a: %w", err)
	}

	if err := commentOutSwapInFstab(fstabPath); err != nil {
		return fmt.Errorf("commenting out swap in %s: %w", fstabPath, err)
	}

	return nil
}

// commentOutSwapInFstab reads the fstab file at the given path, comments out
// any uncommented lines containing "swap", and writes the result back. A backup
// of the original file is saved to <path>.bak before any modifications are made.
func commentOutSwapInFstab(path string) error {
	content, err := os.ReadFile(path) //#nosec G304 -- trusted path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// no fstab, nothing to do
			return nil
		}

		return fmt.Errorf("reading %s: %w", path, err)
	}

	lines := strings.Split(string(content), "\n")
	modified := false

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// skip empty lines and already-commented lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.Contains(trimmed, "swap") {
			lines[i] = "# " + line
			modified = true
		}
	}

	if !modified {
		return nil
	}

	// back up the original fstab before writing changes
	if err := utilio.WriteFile(path+".bak", content, 0o644); err != nil {
		return fmt.Errorf("backing up %s: %w", path, err)
	}

	newContent := []byte(strings.Join(lines, "\n"))

	return utilio.WriteFile(path, newContent, 0o644)
}

// swapoff returns a command factory for swapoff.
func swapoff() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "swapoff")
	}
}
