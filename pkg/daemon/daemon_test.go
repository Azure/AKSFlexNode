package daemon

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestBootstrapCredentialRESTConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ServerURL = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))
	cfg.Azure.BootstrapToken = &config.BootstrapTokenConfig{Token: "token.value"}

	restCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		t.Fatalf("bootstrapCredentialRESTConfig: %v", err)
	}
	if restCfg.Host != cfg.Node.Kubelet.ServerURL || restCfg.BearerToken != cfg.Azure.BootstrapToken.Token {
		t.Fatalf("rest config = %#v", restCfg)
	}
}

func TestBootstrapCredentialRESTConfigExecCredential(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ServerURL = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))
	cfg.Azure.ServicePrincipal = &config.ServicePrincipalConfig{TenantID: "tenant", ClientID: "client", ClientSecret: "secret"}
	cfg.Kubernetes.Version = "1.34.0"

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
	cfg.Node.Kubelet.ServerURL = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))

	_, err := bootstrapCredentialRESTConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "exec credential") {
		t.Fatalf("error = %v, want credential error", err)
	}
}
