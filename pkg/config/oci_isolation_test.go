package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIWorkerRequiresExperimentalOptIn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		data   string
		wantOK bool
	}{
		{
			name: "missing",
			data: `{"isolation":"container"}`,
		},
		{
			name: "disabled",
			data: `{"isolation":"container","experimental":{"containerIsolation":false}}`,
		},
		{
			name:   "enabled",
			data:   `{"isolation":"container","experimental":{"containerIsolation":true}}`,
			wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var bootstrap BootstrapConfig
			if err := json.Unmarshal([]byte(tc.data), &bootstrap); err != nil {
				t.Fatal(err)
			}

			err := bootstrap.validate()
			if (err == nil) != tc.wantOK {
				t.Fatalf("validation = %v, want success %t", err, tc.wantOK)
			}
			if err != nil && !strings.Contains(err.Error(), "bootstrap.experimental.containerIsolation") {
				t.Fatalf("validation error does not identify the opt-in: %v", err)
			}
		})
	}
}

func TestOCIWorkerUsesGuestCredentialHelper(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Bootstrap: BootstrapConfig{
			Isolation: isolation.ModeContainer,
			Experimental: &ExperimentalConfig{
				ContainerIsolation: true,
			},
		},
		Azure: AzureConfig{
			ManagedIdentity: &ManagedIdentityConfig{},
		},
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
