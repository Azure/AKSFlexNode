package config

import (
	"bytes"
	"testing"
)

func TestToAgentConfigUsesVerbatimKubeletKubeconfig(t *testing.T) {
	t.Parallel()

	kubeletKubeconfig := embeddedKubeconfigWithTLS(t) + "\n# kubelet identity\n"
	cfg := &Config{
		Agent: AgentConfig{KubeconfigData: validEmbeddedKubeconfig},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				KubeconfigData:       kubeletKubeconfig,
				ClusterFQDN:          "stale.example",
				CACertData:           "stale-ca",
				ImageGCHighThreshold: 80,
				ImageGCLowThreshold:  70,
			},
		},
	}
	if err := cfg.Node.Kubelet.validate(); err != nil {
		t.Fatalf("KubeletConfig.validate() error = %v", err)
	}

	agentCfg := ToAgentConfig(cfg, "kube1")
	if !bytes.Equal(agentCfg.Kubelet.KubeconfigData, []byte(kubeletKubeconfig)) {
		t.Fatal("kubelet kubeconfig was not preserved verbatim")
	}
	if agentCfg.Kubelet.Auth.BootstrapToken != "" {
		t.Fatal("bootstrap token was configured with embedded kubeconfigs")
	}
	if agentCfg.Kubelet.Auth.ExecCredential != nil {
		t.Fatal("generated exec credential was configured with embedded kubeconfigs")
	}
	if agentCfg.Cluster.CaCertBase64 == "" || agentCfg.Cluster.CaCertBase64 == "stale-ca" ||
		agentCfg.Cluster.CaCertBase64 != cfg.Node.Kubelet.CACertData {
		t.Fatal("Cluster.CaCertBase64 did not use the active kubeconfig CA")
	}
	if agentCfg.Kubelet.ApiServer != "https://cluster.example:443" {
		t.Fatalf("Kubelet.ApiServer = %q, want active kubeconfig server", agentCfg.Kubelet.ApiServer)
	}
}
