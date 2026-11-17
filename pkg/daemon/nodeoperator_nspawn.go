package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	corev1 "k8s.io/api/core/v1"
)

func (o *nspawnNodeOperator) RestartNode(ctx context.Context, log *slog.Logger) error {
	active, err := o.findActiveMachine(ctx)
	if err != nil {
		return err
	}

	cfg := o.cfg.DeepCopy()
	if active.State.AppliedKubernetesVersion != "" {
		cfg.Components.Kubernetes = active.State.AppliedKubernetesVersion
	}
	_, gs, containerImageArchives, err := config.ResolveMachineGoalState(ctx, log, cfg, active.Name)
	if err != nil {
		return fmt.Errorf("resolve goal state for node restart: %w", err)
	}

	return phases.Serial(log,
		stageContainerImageArchiveBindSource(log, containerImageArchives),
		nodestop.StopNode(log, active.Name),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, active.Name),
		npd.Start(log, cfg, gs.RootFS.Root(), gs.NodeStart),
	).Do(ctx)
}

type nspawnNodeOperator struct {
	*operatorBase
}

func (p nspawnProvider) NewOperator(ctx context.Context, cfg *config.Config, state stateStore) (nodeOperator, error) {
	base, _, err := newOperatorBase(ctx, cfg, state, p)
	if err != nil {
		return nil, err
	}
	return &nspawnNodeOperator{operatorBase: base}, nil
}

func (*nspawnNodeOperator) ResumeRepave(context.Context, *slog.Logger, *corev1.Node) (bool, error) {
	return false, nil
}

func (o *nspawnNodeOperator) findActiveMachine(ctx context.Context) (*activeMachine, error) {
	return activeMachineFromStore(ctx, o.state)
}

func (o *nspawnNodeOperator) ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error) {
	active, err := o.findActiveMachine(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := o.configForGoalState(ctx, log, goal)
	if err != nil {
		return nil, err
	}
	oldMachine := active.Name
	newMachine := goalstates.AlternateMachine(oldMachine)
	log.Info("starting nspawn machine goal-state apply",
		"oldMachine", oldMachine,
		"newMachine", newMachine,
		"settingsVersion", goal.SettingsVersion,
		"kubernetesVersion", cfg.Components.Kubernetes,
	)

	_, gs, containerImageArchives, err := config.ResolveMachineGoalState(ctx, log, cfg, newMachine)
	if err != nil {
		return nil, fmt.Errorf("resolve goal state for repave: %w", err)
	}
	newState := nextAppliedState(active.State, goal, &activeMachine{Name: newMachine})

	tasks := phases.Serial(log,
		nodestop.StopNode(log, oldMachine),
		reset.CleanupNetwork(log),
		o.provider.StartNode(cfg, log, newMachine, gs, containerImageArchives, o.state, newState),
		reset.CleanupMachine(log, oldMachine),
	)
	if err := tasks.Do(ctx); err != nil {
		return nil, fmt.Errorf("apply machine goal state: %w", err)
	}
	return newState, nil
}
