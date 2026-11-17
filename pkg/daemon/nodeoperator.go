package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type activeMachine struct {
	Name  string
	State *State
}

type nodeOperator interface {
	LoadState(ctx context.Context) (*State, error)
	ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error)
	RestartNode(ctx context.Context, log *slog.Logger) error
	// ResetNode removes runtime and state without stopping the daemon, which
	// publishes lifecycle completion before StopDaemon.
	ResetNode(ctx context.Context, log *slog.Logger) error
	StopDaemon(ctx context.Context, log *slog.Logger) error
}

type workerNodeOperator struct {
	cfg                    *config.Config
	state                  stateStore
	runtime                provider.Provider
	bootstrapDataRefresher bootstrapDataRefresher
	lifecycleUnits         phases.Task
}

func newNodeOperator(ctx context.Context, cfg *config.Config, log *slog.Logger, state stateStore) (*workerNodeOperator, error) {
	if state == nil {
		return nil, fmt.Errorf("state store is nil")
	}
	applied, err := state.Load(ctx)
	if err != nil {
		return nil, err
	}
	if applied != nil && applied.Isolation != cfg.Bootstrap.Isolation {
		return nil, fmt.Errorf("worker isolation change requires reset")
	}
	runtime, err := provider.New(cfg.Bootstrap.Isolation)
	if err != nil {
		return nil, err
	}
	if Containerized() && cfg.Bootstrap.Isolation != isolation.ModeNSpawn {
		return nil, fmt.Errorf("containerized agent does not yet support %s workers; use a host-installed agent", cfg.Bootstrap.Isolation)
	}
	return &workerNodeOperator{cfg: cfg, state: state, runtime: runtime, bootstrapDataRefresher: bootstrapDataRefresherForConfig(cfg), lifecycleUnits: EnsureNSpawnLifecycleUnits(log)}, nil
}
func (o *workerNodeOperator) LoadState(ctx context.Context) (*State, error) { return o.state.Load(ctx) }
func (o *workerNodeOperator) findActiveMachine(ctx context.Context) (*activeMachine, error) {
	return activeMachineFromStore(ctx, o.state)
}

func (o *workerNodeOperator) appliedNode(active *activeMachine) provider.Node {
	cfg := active.State.NodeConfig
	if cfg == nil {
		// Only installations predating worker selection lack a runtime config.
		legacy := o.cfg.DeepCopy()
		if active.State.AppliedKubernetesVersion != "" {
			legacy.Components.Kubernetes = active.State.AppliedKubernetesVersion
		}
		cfg = config.ToAgentConfig(legacy, active.Name)
	}
	return provider.Node{Slot: active.Name, Config: cfg}
}

func (o *workerNodeOperator) RestartNode(ctx context.Context, log *slog.Logger) error {
	active, err := o.findActiveMachine(ctx)
	if err != nil {
		return err
	}
	return o.runtime.Restart(ctx, log, provider.StartParams{Node: o.appliedNode(active), Config: o.cfg})
}

func (o *workerNodeOperator) Recover(ctx context.Context, log *slog.Logger) error {
	active, err := o.findActiveMachine(ctx)
	if err != nil {
		return err
	}
	return o.runtime.Recover(ctx, log, provider.StartParams{Node: o.appliedNode(active), Config: o.cfg})
}

func (o *workerNodeOperator) ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error) {
	active, err := o.findActiveMachine(ctx)
	if err != nil {
		return nil, err
	}
	if active.State.Isolation != o.cfg.Bootstrap.Isolation {
		return nil, fmt.Errorf("worker isolation change requires reset")
	}
	previous := o.appliedNode(active)
	if err := o.runtime.Recover(ctx, log, provider.StartParams{Node: previous, Config: o.cfg}); err != nil {
		return nil, fmt.Errorf("recover worker upgrade: %w", err)
	}
	cfg, err := o.configForGoalState(ctx, log, goal)
	if err != nil {
		return nil, err
	}
	newSlot := o.runtime.Layout().NextSlot(active.Name)
	ac, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, newSlot)
	if err != nil {
		return nil, fmt.Errorf("resolve repave inputs: %w", err)
	}
	// A none repave replaces identity at shared host paths. Restore direct API
	// access and remove Hub's old identity before the SDK snapshots the node.
	if active.State.Isolation == isolation.ModeNone {
		if err := clearHostHub(ctx, log); err != nil {
			return nil, err
		}
	}
	params := provider.UpgradeParams{StartParams: provider.StartParams{
		Node: provider.Node{Slot: newSlot, Config: ac, GoalState: gs}, Config: cfg, Archives: archives,
	}, Previous: previous}
	if cfg.Bootstrap.Isolation == isolation.ModeNSpawn {
		if err := o.lifecycleUnits.Do(ctx); err != nil {
			return nil, err
		}
	}
	if err := o.runtime.Upgrade(ctx, log, params); err != nil {
		return nil, fmt.Errorf("apply worker upgrade: %w", err)
	}
	next := nextAppliedState(active.State, goal, &activeMachine{Name: newSlot})
	next.Isolation, next.NodeConfig = cfg.Bootstrap.Isolation, ac.DeepCopy()
	if err := o.state.Save(ctx, next); err != nil {
		return nil, fmt.Errorf("save applied worker: %w", err)
	}
	if err := o.runtime.Retire(ctx, log, params); err != nil {
		return nil, err
	}
	return next, nil
}

func (o *workerNodeOperator) configForGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*config.Config, error) {
	// Short-lived credentials are fetched for this repave, not read from a
	// previous applied config after they may have expired.
	cfg := o.cfg.DeepCopy()
	if cfg == nil {
		return nil, fmt.Errorf("copy config for repave")
	}
	data, err := o.bootstrapDataRefresher.Fetch(ctx, cfg)
	switch {
	case err != nil:
		return nil, fmt.Errorf("refresh bootstrap data for repave: %w", err)
	case data != nil:
		log.Info("refreshed AKS bootstrap data for repave")
		if data.BootstrapToken == "" {
			return nil, fmt.Errorf("refresh bootstrap data for repave: response did not contain a bootstrap token")
		}
		if cfg.Azure.BootstrapToken.Token != data.BootstrapToken {
			log.Info("updated bootstrap token for repave")
			cfg.Azure.BootstrapToken.Token = data.BootstrapToken
		}
		if data.CACertData != "" && cfg.Node.Kubelet.CACertData != data.CACertData {
			log.Info("updated kubelet CA data for repave")
			cfg.Node.Kubelet.CACertData = data.CACertData
		}
	}
	// MaxPods is immutable in AKS; startup configuration remains authoritative.
	if goal.KubernetesVersion != "" && cfg.Components.Kubernetes != goal.KubernetesVersion {
		log.Info("updated Kubernetes version for repave", "oldVersion", cfg.Components.Kubernetes, "newVersion", goal.KubernetesVersion)
		cfg.Components.Kubernetes = goal.KubernetesVersion
	}
	return cfg, nil
}
func (o *workerNodeOperator) ResetNode(ctx context.Context, log *slog.Logger) error {
	task, err := ResetNode(ctx, log)
	if err != nil {
		return err
	}
	return phases.ExecuteTask(ctx, log, task)
}
func (o *workerNodeOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return phases.ExecuteTask(ctx, log, UninstallService(log))
}
func nextAppliedState(current *State, goal aksmachine.GoalState, active *activeMachine) *State {
	next := &State{AppliedSettingsVersion: goal.SettingsVersion, AppliedKubernetesVersion: goal.KubernetesVersion}
	if current != nil {
		next.PreviousSettingsVersion, next.PreviousKubernetesVersion = current.AppliedSettingsVersion, current.AppliedKubernetesVersion
	}
	if active != nil {
		next.ActiveMachine = active.Name
	}
	return next
}
