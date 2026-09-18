package config

import (
	"bytes"
	"testing"
)

func TestToAgentConfigUsesVerbatimKubeletKubeconfig(t *testing.T) {
	t.Parallel()

	const kubeletKubeconfig = validEmbeddedKubeconfig + "\n# kubelet identity\n"
	cfg := &Config{
		Agent: AgentConfig{KubeconfigData: validEmbeddedKubeconfig},
		Node: NodeConfig{
			Kubelet: KubeletConfig{KubeconfigData: kubeletKubeconfig},
		},
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
}
