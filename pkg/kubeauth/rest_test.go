package kubeauth

import (
	"fmt"
	"os"
	"path/filepath"
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
      command: /opt/unbounded/agent/bin/aks-flex-node
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
	dir := t.TempDir()
	plugin := filepath.Join(dir, "host-credential")
	marker := filepath.Join(dir, "called")
	if err := os.WriteFile(plugin, fmt.Appendf(nil, "#!/bin/sh\ntouch %q\nexit 1\n", marker), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Agent: config.AgentConfig{KubeconfigData: strings.Replace(kubeconfig, config.HostBinaryPath(), plugin, 1)},
		Node: config.NodeConfig{
			Kubelet: config.KubeletConfig{KubeconfigData: kubeconfig},
		},
	}

	restCfg, err := BootstrapRESTConfigWithPlugin(cfg, filepath.Join(dir, "unused-legacy-override"))
	if err != nil {
		t.Fatalf("BootstrapRESTConfig() error = %v", err)
	}
	if restCfg.Host != "https://cluster.example:443" {
		t.Fatalf("Host = %q", restCfg.Host)
	}
	if restCfg.ExecProvider == nil || restCfg.ExecProvider.Command != plugin {
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
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host credential plugin ran while building REST config: %v", err)
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
