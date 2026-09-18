package daemon

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestBootstrapCredentialRESTConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ClusterFQDN = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))
	cfg.Azure.BootstrapToken = &config.BootstrapTokenConfig{Token: "token.value"}

	restCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		t.Fatalf("bootstrapCredentialRESTConfig: %v", err)
	}
	if restCfg.Host != cfg.Node.Kubelet.ClusterFQDN || restCfg.BearerToken != cfg.Azure.BootstrapToken.Token {
		t.Fatalf("rest config = %#v", restCfg)
	}
}

func TestBootstrapCredentialRESTConfigExecCredential(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ClusterFQDN = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))
	cfg.Azure.ServicePrincipal = &config.ServicePrincipalConfig{TenantID: "tenant", ClientID: "client", ClientSecret: "secret"}
	cfg.Components.Kubernetes = "1.34.0"

	restCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		t.Fatalf("bootstrapCredentialRESTConfig: %v", err)
	}
	if restCfg.ExecProvider == nil {
		t.Fatalf("ExecProvider = nil, want exec credential")
	}
	if restCfg.ExecProvider.Command != "/usr/local/bin/aks-flex-node" {
		t.Fatalf("ExecProvider.Command = %q", restCfg.ExecProvider.Command)
	}
}

func TestBootstrapCredentialRESTConfigRequiresCredential(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ClusterFQDN = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))

	_, err := bootstrapCredentialRESTConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "exec credential") {
		t.Fatalf("error = %v, want credential error", err)
	}
}

func TestDaemonRESTConfigEmbeddedKubeconfigBypassesBootstrapCredentials(t *testing.T) {
	t.Parallel()

	const kubeconfig = `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: cluster
  cluster:
    server: https://cluster.example:443
    certificate-authority-data: Y2E=
users:
- name: agent
  user:
    token: renewable-token
    as: flex-agent
contexts:
- name: default
  context:
    cluster: cluster
    user: agent
`
	cfg := &config.Config{
		Azure: config.AzureConfig{
			BootstrapToken: &config.BootstrapTokenConfig{Token: "legacy.bootstrap-token"},
		},
		Agent: config.AgentConfig{KubeconfigData: kubeconfig},
		Node: config.NodeConfig{
			Kubelet: config.KubeletConfig{KubeconfigData: kubeconfig},
		},
	}

	restCfg, stop, err := daemonRESTConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("daemonRESTConfig() error = %v", err)
	}
	defer stop()
	if restCfg.BearerToken != "renewable-token" {
		t.Fatalf("BearerToken = %q, want embedded kubeconfig token", restCfg.BearerToken)
	}
	if restCfg.Impersonate.UserName != "flex-agent" {
		t.Fatalf("Impersonate.UserName = %q", restCfg.Impersonate.UserName)
	}
}

func TestDaemonControllerCertificateOptions(t *testing.T) {
	t.Parallel()

	opts := daemonControllerCertificateOptions(filepath.Join(t.TempDir(), "creds"))
	if opts.Name != "aks-flex-node-daemon" {
		t.Fatalf("Name = %q, want aks-flex-node-daemon", opts.Name)
	}
	if opts.DaemonGroup != "aks-flex-node-daemons" {
		t.Fatalf("DaemonGroup = %q, want aks-flex-node-daemons", opts.DaemonGroup)
	}
	if opts.CredentialDir == "" {
		t.Fatal("CredentialDir is empty")
	}
	if opts.WaitTimeout == 0 {
		t.Fatal("WaitTimeout is empty")
	}
}

func TestRetireDaemonCredentialsAt(t *testing.T) {
	t.Parallel()

	credentialDir := filepath.Join(t.TempDir(), "daemon-credentials")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, "client.key"), []byte("private key"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := retireDaemonCredentialsAt(credentialDir); err != nil {
		t.Fatalf("retireDaemonCredentialsAt() error = %v", err)
	}
	if _, err := os.Stat(credentialDir); !os.IsNotExist(err) {
		t.Fatalf("credential directory still exists: %v", err)
	}
	if err := retireDaemonCredentialsAt(credentialDir); err != nil {
		t.Fatalf("second retireDaemonCredentialsAt() error = %v", err)
	}
}
