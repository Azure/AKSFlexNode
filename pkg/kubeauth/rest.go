package kubeauth

import (
	"encoding/base64"
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// BootstrapRESTConfig builds a Kubernetes REST config from the bootstrap or
// exec-credential material in the FlexNode config. It is used before the daemon
// client certificate is available, including during bootstrap Machine reads.
// An exec credential runs the host's aks-flex-node, config.HostBinaryPath.
func BootstrapRESTConfig(cfg *config.Config) (*rest.Config, error) {
	return BootstrapRESTConfigWithPlugin(cfg, config.HostBinaryPath())
}

// BootstrapRESTConfigWithPlugin is BootstrapRESTConfig with the exec
// credential plugin named by the caller.
func BootstrapRESTConfigWithPlugin(cfg *config.Config, plugin string) (*rest.Config, error) {
	if cfg.UsesKubeconfigCredentials() {
		restCfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(cfg.Agent.KubeconfigData))
		if err != nil {
			return nil, fmt.Errorf("build Kubernetes REST config from agent kubeconfig: %w", err)
		}
		transportCfg, err := restCfg.TransportConfig()
		if err != nil {
			return nil, fmt.Errorf("validate agent kubeconfig transport: %w", err)
		}
		if _, err := transport.TLSConfigFor(transportCfg); err != nil {
			return nil, fmt.Errorf("validate agent kubeconfig TLS material: %w", err)
		}
		return restCfg, nil
	}

	apiServerURL := cfg.APIServerURL()
	if apiServerURL == "" {
		return nil, fmt.Errorf("kubernetes API server URL is empty")
	}
	if cfg.Node.Kubelet.CACertData == "" {
		return nil, fmt.Errorf("kubernetes CA certificate data is empty")
	}
	caData, err := base64.StdEncoding.DecodeString(cfg.Node.Kubelet.CACertData)
	if err != nil {
		return nil, fmt.Errorf("decode Kubernetes CA certificate: %w", err)
	}
	restCfg := &rest.Config{
		Host: apiServerURL,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}
	if cfg.IsBootstrapTokenConfigured() {
		restCfg.BearerToken = cfg.Azure.BootstrapToken.Token
		return restCfg, nil
	}
	agentCfg := config.ToAgentConfig(cfg, cfg.Agent.NodeName)
	if agentCfg.Kubelet.Auth.ExecCredential == nil {
		return nil, fmt.Errorf("kubernetes client requires bootstrap token or exec credential")
	}
	// Here rather than at the first request, where client-go would report it
	// as a failed credential.
	if !utilio.IsExecutable(plugin) {
		return nil, fmt.Errorf("exec credential plugin %s is not installed; aks-flex-node start installs it under the host root", plugin)
	}
	restCfg.ExecProvider = agentCfg.Kubelet.Auth.ExecCredential.DeepCopy()
	// The agent config's credential is the kubelet's, and names the binary
	// inside the machine. This client runs on the host.
	restCfg.ExecProvider.Command = plugin
	return restCfg, nil
}
