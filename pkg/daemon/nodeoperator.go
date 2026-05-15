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

type ActiveMachine struct {
	Name  string
	State *State
}

type NodeOperator interface {
	LoadState(ctx context.Context) (*State, error)
	FindActiveMachine(ctx context.Context, log *slog.Logger, state *State) (*ActiveMachine, error)
	ApplyGoalState(ctx context.Context, log *slog.Logger, active *ActiveMachine, goal aksmachine.GoalState) (*State, error)
	RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error
	// ResetNodeRuntime removes nspawn node runtime state but must not stop this
	// daemon process. The controller deletes the Kubernetes Node after host cleanup.
	ResetNodeRuntime(ctx context.Context, log *slog.Logger) error
	ClearState(ctx context.Context) error
	// StopDaemon stops/removes the daemon after lifecycle completion is visible to AKS RP.
	StopDaemon(ctx context.Context, log *slog.Logger) error
}

func (o *NSpawnNodeOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	if o.cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if active == nil || active.Name == "" {
		return fmt.Errorf("active machine is empty")
	}
	if active.State == nil {
		return fmt.Errorf("active machine state is empty")
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

type NSpawnNodeOperator struct {
	cfg   *config.Config
	state stateStore
}

func NewNSpawnNodeOperator(cfg *config.Config, state stateStore) *NSpawnNodeOperator {
	return &NSpawnNodeOperator{cfg: cfg, state: state}
}

func (o *NSpawnNodeOperator) LoadState(ctx context.Context) (*State, error) {
	if o.state == nil {
		return nil, fmt.Errorf("state store is nil")
	}
	return o.state.Load(ctx)
}

func (o *NSpawnNodeOperator) FindActiveMachine(_ context.Context, _ *slog.Logger, state *State) (*ActiveMachine, error) {
	if state == nil {
		return nil, fmt.Errorf("daemon state is missing active machine")
	}
	if !validActiveMachine(state.ActiveMachine) {
		return nil, fmt.Errorf("daemon state active machine %q is invalid", state.ActiveMachine)
	}
	return &ActiveMachine{Name: state.ActiveMachine, State: state}, nil
}

func (o *NSpawnNodeOperator) ApplyGoalState(ctx context.Context, log *slog.Logger, active *ActiveMachine, goal aksmachine.GoalState) (*State, error) {
	if o.cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if active == nil || active.Name == "" {
		return nil, fmt.Errorf("active machine is empty")
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
	newState := nextAppliedState(active.State, goal, &ActiveMachine{Name: newMachine})
	if err := o.state.Save(ctx, newState); err != nil {
		return nil, err
	}
	return newState, nil
}

func (o *NSpawnNodeOperator) ResetNodeRuntime(ctx context.Context, log *slog.Logger) error {
	return phases.Serial(log,
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
	).Do(ctx)
}

func (o *NSpawnNodeOperator) ClearState(ctx context.Context) error {
	if o.state == nil {
		return fmt.Errorf("state store is nil")
	}
	return o.state.Delete(ctx)
}

func (o *NSpawnNodeOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return UninstallService(ctx, log)
}

func nextAppliedState(current *State, goal aksmachine.GoalState, active *ActiveMachine) *State {
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
