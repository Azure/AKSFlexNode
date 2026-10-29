package kubeauth

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

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
