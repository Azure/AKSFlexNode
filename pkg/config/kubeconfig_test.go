package config

import (
	"strings"
	"testing"
)

const validEmbeddedKubeconfig = `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: cluster
  cluster:
    server: https://cluster.example:443
    certificate-authority-data: Y2E=
users:
- name: identity
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /usr/local/bin/aks-flex-node
      args: ["token", "arc"]
      interactiveMode: Never
    as: system:node:flex-node
    as-groups:
    - system:nodes
contexts:
- name: default
  context:
    cluster: cluster
    user: identity
`

func TestValidateEmbeddedKubeconfig(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		data    string
		wantErr string
	}{
		"valid": {
			data: validEmbeddedKubeconfig,
		},
		"missing current context": {
			data:    strings.Replace(validEmbeddedKubeconfig, "current-context: default\n", "", 1),
			wantErr: "current context is empty",
		},
		"external CA": {
			data: strings.Replace(
				validEmbeddedKubeconfig,
				"certificate-authority-data: Y2E=",
				"certificate-authority: /etc/kubernetes/ca.crt",
				1,
			),
			wantErr: "certificate-authority file references",
		},
		"missing CA data": {
			data:    strings.Replace(validEmbeddedKubeconfig, "    certificate-authority-data: Y2E=\n", "", 1),
			wantErr: "certificate-authority-data is required",
		},
		"relative exec command": {
			data:    strings.Replace(validEmbeddedKubeconfig, "command: /usr/local/bin/aks-flex-node", "command: aks-flex-node", 1),
			wantErr: "exec command must be an absolute path",
		},
		"missing exec API version": {
			data:    strings.Replace(validEmbeddedKubeconfig, "      apiVersion: client.authentication.k8s.io/v1\n", "", 1),
			wantErr: "apiVersion must be specified",
		},
		"missing exec interactive mode": {
			data:    strings.Replace(validEmbeddedKubeconfig, "      interactiveMode: Never\n", "", 1),
			wantErr: "interactiveMode must be specified",
		},
		"external token file": {
			data: strings.Replace(
				validEmbeddedKubeconfig,
				"    exec:\n      apiVersion: client.authentication.k8s.io/v1\n      command: /usr/local/bin/aks-flex-node\n      args: [\"token\", \"arc\"]\n      interactiveMode: Never",
				"    tokenFile: /run/token",
				1,
			),
			wantErr: "token-file references",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := validateEmbeddedKubeconfig(tt.data)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateEmbeddedKubeconfig() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateEmbeddedKubeconfig() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAuthSettingsRequiresKubeconfigPair(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		agent   string
		kubelet string
		wantErr bool
	}{
		"neither": {},
		"both": {
			agent:   validEmbeddedKubeconfig,
			kubelet: validEmbeddedKubeconfig,
		},
		"agent only": {
			agent:   validEmbeddedKubeconfig,
			wantErr: true,
		},
		"kubelet only": {
			kubelet: validEmbeddedKubeconfig,
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{
				Azure: AzureConfig{
					Arc: &ArcConfig{Enabled: true},
				},
				Agent: AgentConfig{KubeconfigData: tt.agent},
				Node: NodeConfig{
					Kubelet: KubeletConfig{KubeconfigData: tt.kubelet},
				},
			}
			err := cfg.validateAuthSettings()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAuthSettings() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestUsesKubeconfigCredentials(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agent: AgentConfig{KubeconfigData: validEmbeddedKubeconfig},
		Node: NodeConfig{
			Kubelet: KubeletConfig{KubeconfigData: validEmbeddedKubeconfig},
		},
	}
	if !cfg.UsesKubeconfigCredentials() {
		t.Fatal("UsesKubeconfigCredentials() = false, want true")
	}
}

func TestValidateArcWithEmbeddedKubeconfigsDoesNotRequireBootstrapToken(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			SubscriptionID: "12345678-1234-1234-1234-123456789012",
			Arc:            &ArcConfig{Enabled: true},
			TargetCluster: &TargetClusterConfig{
				ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
				Location:   "eastus",
			},
		},
		Agent: AgentConfig{
			LogLevel:       "info",
			KubeconfigData: validEmbeddedKubeconfig,
		},
		Node: NodeConfig{
			Kubelet: KubeletConfig{KubeconfigData: validEmbeddedKubeconfig},
		},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestDeepCopyPreservesEmbeddedKubeconfigs(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agent: AgentConfig{KubeconfigData: validEmbeddedKubeconfig},
		Node: NodeConfig{
			Kubelet: KubeletConfig{KubeconfigData: validEmbeddedKubeconfig + "\n"},
		},
	}
	copy := cfg.DeepCopy()
	if copy == nil {
		t.Fatal("DeepCopy() = nil")
	}
	if copy.Agent.KubeconfigData != cfg.Agent.KubeconfigData {
		t.Fatal("agent kubeconfig changed during DeepCopy")
	}
	if copy.Node.Kubelet.KubeconfigData != cfg.Node.Kubelet.KubeconfigData {
		t.Fatal("kubelet kubeconfig changed during DeepCopy")
	}
}

func TestEmbeddedKubeconfigsBypassLegacyBootstrapTokenRequirements(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			Arc:            &ArcConfig{Enabled: true},
			BootstrapToken: &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
		Agent: AgentConfig{KubeconfigData: validEmbeddedKubeconfig},
		Node: NodeConfig{
			Kubelet: KubeletConfig{KubeconfigData: validEmbeddedKubeconfig},
		},
	}
	if err := cfg.validateBootstrapToken(); err != nil {
		t.Fatalf("validateBootstrapToken() error = %v", err)
	}
	if cfg.NeedsBootstrapDataRefresh() {
		t.Fatal("NeedsBootstrapDataRefresh() = true, want false")
	}
}
