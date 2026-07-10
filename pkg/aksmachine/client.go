package aksmachine

import (
	"log/slog"

	"k8s.io/client-go/rest"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// MachineClientOptions contains runtime dependencies that are not part of the
// persisted agent configuration.
type MachineClientOptions struct {
	KubernetesRESTConfig *rest.Config
}

// NewMachineClient creates the configured MachineClient. During start, an empty
// options value makes EnsureMachine use bootstrap credentials before daemon
// credentials exist; the daemon supplies its Kubernetes REST config.
func NewMachineClient(cfg *config.Config, logger *slog.Logger, options MachineClientOptions) (MachineClient, error) {
	switch {
	case cfg.Agent.MachineClient.Mode == config.MachineClientModeInCluster && options.KubernetesRESTConfig != nil:
		logger.Info("using Kubernetes service-proxy machine endpoint", "endpointURL", cfg.Agent.MachineClient.EndpointURL)
		return newClusterEndpointClient(cfg, logger, options.KubernetesRESTConfig)
	case cfg.Agent.MachineClient.Mode == config.MachineClientModeInCluster:
		logger.Info("using Kubernetes service-proxy machine endpoint with bootstrap credentials", "endpointURL", cfg.Agent.MachineClient.EndpointURL)
		return newClusterEndpointClientFromBootstrapConfig(cfg, logger)
	case cfg.Agent.MachineClient.EndpointURL != "":
		// TODO: Deprecate and remove the ARM proxy client once the in-cluster endpoint is the default.
		logger.Warn("using ARM proxy machine client for dev-test")
		return newARMProxyClient(cfg, logger)
	default:
		return newARMClient(cfg, logger)
	}
}
