package config

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"runtime"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	// flexNodeBinaryPath is the path to the aks-flex-node binary inside
	// the nspawn rootfs. The start task copies the binary here before
	// starting the kubelet so that exec credential plugins can invoke it.
	flexNodeBinaryPath = "/usr/local/bin/aks-flex-node"

	// aksAADServerID is the Azure AD server application ID for AKS.
	aksAADServerID = "6dae42f8-4368-4678-94ff-3960e28e3630"
)

// ToAgentConfig converts a FlexNode Config to the shared agent library's
// AgentConfig. The resulting struct can be passed to goalstates.ResolveMachine
// to produce goal states for the nspawn-based bootstrap phases.
//
// cfg.Node.Kubelet.ClusterFQDN and cfg.Node.Kubelet.CACertData must be populated.
func ToAgentConfig(cfg *Config, machineName string) *agentconfig.AgentConfig {
	kubeletConfig := kubeletConfiguration(cfg)

	ac := &agentconfig.AgentConfig{
		MachineName:           machineName,
		NodeName:              cfg.Agent.NodeName,
		OCIImage:              cfg.Bootstrap.OCIImage,
		AdditionalHostDevices: cfg.Bootstrap.AdditionalHostDevices,
		AdditionalHostMounts:  cfg.Bootstrap.AdditionalHostMounts,
		Cluster: agentconfig.AgentClusterConfig{
			CaCertBase64: cfg.Node.Kubelet.CACertData,
			ClusterDNS:   cfg.Networking.DNSServiceIP,
			Version:      cfg.Components.Kubernetes,
		},
		Kubelet: agentconfig.AgentKubeletConfig{
			ApiServer:          cfg.APIServerURL(),
			NodeIP:             cfg.Node.Kubelet.NodeIP,
			Labels:             cfg.Node.Labels,
			RegisterWithTaints: cfg.Node.Taints,
			Configuration:      kubeletConfig,
		},
		CRI: agentconfig.CRIConfig{
			Containerd: agentconfig.ContainerdConfig{
				Version:      cfg.Components.Containerd,
				SandboxImage: cfg.Components.SandboxImage,
			},
			Runc: agentconfig.RuncConfig{
				Version: cfg.Components.Runc,
			},
		},
		CNI: agentconfig.CNIConfig{
			PluginVersion: cfg.Networking.CNIVersion,
		},
	}

	if profile := cfg.Networking.LocalDNS; profile != nil {
		corefile, _ := profile.CorefileTemplate() // Config validation runs before adaptation.
		ac.LocalDNS = &agentconfig.AgentLocalDNSConfig{
			Enabled:          profile.Enabled(),
			RequiredPlugins:  []string{"log", "nsid"},
			CorefileTemplate: corefile,
		}
	}

	if provider := cfg.Node.Kubelet.ImageCredentialProvider; provider != nil {
		ac.Kubelet.ImageCredentialProvider = &agentconfig.ImageCredentialProvider{
			ConfigPath: provider.ConfigPath,
			BinDir:     provider.BinDir,
		}
	}

	if cfg.Bootstrap.OfflineArtifacts.Source != "" {
		ac.OfflineArtifacts = &agentconfig.AgentOfflineArtifacts{
			Source: cfg.Bootstrap.OfflineArtifacts.Source,
		}
	}

	if cfg.Components.Gantry != nil {
		ac.Gantry = &agentconfig.GantryConfig{
			Disabled: cfg.Components.Gantry.Disabled,
		}
	}

	switch {
	case cfg.IsBootstrapTokenConfigured():
		ac.Kubelet.Auth.BootstrapToken = cfg.Azure.BootstrapToken.Token

	case cfg.IsSPConfigured():
		env := map[string]string{
			"AAD_LOGIN_METHOD":                "spn",
			"AAD_SERVICE_PRINCIPAL_CLIENT_ID": cfg.Azure.ServicePrincipal.ClientID,
			"AZURE_TENANT_ID":                 cfg.Azure.ServicePrincipal.TenantID,
		}
		if cfg.Azure.ServicePrincipal.clientCertificateData() != "" {
			ac.Kubelet.Auth.ExecCredential = buildExecCredential(
				env,
				"--client-certificate-file",
				cfg.Azure.ServicePrincipal.ClientSecretFile,
			)
		} else {
			env["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"] = cfg.Azure.ServicePrincipal.ClientSecret
			ac.Kubelet.Auth.ExecCredential = buildExecCredential(env)
		}

	case cfg.IsMIConfigured():
		env := map[string]string{
			"AAD_LOGIN_METHOD": "msi",
		}
		if cfg.Azure.ManagedIdentity != nil && cfg.Azure.ManagedIdentity.ClientID != "" {
			env["AZURE_CLIENT_ID"] = cfg.Azure.ManagedIdentity.ClientID
		}
		ac.Kubelet.Auth.ExecCredential = buildExecCredential(env)
	}

	return ac
}

func kubeletConfiguration(cfg *Config) map[string]any {
	maxPods := cfg.Node.MaxPods
	if maxPods == 0 {
		maxPods = defaultMaxPods
	}

	return map[string]any{
		"maxPods":                     maxPods,
		"systemReserved":              systemReservedOrDefault(cfg),
		"kubeReserved":                kubeReservedOrDefault(cfg, maxPods),
		"imageGCHighThresholdPercent": cfg.Node.Kubelet.ImageGCHighThreshold,
		"imageGCLowThresholdPercent":  cfg.Node.Kubelet.ImageGCLowThreshold,
		"logging": map[string]any{
			"verbosity": cfg.Node.Kubelet.Verbosity,
		},
	}
}

// systemReservedOrDefault returns the configured system reservation, or the
// AKS default of zero CPU and memory when it is not overridden.
func systemReservedOrDefault(cfg *Config) map[string]string {
	if systemReserved := maps.Clone(cfg.Node.Kubelet.SystemReserved); systemReserved != nil {
		return systemReserved
	}

	return map[string]string{"cpu": "0", "memory": "0"}
}

// kubeReservedOrDefault returns the configured kube reservation, or the AKS
// defaults computed from the host resources and pod density.
func kubeReservedOrDefault(cfg *Config, maxPods int) map[string]string {
	if kubeReserved := maps.Clone(cfg.Node.Kubelet.KubeReserved); kubeReserved != nil {
		return kubeReserved
	}

	return defaultKubeReserved(runtime.NumCPU(), hostTotalMemoryMi(), maxPods)
}

// ResolveMachineGoalState converts FlexNode config to the shared agent config
// and resolves the nspawn machine goal state. Bootstrap and preflight both use
// this helper so preflight validates the same sources that bootstrap consumes.
func ResolveMachineGoalState(ctx context.Context, log *slog.Logger, cfg *Config, machineName string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	agentCfg := ToAgentConfig(cfg, machineName)
	downloads, containerImageArchives, err := goalstates.ResolveDownloadOverridesWithOfflineArtifacts(ctx, agentCfg, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve download overrides: %w", err)
	}
	gs, err := goalstates.ResolveMachine(log, agentCfg, machineName, downloads)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve machine goal state: %w", err)
	}

	return agentCfg, gs, containerImageArchives, nil
}

// buildExecCredential creates an ExecConfig that invokes the aks-flex-node
// binary as a credential plugin. The binary's `token kubelogin` subcommand
// uses kubelogin to obtain an Azure AD token for the AKS API server.
func buildExecCredential(env map[string]string, args ...string) *clientcmdapi.ExecConfig {
	execEnv := make([]clientcmdapi.ExecEnvVar, 0, len(env))
	for k, v := range env {
		execEnv = append(execEnv, clientcmdapi.ExecEnvVar{Name: k, Value: v})
	}
	execArgs := append([]string{"token", "kubelogin", "--server-id", aksAADServerID}, args...)

	return &clientcmdapi.ExecConfig{
		APIVersion:         "client.authentication.k8s.io/v1",
		Command:            flexNodeBinaryPath,
		Args:               execArgs,
		Env:                execEnv,
		InteractiveMode:    clientcmdapi.NeverExecInteractiveMode,
		ProvideClusterInfo: false,
	}
}
