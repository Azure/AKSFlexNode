// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

type waitForKubelet struct {
	log     *slog.Logger
	machine string
}

// WaitForKubelet returns a task that polls the kubelet systemd service inside
// the nspawn machine until it reports as active.
func WaitForKubelet(log *slog.Logger, machineName string) phases.Task {
	return &waitForKubelet{log: log, machine: machineName}
}

func (w *waitForKubelet) Name() string { return "wait-for-kubelet" }

func (w *waitForKubelet) Do(ctx context.Context) error {
	const (
		pollInterval = 2 * time.Second
		timeout      = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		out, err := utilexec.MachineRun(ctx, w.log, w.machine,
			"systemctl", "is-active", goalstates.SystemdUnitKubelet,
		)
		if err == nil && strings.TrimSpace(out) == "active" {
			w.log.Info("kubelet is active", "machine", w.machine)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("kubelet not active in %s after %s: %w", w.machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
