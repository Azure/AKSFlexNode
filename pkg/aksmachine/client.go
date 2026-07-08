package aksmachine

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func newMachineClientFromConfig(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	switch cfg.Agent.MachineClient.Mode {
	case config.MachineClientModeInCluster:
		logger.Info("using Kubernetes service-proxy machine endpoint", "endpointURL", cfg.Agent.MachineClient.EndpointURL)
		return newClusterEndpointClientFromBootstrapConfig(cfg, logger)
	case config.MachineClientModeE2E:
		logger.Warn("local e2e machine client is not supported in current build; using ARM machine client")
	}
	if cfg.Agent.MachineClient.EndpointURL != "" {
		logger.Warn("using ARM proxy machine client for dev-test")
		return newARMProxyClient(cfg, logger)
	}
	return newARMClient(cfg, logger)
}
