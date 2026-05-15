package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/cni"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
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
		cfg.Kubernetes.Version = active.State.AppliedKubernetesVersion
	}
	agentCfg := config.ToAgentConfig(cfg, active.Name)
	gs, err := goalstates.ResolveMachine(log, agentCfg, active.Name, nil)
	if err != nil {
		return fmt.Errorf("resolve goal state for node restart: %w", err)
	}

	return phases.Serial(log,
		nodestop.StopNode(log, active.Name),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, active.Name),
		npd.Start(cfg, log, gs.RootFS.MachineDir, active.Name),
	).Do(ctx)
}

type nspawnNodeOperator struct {
	cfg   *config.Config
	state stateStore
}

func newNSpawnNodeOperator(cfg *config.Config, state stateStore) (*nspawnNodeOperator, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if state == nil {
		return nil, fmt.Errorf("state store is nil")
	}
	return &nspawnNodeOperator{cfg: cfg, state: state}, nil
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
	cfg := o.cfg.DeepCopy()
	if goal.KubernetesVersion != "" {
		cfg.Kubernetes.Version = goal.KubernetesVersion
	}
	oldMachine := active.Name
	newMachine := goalstates.AlternateMachine(oldMachine)
	log.Info("starting nspawn machine goal-state apply", "oldMachine", oldMachine, "newMachine", newMachine, "settingsVersion", goal.SettingsVersion, "kubernetesVersion", cfg.Kubernetes.Version)

	agentCfg := config.ToAgentConfig(cfg, newMachine)
	gs, err := goalstates.ResolveMachine(log, agentCfg, newMachine, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve goal state for repave: %w", err)
	}

	tasks := phases.Serial(log,
		rootfs.Provision(log, gs.RootFS),
		phases.Parallel(log,
			npd.Download(cfg, gs.RootFS.MachineDir),
			InstallBinary(gs.RootFS.MachineDir),
			cni.WriteCNIConfig(gs.RootFS.MachineDir),
		),
		nodestop.StopNode(log, oldMachine),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, newMachine),
		npd.Start(cfg, log, gs.RootFS.MachineDir, newMachine),
		reset.CleanupMachine(log, oldMachine),
	)
	if err := tasks.Do(ctx); err != nil {
		return nil, fmt.Errorf("apply machine goal state: %w", err)
	}
	newState := nextAppliedState(active.State, goal, &activeMachine{Name: newMachine})
	if err := o.state.Save(ctx, newState); err != nil {
		return nil, fmt.Errorf("save daemon state: %w", err)
	}
	return newState, nil
}

func (o *nspawnNodeOperator) ResetNode(ctx context.Context, log *slog.Logger) error {
	if err := phases.Serial(log,
		phases.Parallel(log,
			nodestop.StopNode(log, goalstates.NSpawnMachineKube1),
			nodestop.StopNode(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.CleanupMachine(log, goalstates.NSpawnMachineKube1),
			reset.CleanupMachine(log, goalstates.NSpawnMachineKube2),
		),
		reset.CleanupRoutes(log),
		reset.ReloadSystemd(log),
	).Do(ctx); err != nil {
		return err
	}
	if err := o.state.Delete(ctx); err != nil {
		return fmt.Errorf("delete daemon state: %w", err)
	}
	return nil
}

func (o *nspawnNodeOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return UninstallService(ctx, log)
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
