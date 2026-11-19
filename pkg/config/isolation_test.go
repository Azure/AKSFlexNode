package config

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestIsolationDefaultsAndCopy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		mode  isolation.Mode
		want  isolation.Mode
		valid bool
	}{
		{
			name:  "default",
			want:  isolation.ModeNSpawn,
			valid: true,
		},
		{
			name:  "nspawn",
			mode:  isolation.ModeNSpawn,
			want:  isolation.ModeNSpawn,
			valid: true,
		},
		{
			name:  "none",
			mode:  isolation.ModeNone,
			want:  isolation.ModeNone,
			valid: true,
		},
		{
			name: "container without experimental opt-in",
			mode: isolation.ModeContainer,
			want: isolation.ModeContainer,
		},
		{
			name: "unknown",
			mode: "unknown",
			want: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{
				Bootstrap: BootstrapConfig{Isolation: tc.mode},
			}
			copyBeforeDefaults := cfg.DeepCopy()
			if copyBeforeDefaults.Bootstrap.Isolation != tc.mode {
				t.Fatal("DeepCopy changed isolation")
			}
			for range 2 {
				cfg.setDefaults()
				if cfg.Bootstrap.Isolation != tc.want {
					t.Fatalf("mode = %q, want %q", cfg.Bootstrap.Isolation, tc.want)
				}
			}
			if err := cfg.Bootstrap.validate(); (err == nil) != tc.valid {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestNoneExecCredentialUsesHostAgent(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Bootstrap: BootstrapConfig{Isolation: isolation.ModeNone},
		Azure: AzureConfig{
			ManagedIdentity: &ManagedIdentityConfig{},
		},
	}
	got := ToAgentConfig(cfg, "kube1")
	if got.Isolation != string(isolation.ModeNone) || got.Kubelet.Auth.ExecCredential.Command != "/opt/unbounded/agent/bin/aks-flex-node" {
		t.Fatal("none credential helper does not use host agent")
	}
}
