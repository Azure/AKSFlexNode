package kubeauth

import (
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestBootstrapRESTConfigEmbeddedAgentKubeconfig(t *testing.T) {
	t.Parallel()

	const kubeconfig = `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: cluster
  cluster:
    server: https://cluster.example:443
users:
- name: agent
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /usr/local/bin/aks-flex-node
      args: ["token", "arc"]
      interactiveMode: Never
    as: flex-agent
    as-groups: ["flex-agents"]
    as-user-extra:
      example.com/scope: ["machine"]
contexts:
- name: default
  context:
    cluster: cluster
    user: agent
`
	cfg := &config.Config{
		Agent: config.AgentConfig{KubeconfigData: kubeconfig},
		Node: config.NodeConfig{
			Kubelet: config.KubeletConfig{KubeconfigData: kubeconfig},
		},
	}

	restCfg, err := BootstrapRESTConfig(cfg)
	if err != nil {
		t.Fatalf("BootstrapRESTConfig() error = %v", err)
	}
	if restCfg.Host != "https://cluster.example:443" {
		t.Fatalf("Host = %q", restCfg.Host)
	}
	if restCfg.ExecProvider == nil || restCfg.ExecProvider.Command != "/usr/local/bin/aks-flex-node" {
		t.Fatalf("ExecProvider = %#v", restCfg.ExecProvider)
	}
	if restCfg.Impersonate.UserName != "flex-agent" {
		t.Fatalf("Impersonate.UserName = %q", restCfg.Impersonate.UserName)
	}
	if len(restCfg.Impersonate.Groups) != 1 || restCfg.Impersonate.Groups[0] != "flex-agents" {
		t.Fatalf("Impersonate.Groups = %#v", restCfg.Impersonate.Groups)
	}
	if got := restCfg.Impersonate.Extra["example.com/scope"]; len(got) != 1 || got[0] != "machine" {
		t.Fatalf("Impersonate.Extra = %#v", got)
	}
}

func TestBootstrapRESTConfigRejectsInvalidAgentTLSData(t *testing.T) {
	t.Parallel()

	const invalidTLSKubeconfig = `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: cluster
  cluster:
    server: https://cluster.example:443
users:
- name: agent
  user:
    client-certificate-data: bmFk
    client-key-data: bmFk
contexts:
- name: default
  context:
    cluster: cluster
    user: agent
`
	cfg := &config.Config{
		Agent: config.AgentConfig{KubeconfigData: invalidTLSKubeconfig},
		Node: config.NodeConfig{
			Kubelet: config.KubeletConfig{KubeconfigData: invalidTLSKubeconfig},
		},
	}

	_, err := BootstrapRESTConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "validate agent kubeconfig TLS material") {
		t.Fatalf("BootstrapRESTConfig() error = %v, want transport validation error", err)
	}
}
