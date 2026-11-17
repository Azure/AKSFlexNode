package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
	corev1 "k8s.io/api/core/v1"
)

type activeMachine struct {
	Name  string
	State *State
}

type nodeOperator interface {
	LoadState(context.Context) (*State, error)
	ApplyGoalState(context.Context, *slog.Logger, aksmachine.GoalState) (*State, error)
	RestartNode(context.Context, *slog.Logger) error
	// ResumeRepave recovers durable work before accepting a new goal. True means
	// this reconciliation completed previously prepared work rather than a new goal.
	ResumeRepave(context.Context, *slog.Logger, *corev1.Node) (bool, error)
	// ResetNode removes runtime and persisted state without stopping this daemon.
	// The controller publishes completion before StopDaemon removes the service.
	ResetNode(context.Context, *slog.Logger) error
	StopDaemon(context.Context, *slog.Logger) error
}

type operatorBase struct {
	cfg                    *config.Config
	state                  stateStore
	bootstrapDataRefresher bootstrapDataRefresher
	provider               NodeProvider
}

func newOperatorBase(ctx context.Context, cfg *config.Config, store stateStore, provider NodeProvider) (*operatorBase, *State, error) {
	if store == nil {
		return nil, nil, fmt.Errorf("state store is nil")
	}
	applied, err := store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := validateAppliedProvider(provider, applied); err != nil {
		return nil, nil, err
	}
	return &operatorBase{cfg: cfg, state: store, bootstrapDataRefresher: bootstrapDataRefresherForConfig(cfg), provider: provider}, applied, nil
}

func (o *operatorBase) LoadState(ctx context.Context) (*State, error) { return o.state.Load(ctx) }
func (o *operatorBase) ResetNode(ctx context.Context, log *slog.Logger) error {
	applied, err := o.state.Load(ctx)
	if err != nil {
		return err
	}
	if err := validateAppliedProvider(o.provider, applied); err != nil {
		return err
	}
	if applied != nil {
		if err := o.provider.ValidateResetState(applied); err != nil {
			return err
		}
	}
	return phases.ExecuteTask(ctx, log, o.provider.ResetNode(log))
}

func (o *operatorBase) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return phases.ExecuteTask(ctx, log, UninstallService(log))
}

func (o *operatorBase) configForGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*config.Config, error) {
	// Keep short-lived bootstrap credentials scoped to this repave.
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
	// MaxPods is immutable in AKS; the startup config remains authoritative.
	if goal.KubernetesVersion != "" && cfg.Components.Kubernetes != goal.KubernetesVersion {
		log.Info("updated Kubernetes version for repave", "oldVersion", cfg.Components.Kubernetes, "newVersion", goal.KubernetesVersion)
		cfg.Components.Kubernetes = goal.KubernetesVersion
	}
	return cfg, nil
}

func nextAppliedState(current *State, goal aksmachine.GoalState, active *activeMachine) *State {
	next := &State{AppliedSettingsVersion: goal.SettingsVersion, AppliedKubernetesVersion: goal.KubernetesVersion}
	if current != nil {
		next.PreviousSettingsVersion = current.AppliedSettingsVersion
		next.PreviousKubernetesVersion = current.AppliedKubernetesVersion
	}
	if active != nil {
		next.ActiveMachine = active.Name
	}
	return next
}
