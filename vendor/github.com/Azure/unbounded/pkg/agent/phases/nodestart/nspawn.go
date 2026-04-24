// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

type startNSpawnMachine struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// StartNSpawnMachine returns a task that starts the systemd-nspawn machine using machinectl and
// waits until D-Bus is responsive inside the machine so that subsequent phases
// can safely use utilexec.MachineRun().
func StartNSpawnMachine(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &startNSpawnMachine{log: log, goalState: goalState}
}

func (s *startNSpawnMachine) Name() string { return "start-nspawn-machine" }

func (s *startNSpawnMachine) Do(ctx context.Context) error {
	if err := utilexec.RunCmd(ctx, s.log, utilexec.Machinectl(), "enable", s.goalState.MachineName); err != nil {
		return fmt.Errorf("machinectl enable %s: %w", s.goalState.MachineName, err)
	}

	if err := utilexec.RunCmd(ctx, s.log, utilexec.Machinectl(), "start", s.goalState.MachineName); err != nil {
		return fmt.Errorf("machinectl start %s: %w", s.goalState.MachineName, err)
	}

	if err := waitForMachine(ctx, s.log, s.goalState.MachineName); err != nil {
		return fmt.Errorf("wait for machine %s: %w", s.goalState.MachineName, err)
	}

	return nil
}

// waitForMachine polls the machine until it is responsive to systemd-run
// commands. machinectl start returns before D-Bus is ready, so phases that use
// utilexec.MachineRun() would fail without this gate.
func waitForMachine(ctx context.Context, log *slog.Logger, machine string) error {
	const (
		pollInterval = 500 * time.Millisecond
		timeout      = 30 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		if _, err := utilexec.MachineRun(ctx, log, machine, "/bin/true"); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			// TODO: dump machine state (e.g. machinectl status, journalctl) to aid debugging when the machine fails to start.
			return fmt.Errorf("machine %s not responsive after %s: %w", machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
