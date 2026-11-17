package config

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIWorkerUsesGuestCredentialHelper(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Bootstrap: BootstrapConfig{Isolation: isolation.ModeContainer},
		Azure:     AzureConfig{ManagedIdentity: &ManagedIdentityConfig{}},
	}
	if err := cfg.Bootstrap.validate(); err != nil {
		t.Fatalf("OCI mode rejected: %v", err)
	}
	got := ToAgentConfig(cfg, "kube2")
	if got.Isolation != string(isolation.ModeContainer) || got.Experimental == nil || !got.Experimental.ContainerIsolation {
		t.Fatal("SDK OCI mode was not enabled")
	}
	if got.Kubelet.Auth.ExecCredential.Command != "/usr/local/bin/aks-flex-node" {
		t.Fatalf("credential helper = %q", got.Kubelet.Auth.ExecCredential.Command)
	}
}
