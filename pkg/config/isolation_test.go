package config

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestBootstrapIsolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		bootstrap BootstrapConfig
		want      isolation.Mode
		wantErr   bool
	}{
		{name: "default", want: isolation.ModeNSpawn},
		{name: "nspawn", bootstrap: BootstrapConfig{Isolation: "nspawn"}, want: isolation.ModeNSpawn},
		{name: "none", bootstrap: BootstrapConfig{Isolation: "none"}, want: isolation.ModeNone},
		{name: "container not supported", bootstrap: BootstrapConfig{Isolation: "container"}, wantErr: true},
		{name: "unknown", bootstrap: BootstrapConfig{Isolation: "invalid"}, wantErr: true},
		{name: "none ignores no rootfs", bootstrap: BootstrapConfig{Isolation: "none", OCIImage: "oci://rootfs"}, wantErr: true},
		{name: "none rejects mount remapping", bootstrap: BootstrapConfig{Isolation: "none", AdditionalHostMounts: []AdditionalHostMount{{Source: "/srv/data", Target: "/data"}}}, wantErr: true},
		{name: "none permits native mount", bootstrap: BootstrapConfig{Isolation: "none", AdditionalHostMounts: []AdditionalHostMount{{Source: "/srv/data", Target: "/srv/data"}}}, want: isolation.ModeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.bootstrap.validate(); (err != nil) != tc.wantErr {
				t.Fatalf("validate = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && tc.bootstrap.IsolationMode() != tc.want {
				t.Fatalf("mode = %s, want %s", tc.bootstrap.IsolationMode(), tc.want)
			}
		})
	}
}

func TestIsolationAdapter(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nspawn", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Bootstrap: BootstrapConfig{Isolation: mode}, Azure: AzureConfig{ManagedIdentity: &ManagedIdentityConfig{}}}
			ac := ToAgentConfig(cfg, "kube1")
			if ac.Isolation != mode {
				t.Fatalf("isolation = %q", ac.Isolation)
			}
			if (ac.NSpawnLifecycle == nil) != (mode == "none") {
				t.Fatal("incorrect nspawn lifecycle selection")
			}
			want := flexNodeBinaryPath
			if mode == "none" {
				want = HostBinaryPath()
			}
			if ac.Kubelet.Auth.ExecCredential == nil || ac.Kubelet.Auth.ExecCredential.Command != want {
				t.Fatalf("exec plugin must use %s", want)
			}
			if cfg.DeepCopy().Bootstrap.Isolation != mode {
				t.Fatal("deep copy lost isolation")
			}
		})
	}
}
