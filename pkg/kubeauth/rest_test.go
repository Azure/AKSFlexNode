package kubeauth

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

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

// In the agent's container, the caller's plugin is the image's agent, and the host has none: an
// agent kubeconfig that names the host's agent, as the documentation's does, runs the image's.
func TestBootstrapRESTConfigRunsThePluginWhereTheKubeconfigNamesTheHostAgent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	image := filepath.Join(dir, "image-agent")
	other := filepath.Join(dir, "other-plugin")
	for _, path := range []string{image, other} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]struct {
		command string
		plugin  string
		want    string
	}{
		"the host's agent as documented runs the plugin": {
			command: "/opt/unbounded/agent/bin/aks-flex-node",
			plugin:  image,
			want:    image,
		},
		"the host's agent as resolved runs the plugin": {
			command: config.HostBinaryPath(),
			plugin:  image,
			want:    image,
		},
		"another command runs as written": {
			command: other,
			plugin:  image,
			want:    other,
		},
		// On a host, the plugin is the host's agent, and the kubeconfig is used as written.
		"on a host, the host's agent runs as written": {
			command: config.HostBinaryPath(),
			plugin:  config.HostBinaryPath(),
			want:    config.HostBinaryPath(),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			kubeconfig := fmt.Sprintf(`apiVersion: v1
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
      command: %s
      args: ["token", "kubelogin"]
      interactiveMode: Never
contexts:
- name: default
  context:
    cluster: cluster
    user: agent
`, tc.command)
			cfg := &config.Config{
				Agent: config.AgentConfig{KubeconfigData: kubeconfig},
				Node:  config.NodeConfig{Kubelet: config.KubeletConfig{KubeconfigData: kubeconfig}},
			}
			restCfg, err := BootstrapRESTConfigWithPlugin(cfg, tc.plugin)
			// Where no agent is installed, as on a build machine, the host's agent isn't executable.
			if err != nil && strings.Contains(err.Error(), tc.want+" is not executable") {
				return
			}
			if err != nil {
				t.Fatalf("BootstrapRESTConfigWithPlugin() error = %v", err)
			}
			if restCfg.ExecProvider == nil || restCfg.ExecProvider.Command != tc.want {
				t.Fatalf("exec command = %#v, want %s", restCfg.ExecProvider, tc.want)
			}
		})
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

func TestBootstrapRESTConfigWithPlugin(t *testing.T) {
	t.Parallel()

	managedIdentity := func() *config.Config {
		return &config.Config{
			Azure: config.AzureConfig{
				ManagedIdentity: &config.ManagedIdentityConfig{ClientID: "mi-client-id"},
			},
			Node: config.NodeConfig{
				Kubelet: config.KubeletConfig{
					ClusterFQDN: "api.example.com:6443",
					CACertData:  "dGVzdC1jYS1kYXRh",
				},
			},
		}
	}
	bootstrapToken := func() *config.Config {
		cfg := managedIdentity()
		cfg.Azure.ManagedIdentity = nil
		cfg.Azure.BootstrapToken = &config.BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"}
		return cfg
	}

	// The plugin must be installed; a bootstrap token needs none, so it is not.
	tests := map[string]struct {
		cfg       func() *config.Config
		installed bool
		wantToken bool
	}{
		"a plugin replaces the kubelet's command": {cfg: managedIdentity, installed: true},
		"a bootstrap token needs no plugin":       {cfg: bootstrapToken, wantToken: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			plugin := filepath.Join(t.TempDir(), "aks-flex-node")
			if tc.installed {
				if err := os.WriteFile(plugin, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cfg := tc.cfg()
			got, err := BootstrapRESTConfigWithPlugin(cfg, plugin)
			if err != nil {
				t.Fatalf("BootstrapRESTConfigWithPlugin() error = %v", err)
			}
			if tc.wantToken {
				if got.BearerToken != cfg.Azure.BootstrapToken.Token || got.ExecProvider != nil {
					t.Fatalf("BootstrapRESTConfigWithPlugin() = token %q, exec provider %v; want the bootstrap token and no exec provider", got.BearerToken, got.ExecProvider)
				}
				return
			}

			kubelet := config.ToAgentConfig(cfg, cfg.Agent.NodeName).Kubelet.Auth.ExecCredential
			if got.ExecProvider == nil {
				t.Fatal("BootstrapRESTConfigWithPlugin() has no exec provider")
			}
			if got.ExecProvider.Command != plugin {
				t.Errorf("exec provider command = %q, want %q", got.ExecProvider.Command, plugin)
			}
			if !slices.Equal(got.ExecProvider.Args, kubelet.Args) {
				t.Errorf("exec provider args = %v, want the kubelet's %v", got.ExecProvider.Args, kubelet.Args)
			}
			if !slices.Equal(sortedEnv(got.ExecProvider.Env), sortedEnv(kubelet.Env)) {
				t.Errorf("exec provider env = %v, want the kubelet's %v", got.ExecProvider.Env, kubelet.Env)
			}
		})
	}
}

// sortedEnv lists env as sorted NAME=VALUE pairs, since the kubelet's comes from a map.
func sortedEnv(env []clientcmdapi.ExecEnvVar) []string {
	pairs := make([]string, 0, len(env))
	for _, v := range env {
		pairs = append(pairs, v.Name+"="+v.Value)
	}
	slices.Sort(pairs)
	return pairs
}
