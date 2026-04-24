package config

import (
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// flexNodeBinaryPath is the path to the aks-flex-node binary inside
	// the nspawn rootfs. The bootstrapper copies the binary here before
	// starting the kubelet so that exec credential plugins can invoke it.
	flexNodeBinaryPath = "/usr/local/bin/aks-flex-node"

	// aksAADServerID is the Azure AD server application ID for AKS.
	aksAADServerID = "6dae42f8-4368-4678-94ff-3960e28e3630"
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
		CRI: agentconfig.CRIConfig{
			Containerd: agentconfig.ContainerdConfig{
				Version: cfg.Containerd.Version,
			},
			Runc: agentconfig.RuncConfig{
				Version: cfg.Runc.Version,
			},
		},
		CNI: agentconfig.CNIConfig{
			PluginVersion: cfg.CNI.Version,
		},
	}

	switch {
	case cfg.IsBootstrapTokenConfigured():
		ac.Kubelet.Auth.BootstrapToken = cfg.Azure.BootstrapToken.Token

	case cfg.IsSPConfigured():
		ac.Kubelet.Auth.ExecCredential = buildExecCredential(cfg, "spn", map[string]string{
			"AAD_LOGIN_METHOD":                    "spn",
			"AAD_SERVICE_PRINCIPAL_CLIENT_ID":     cfg.Azure.ServicePrincipal.ClientID,
			"AAD_SERVICE_PRINCIPAL_CLIENT_SECRET": cfg.Azure.ServicePrincipal.ClientSecret,
			"AZURE_TENANT_ID":                     cfg.Azure.ServicePrincipal.TenantID,
		})

	case cfg.IsMIConfigured():
		env := map[string]string{
			"AAD_LOGIN_METHOD": "msi",
		}
		if cfg.Azure.ManagedIdentity != nil && cfg.Azure.ManagedIdentity.ClientID != "" {
			env["AZURE_CLIENT_ID"] = cfg.Azure.ManagedIdentity.ClientID
		}
		ac.Kubelet.Auth.ExecCredential = buildExecCredential(cfg, "msi", env)
	}

	return ac
}

// buildExecCredential creates an ExecConfig that invokes the aks-flex-node
// binary as a credential plugin. The binary's `token kubelogin` subcommand
// uses kubelogin to obtain an Azure AD token for the AKS API server.
func buildExecCredential(_ *Config, _ string, env map[string]string) *clientcmdapi.ExecConfig {
	execEnv := make([]clientcmdapi.ExecEnvVar, 0, len(env))
	for k, v := range env {
		execEnv = append(execEnv, clientcmdapi.ExecEnvVar{Name: k, Value: v})
	}

	return &clientcmdapi.ExecConfig{
		APIVersion:         "client.authentication.k8s.io/v1",
		Command:            flexNodeBinaryPath,
		Args:               []string{"token", "kubelogin", "--server-id", aksAADServerID},
		Env:                execEnv,
		InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
		ProvideClusterInfo: false,
	}
}
