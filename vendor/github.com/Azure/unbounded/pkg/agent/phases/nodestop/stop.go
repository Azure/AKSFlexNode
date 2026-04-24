// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestop

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

type stopNode struct {
	log         *slog.Logger
	machineName string
}

// StopNode returns a task that gracefully stops a running nspawn machine node.
// It first attempts to stop kubelet and containerd inside the container so the
// nspawn stop does not have to force-kill them, then delegates to
// reset.StopMachine for the actual nspawn teardown.
func StopNode(log *slog.Logger, machineName string) phases.Task {
	return &stopNode{log: log, machineName: machineName}
}

func (s *stopNode) Name() string { return "stop-node" }

func (s *stopNode) Do(ctx context.Context) error {
	// Gracefully stop kubelet and containerd inside the container first so
	// the nspawn stop is fast and does not have to force-kill them.
	s.log.Info("pre-stopping services in machine", "machine", s.machineName)

	if _, err := utilexec.MachineRun(ctx, s.log, s.machineName, "systemctl", "stop", goalstates.SystemdUnitKubelet); err != nil {
		s.log.Warn("failed to pre-stop kubelet (proceeding anyway)", "machine", s.machineName, "error", err)
	}

	if _, err := utilexec.MachineRun(ctx, s.log, s.machineName, "systemctl", "stop", goalstates.SystemdUnitContainerd); err != nil {
		s.log.Warn("failed to pre-stop containerd (proceeding anyway)", "machine", s.machineName, "error", err)
	}

	s.log.Info("stopping machine", "machine", s.machineName)

	if err := reset.StopMachine(s.log, s.machineName).Do(ctx); err != nil {
		return fmt.Errorf("stop machine %s: %w", s.machineName, err)
	}

	return nil
}
