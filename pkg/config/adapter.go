package config

import (
	agentconfig "github.com/Azure/unbounded/agent/config"
)

// ToAgentConfig converts a FlexNode Config to the shared agent library's
// AgentConfig. The resulting struct can be passed to goalstates.ResolveMachine
// to produce goal states for the nspawn-based bootstrap phases.
//
// The enrich-cluster-config step must have already run so that
// cfg.Node.Kubelet.ServerURL and cfg.Node.Kubelet.CACertData are populated.
func ToAgentConfig(cfg *Config, machineName string) *agentconfig.AgentConfig {
	ac := &agentconfig.AgentConfig{
		MachineName: machineName,
		Cluster: agentconfig.AgentClusterConfig{
			CaCertBase64: cfg.Node.Kubelet.CACertData,
			ClusterDNS:   cfg.Node.Kubelet.DNSServiceIP,
			Version:      cfg.Kubernetes.Version,
		},
		Kubelet: agentconfig.AgentKubeletConfig{
			ApiServer:          cfg.Node.Kubelet.ServerURL,
			Labels:             cfg.Node.Labels,
			RegisterWithTaints: cfg.Node.Taints,
		},
	}

	// TODO: support passing kubelet auth config (Arc, ServicePrincipal, ManagedIdentity)
	// into the shared agent library. Currently only bootstrap token maps directly.
	// Other auth modes will need the shared library to accept a kubeconfig or
	// credential provider interface.
	if cfg.IsBootstrapTokenConfigured() {
		ac.Kubelet.BootstrapToken = cfg.Azure.BootstrapToken.Token
	}

	return ac
}
