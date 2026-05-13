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

func TestBootstrapCredentialRESTConfigRequiresBootstrapToken(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ServerURL = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))

	_, err := bootstrapCredentialRESTConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "bootstrap token") {
		t.Fatalf("error = %v, want bootstrap token error", err)
	}
}
