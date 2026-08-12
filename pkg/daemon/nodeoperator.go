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
)

type activeMachine struct {
	Name  string
	State *State
}

type nodeOperator interface {
	LoadState(ctx context.Context) (*State, error)
	ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error)
	RestartNode(ctx context.Context, log *slog.Logger) error
	// ResetNode removes nspawn node runtime and persisted daemon state but must
	// not stop this daemon process. The controller publishes lifecycle completion
	// after host cleanup.
	ResetNode(ctx context.Context, log *slog.Logger) error
	// StopDaemon stops/removes the daemon after lifecycle completion is visible to AKS RP.
	StopDaemon(ctx context.Context, log *slog.Logger) error
}

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
		npd.Start(log, cfg, gs.NodeStart),
	).Do(ctx)
}

type nspawnNodeOperator struct {
	cfg                    *config.Config
	state                  stateStore
	bootstrapDataRefresher bootstrapDataRefresher
}

func newNSpawnNodeOperator(cfg *config.Config, state stateStore) (*nspawnNodeOperator, error) {
	if state == nil {
		return nil, fmt.Errorf("state store is nil")
	}
	return &nspawnNodeOperator{
		cfg:                    cfg,
		state:                  state,
		bootstrapDataRefresher: aksBootstrapDataRefresher{},
	}, nil
}

func (o *nspawnNodeOperator) LoadState(ctx context.Context) (*State, error) {
	return o.state.Load(ctx)
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
		StartNode(cfg, log, newMachine, gs, containerImageArchives, o.state, newState),
		reset.CleanupMachine(log, oldMachine),
	)
	if err := tasks.Do(ctx); err != nil {
		return nil, fmt.Errorf("apply machine goal state: %w", err)
	}
	return newState, nil
}

func (o *nspawnNodeOperator) configForGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*config.Config, error) {
	// Keep short-lived bootstrap credentials scoped to this repave. Persisting
	// them would make a later repave depend on this token's lifetime again.
	cfg := o.cfg.DeepCopy()
	if cfg == nil {
		return nil, fmt.Errorf("copy config for repave")
	}
	if shouldRefreshBootstrapData(cfg) {
		if o.bootstrapDataRefresher == nil {
			return nil, fmt.Errorf("bootstrap-data refresher is not configured")
		}
		log.Info("refreshing AKS bootstrap data for repave")
		data, err := o.bootstrapDataRefresher.Fetch(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("refresh bootstrap data for repave: %w", err)
		}
		if data == nil || data.BootstrapToken == "" {
			return nil, fmt.Errorf("refresh bootstrap data for repave: response did not contain a bootstrap token")
		}
		cfg.Azure.BootstrapToken.Token = data.BootstrapToken
		if data.ClusterFQDN != "" {
			cfg.Node.Kubelet.ClusterFQDN = data.ClusterFQDN
		}
		if data.CACertData != "" {
			cfg.Node.Kubelet.CACertData = data.CACertData
		}
	}
	if goal.KubernetesVersion != "" {
		cfg.Components.Kubernetes = goal.KubernetesVersion
	}
	return cfg, nil
}

func (o *nspawnNodeOperator) ResetNode(ctx context.Context, log *slog.Logger) error {
	return phases.ExecuteTask(ctx, log, ResetNode(log))
}

func (o *nspawnNodeOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return phases.ExecuteTask(ctx, log, UninstallService(log))
}

func nextAppliedState(current *State, goal aksmachine.GoalState, active *activeMachine) *State {
	next := &State{
		AppliedSettingsVersion:    goal.SettingsVersion,
		AppliedKubernetesVersion:  goal.KubernetesVersion,
		PreviousSettingsVersion:   "",
		PreviousKubernetesVersion: "",
	}
	if current != nil {
		next.PreviousSettingsVersion = current.AppliedSettingsVersion
		next.PreviousKubernetesVersion = current.AppliedKubernetesVersion
	}
	if active != nil {
		next.ActiveMachine = active.Name
	}
	return next
}
