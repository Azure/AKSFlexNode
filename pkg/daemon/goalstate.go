package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

func configForGoalState(ctx context.Context, log *slog.Logger, cfgBase *config.Config, refresher bootstrapDataRefresher, goal aksmachine.GoalState) (*config.Config, error) {
	// Keep short-lived bootstrap credentials scoped to this repave. Persisting
	// them would make a later repave depend on this token's lifetime again.
	cfg := cfgBase.DeepCopy()
	if cfg == nil {
		return nil, fmt.Errorf("copy config for repave")
	}
	data, err := refresher.Fetch(ctx, cfg)
	switch {
	case err != nil:
		return nil, fmt.Errorf("refresh bootstrap data for repave: %w", err)
	case data != nil:
		log.Info("refreshed AKS bootstrap data for repave")
		if data.BootstrapToken == "" {
			return nil, fmt.Errorf("refresh bootstrap data for repave: response did not contain a bootstrap token")
		}
		if cfg.Azure.BootstrapToken.Token != data.BootstrapToken {
			// Never log either token value; only record that the sensitive value changed.
			log.Info("updated bootstrap token for repave")
			cfg.Azure.BootstrapToken.Token = data.BootstrapToken
		}
		if data.CACertData != "" && cfg.Node.Kubelet.CACertData != data.CACertData {
			// Log only the change so the complete bootstrap response stays private.
			log.Info("updated kubelet CA data for repave")
			cfg.Node.Kubelet.CACertData = data.CACertData
		}
	}
	// MaxPods is immutable in AKS, so the startup configuration remains its
	// authoritative source rather than reapplying the value from each goal.
	if goal.KubernetesVersion != "" && cfg.Components.Kubernetes != goal.KubernetesVersion {
		log.Info("updated Kubernetes version for repave",
			"oldVersion", cfg.Components.Kubernetes,
			"newVersion", goal.KubernetesVersion,
		)
		cfg.Components.Kubernetes = goal.KubernetesVersion
	}
	return cfg, nil
}
