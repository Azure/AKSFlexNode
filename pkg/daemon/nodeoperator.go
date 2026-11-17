package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	runtimeprovider "github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type activeMachine = runtimeprovider.ActiveMachine

type nodeOperator = runtimeprovider.Operator

type operatorBase struct {
	cfg                    *config.Config
	state                  stateStore
	bootstrapDataRefresher bootstrapDataRefresher
	provider               NodeProvider
}

func newOperatorBase(ctx context.Context, cfg *config.Config, store stateStore, provider NodeProvider) (*operatorBase, *runtimeprovider.State, error) {
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

func (o *operatorBase) LoadState(ctx context.Context) (*runtimeprovider.State, error) {
	return o.state.Load(ctx)
}
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

func nextAppliedState(current *runtimeprovider.State, goal aksmachine.GoalState, active *activeMachine) *runtimeprovider.State {
	next := &runtimeprovider.State{AppliedSettingsVersion: goal.SettingsVersion, AppliedKubernetesVersion: goal.KubernetesVersion}
	if current != nil {
		next.Isolation = current.Isolation
		next.PreviousSettingsVersion = current.AppliedSettingsVersion
		next.PreviousKubernetesVersion = current.AppliedKubernetesVersion
	}
	if active != nil {
		next.ActiveMachine = active.Name
	}
	return next
}
