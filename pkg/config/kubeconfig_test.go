package config

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
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

func embeddedKubeconfigWithTLS(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "certificate.pem")
	writeTestClientCertificate(t, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test certificate: %v", err)
	}
	return strings.Replace(validEmbeddedKubeconfig, "Y2E=", base64.StdEncoding.EncodeToString(data), 1)
}

func TestValidateEmbeddedKubeconfig(t *testing.T) {
	t.Parallel()

	validEmbeddedKubeconfig := embeddedKubeconfigWithTLS(t)
	_, caData, _ := strings.Cut(validEmbeddedKubeconfig, "certificate-authority-data: ")
	caData, _, _ = strings.Cut(caData, "\n")
	caLine := "certificate-authority-data: " + caData
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
				caLine,
				"certificate-authority: /etc/kubernetes/ca.crt",
				1,
			),
			wantErr: "certificate-authority file references",
		},
		"agent can use system roots": {
			data: strings.Replace(validEmbeddedKubeconfig, "    "+caLine+"\n", "", 1),
		},
		"trailing server slash": {
			data: strings.Replace(validEmbeddedKubeconfig, "https://cluster.example:443", "https://cluster.example:443/", 1),
		},
		"server path": {
			data:    strings.Replace(validEmbeddedKubeconfig, "https://cluster.example:443", "https://cluster.example:443/path", 1),
			wantErr: "must not include a path",
		},
		"encoded server slash": {
			data:    strings.Replace(validEmbeddedKubeconfig, "https://cluster.example:443", "https://cluster.example:443/%2f", 1),
			wantErr: "must not include a path",
		},
		"unused cluster without CA": {
			data: strings.Replace(validEmbeddedKubeconfig, "users:\n",
				"- name: unused\n  cluster:\n    server: https://unused.example:443\nusers:\n", 1),
		},
		"malformed CA": {
			data:    strings.Replace(validEmbeddedKubeconfig, caLine, "certificate-authority-data: Y2E=", 1),
			wantErr: "validate kubeconfig TLS material",
		},
		"malformed client certificate": {
			data: strings.Replace(
				validEmbeddedKubeconfig,
				"    exec:\n",
				"    client-certificate-data: bmFk\n    client-key-data: bmFk\n    exec:\n",
				1,
			),
			wantErr: "validate kubeconfig TLS material",
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
		"unused cluster file reference": {
			data: strings.Replace(
				validEmbeddedKubeconfig,
				"users:\n",
				"- name: unused\n  cluster:\n    server: https://unused.example:443\n    certificate-authority: /run/unused-ca.crt\nusers:\n",
				1,
			),
			wantErr: "certificate-authority file references",
		},
		"unused user file reference": {
			data: strings.Replace(
				validEmbeddedKubeconfig,
				"contexts:\n",
				"- name: unused\n  user:\n    tokenFile: /run/unused-token\ncontexts:\n",
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

	validEmbeddedKubeconfig := embeddedKubeconfigWithTLS(t)
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
			NodeName:       "flex-node",
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

func TestValidatePairedKubeconfigsWithoutAzureIdentity(t *testing.T) {
	t.Parallel()

	kubeconfig := embeddedKubeconfigWithTLS(t)
	cfg := &Config{
		Azure: AzureConfig{
			TargetCluster: &TargetClusterConfig{
				ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
				Location:   "eastus",
			},
		},
		Agent: AgentConfig{NodeName: "flex-node", KubeconfigData: kubeconfig},
		Node:  NodeConfig{Kubelet: KubeletConfig{KubeconfigData: kubeconfig}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
}

func TestLoadInvalidKubeconfigReturnsValidationError(t *testing.T) {
	t.Parallel()

	valid := embeddedKubeconfigWithTLS(t)
	withoutCA, err := clientcmd.Load([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	withoutCA.Clusters["cluster"].CertificateAuthorityData = nil
	withoutCAData, err := clientcmd.Write(*withoutCA)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		field string
		data  string
	}{
		{name: "malformed agent", field: "agent", data: "apiVersion: ["},
		{name: "malformed kubelet", field: "kubelet", data: "apiVersion: ["},
		{name: "whitespace agent", field: "agent", data: "\n"},
		{name: "whitespace kubelet", field: "kubelet", data: "\n"},
		{name: "missing kubelet CA", field: "kubelet", data: string(withoutCAData)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{
				Azure: AzureConfig{
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{KubeconfigData: valid},
				Node:  NodeConfig{Kubelet: KubeletConfig{KubeconfigData: valid}},
			}
			if tt.field == "agent" {
				cfg.Agent.KubeconfigData = tt.data
			} else {
				cfg.Node.Kubelet.KubeconfigData = tt.data
			}
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = LoadConfig(path)
			var validationErr *KubeconfigValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("LoadConfig() error = %v, want kubeconfig validation error", err)
			}
		})
	}
}

func TestLoadPairedKubeconfigConsistency(t *testing.T) {
	t.Parallel()

	base := embeddedKubeconfigWithTLS(t)
	other := embeddedKubeconfigWithTLS(t)
	otherConfig, err := clientcmd.Load([]byte(other))
	if err != nil {
		t.Fatal(err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	otherCA := otherConfig.Clusters["cluster"].CertificateAuthorityData
	tests := map[string]struct {
		configure func(*Config, *clientcmdapi.Config, *clientcmdapi.Config)
		wantErr   string
	}{
		"matching pair": {},
		"node name mismatch": {
			configure: func(cfg *Config, _, _ *clientcmdapi.Config) { cfg.Agent.NodeName = "different-node" },
			wantErr:   "must match system:node:different-node",
		},
		"non-node impersonation": {
			configure: func(_ *Config, _, kubelet *clientcmdapi.Config) {
				kubelet.AuthInfos["identity"].Impersonate = "other-user"
			},
			wantErr: "kubelet impersonation user must match",
		},
		"without impersonation": {
			configure: func(_ *Config, _, kubelet *clientcmdapi.Config) {
				kubelet.AuthInfos["identity"].Impersonate = ""
				kubelet.AuthInfos["identity"].ImpersonateGroups = nil
			},
		},
		"resolved hostname": {
			configure: func(cfg *Config, _, kubelet *clientcmdapi.Config) {
				cfg.Agent.NodeName = ""
				kubelet.AuthInfos["identity"].Impersonate = "system:node:" + strings.ToLower(strings.TrimSpace(hostname))
			},
		},
		"server mismatch": {
			configure: func(_ *Config, agent, _ *clientcmdapi.Config) {
				agent.Clusters["cluster"].Server = "https://different.example:443"
			},
			wantErr: "must use the same API server",
		},
		"trailing slash": {
			configure: func(_ *Config, _, kubelet *clientcmdapi.Config) {
				kubelet.Clusters["cluster"].Server += "/"
			},
		},
		"CA mismatch": {
			configure: func(_ *Config, agent, _ *clientcmdapi.Config) {
				agent.Clusters["cluster"].CertificateAuthorityData = otherCA
			},
			wantErr: "must use the same certificate authority",
		},
		"missing agent CA": {
			configure: func(_ *Config, agent, _ *clientcmdapi.Config) {
				agent.Clusters["cluster"].CertificateAuthorityData = nil
			},
			wantErr: "must use the same certificate authority",
		},
		"CA bundle order and formatting": {
			configure: func(_ *Config, agent, kubelet *clientcmdapi.Config) {
				ca := agent.Clusters["cluster"].CertificateAuthorityData
				agent.Clusters["cluster"].CertificateAuthorityData = append(append([]byte("\n"), ca...), otherCA...)
				kubelet.Clusters["cluster"].CertificateAuthorityData = append(append([]byte("\n\n"), otherCA...), ca...)
			},
		},
		"unused cluster without CA": {
			configure: func(_ *Config, agent, kubelet *clientcmdapi.Config) {
				agent.Clusters["unused"] = &clientcmdapi.Cluster{Server: "https://agent-unused.example"}
				kubelet.Clusters["unused"] = &clientcmdapi.Cluster{Server: "https://kubelet-unused.example"}
			},
		},
		"selected context mismatch": {
			configure: func(_ *Config, agent, _ *clientcmdapi.Config) {
				agent.Clusters["selected"] = &clientcmdapi.Cluster{
					Server:                   "https://different.example",
					CertificateAuthorityData: agent.Clusters["cluster"].CertificateAuthorityData,
				}
				agent.Contexts["selected"] = &clientcmdapi.Context{Cluster: "selected", AuthInfo: "identity"}
				agent.CurrentContext = "selected"
			},
			wantErr: "must use the same API server",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			agent, err := clientcmd.Load([]byte(base))
			if err != nil {
				t.Fatal(err)
			}
			kubelet, err := clientcmd.Load([]byte(base))
			if err != nil {
				t.Fatal(err)
			}
			cfg := &Config{
				Azure: AzureConfig{TargetCluster: &TargetClusterConfig{
					ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					Location:   "eastus",
				}},
				Agent: AgentConfig{NodeName: "flex-node"},
			}
			if tt.configure != nil {
				tt.configure(cfg, agent, kubelet)
			}
			agentData, err := clientcmd.Write(*agent)
			if err != nil {
				t.Fatal(err)
			}
			kubeletData, err := clientcmd.Write(*kubelet)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Agent.KubeconfigData = string(agentData)
			cfg.Node.Kubelet.KubeconfigData = string(kubeletData)
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if tt.wantErr != "" {
				var validationErr *KubeconfigValidationError
				if !errors.As(err, &validationErr) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadConfig() error = %v, want permanent error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if err := loaded.validate(); err != nil {
				t.Fatalf("repeated validation error = %v", err)
			}
			if loaded.Agent.KubeconfigData != string(agentData) || loaded.Node.Kubelet.KubeconfigData != string(kubeletData) {
				t.Fatal("validation changed the embedded kubeconfigs")
			}
			if loaded.Node.Kubelet.ClusterFQDN != "https://cluster.example:443" {
				t.Fatalf("ClusterFQDN = %q, want normalized active server", loaded.Node.Kubelet.ClusterFQDN)
			}
		})
	}
}

func TestValidateAgentKubeconfigRequiresAuthentication(t *testing.T) {
	t.Parallel()

	kubeletKubeconfig := embeddedKubeconfigWithTLS(t)
	tests := map[string]clientcmdapi.AuthInfo{
		"empty user":                {},
		"impersonation only":        {Impersonate: "flex-agent", ImpersonateGroups: []string{"flex-agents"}},
		"certificate without key":   {ClientCertificateData: []byte("certificate")},
		"username without password": {Username: "agent"},
	}
	for name, authInfo := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			agentKubeconfig, err := clientcmd.Load([]byte(kubeletKubeconfig))
			if err != nil {
				t.Fatalf("load test kubeconfig: %v", err)
			}
			agentKubeconfig.AuthInfos["identity"] = &authInfo
			data, err := clientcmd.Write(*agentKubeconfig)
			if err != nil {
				t.Fatalf("write test kubeconfig: %v", err)
			}
			cfg := &Config{
				Azure: AzureConfig{
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{KubeconfigData: string(data)},
				Node:  NodeConfig{Kubelet: KubeletConfig{KubeconfigData: kubeletKubeconfig}},
			}
			err = cfg.validate()
			if err == nil || !strings.Contains(err.Error(), "invalid agent.kubeconfigData: current context must provide authentication credentials") {
				t.Fatalf("validate() error = %v, want agent authentication error", err)
			}
		})
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
