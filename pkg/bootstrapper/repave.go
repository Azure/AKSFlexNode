package bootstrapper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/cni"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// ActiveMachine describes the currently running nspawn machine side.
type ActiveMachine struct {
	Name string
}

// Repave performs a blue/green nspawn node replacement.
//
// It discovers the currently running side (kube1 or kube2) from host machinectl
// state, provisions the inactive alternate side with the desired config, stops
// the old side, starts the new side, waits for kubelet to become active, and
// then removes the old side's nspawn artifacts. It fails if no side is running
// or if both sides are running, because either state makes the cutover target
// ambiguous.
func Repave(
	ctx context.Context,
	cfg *config.Config,
	logger *slog.Logger,
) error {
	// Unlike unbounded-agent, AKS FlexNode does not use persisted applied-config
	// as the desired state source. Drift remediation already resolved desired
	// state from fresh AKS/status snapshots, so here we only need host runtime
	// state to decide which nspawn side is currently active.
	active, err := FindActiveMachine(ctx, logger)
	if err != nil {
		return err
	}
	oldMachine := active.Name
	newMachine := goalstates.AlternateMachine(oldMachine)

	logger.Info("Starting nspawn machine repave", "oldMachine", oldMachine, "newMachine", newMachine)

	agentCfg := config.ToAgentConfig(cfg, newMachine)
	gs, err := goalstates.ResolveMachine(logger, agentCfg, newMachine, nil)
	if err != nil {
		return fmt.Errorf("resolve goal state for repave: %w", err)
	}

	repaveTasks := phases.Serial(logger,
		rootfs.Provision(logger, gs.RootFS),
		phases.Parallel(logger,
			npd.Download(cfg, gs.RootFS.MachineDir),
			daemon.InstallBinary(gs.RootFS.MachineDir),
			cni.WriteCNIConfig(gs.RootFS.MachineDir),
		),
		// TODO: refine goal-state persistence and use it for active/desired
		// machine discovery. Today a failure after this cutover may leave the
		// old side stopped without enough persisted side state to drive rollback.
		nodestop.StopNode(logger, oldMachine),
		nodestart.StartNode(logger, gs.NodeStart),
		nodestart.WaitForKubelet(logger, newMachine),
		npd.Start(cfg, logger, gs.RootFS.MachineDir, newMachine),
		reset.CleanupMachine(logger, oldMachine),
	)

	if err := repaveTasks.Do(ctx); err != nil {
		return fmt.Errorf("repave nspawn machine: %w", err)
	}

	return nil
}

// FindActiveMachine returns the single running nspawn side detected from host machinectl state.
func FindActiveMachine(ctx context.Context, logger *slog.Logger) (*ActiveMachine, error) {
	var active []string
	for _, machineName := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		running, err := isMachineRunning(ctx, logger, machineName)
		if err != nil {
			logger.Debug("Failed to inspect nspawn machine", "machine", machineName, "error", err)
			continue
		}
		if running {
			active = append(active, machineName)
		}
	}

	switch len(active) {
	case 0:
		return nil, errors.New("no active nspawn machine found")
	case 1:
		return &ActiveMachine{Name: active[0]}, nil
	default:
		return nil, fmt.Errorf("multiple active nspawn machines found: %s", strings.Join(active, ", "))
	}
}

func isMachineRunning(ctx context.Context, logger *slog.Logger, machineName string) (bool, error) {
	state, err := utilexec.OutputCmdAt(ctx, logger, slog.LevelDebug, "machinectl", "show", machineName, "--property=State", "--value")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(state) == "running", nil
}
