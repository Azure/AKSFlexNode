package aksmachine

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// NewMachineClient creates a MachineClient instance from config.
func NewMachineClient(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	return newMachineClientFromConfig(cfg, logger)
}

func newMachineClientFromConfig(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	if cfg.Agent.MachineClient.Mode == config.MachineClientModeInCluster {
		logger.Info("using Kubernetes service-proxy machine endpoint", "endpointURL", cfg.Agent.MachineClient.EndpointURL)
		return newClusterEndpointClientFromBootstrapConfig(cfg, logger)
	}
	if cfg.Agent.MachineClient.EndpointURL != "" {
		logger.Warn("using ARM proxy machine client for dev-test")
		return newARMProxyClient(cfg, logger)
	}
	return newARMClient(cfg, logger)
}
