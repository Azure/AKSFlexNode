package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
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
		{name: "container unsupported", bootstrap: BootstrapConfig{Isolation: "container"}, wantErr: true},
		{name: "unknown", bootstrap: BootstrapConfig{Isolation: "invalid"}, wantErr: true},
		{name: "case sensitive", bootstrap: BootstrapConfig{Isolation: "NSpawn"}, wantErr: true},
		{name: "whitespace rejected", bootstrap: BootstrapConfig{Isolation: " nspawn "}, wantErr: true},
		{name: "none rejects rootfs", bootstrap: BootstrapConfig{Isolation: "none", OCIImage: "oci://rootfs"}, wantErr: true},
		{name: "none rejects mount remapping", bootstrap: BootstrapConfig{Isolation: "none", AdditionalHostMounts: []AdditionalHostMount{{Source: "/srv/data", Target: "/data"}}}, wantErr: true},
		{name: "none permits native mount", bootstrap: BootstrapConfig{Isolation: "none", AdditionalHostMounts: []AdditionalHostMount{{Source: "/srv/data", Target: "/srv/data"}}}, want: isolation.ModeNone},
		{name: "none permits omitted mount target", bootstrap: BootstrapConfig{Isolation: "none", AdditionalHostMounts: []AdditionalHostMount{{Source: "/srv/data"}}}, want: isolation.ModeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.bootstrap.validate(); (err != nil) != tc.wantErr {
				t.Fatalf("validate = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.bootstrap.Isolation != tc.want {
				t.Fatalf("stored mode = %q, want %q", tc.bootstrap.Isolation, tc.want)
			}
			if err := tc.bootstrap.validate(); err != nil {
				t.Fatalf("second validate: %v", err)
			}
			if tc.bootstrap.Isolation != tc.want {
				t.Fatalf("second validate changed mode to %q, want %q", tc.bootstrap.Isolation, tc.want)
			}
		})
	}
}

func TestLoadConfigIsolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		bootstrap string
		want      isolation.Mode
		wantErr   bool
	}{
		{name: "default", bootstrap: `{}`, want: isolation.ModeNSpawn},
		{name: "empty", bootstrap: `{"isolation":""}`, want: isolation.ModeNSpawn},
		{name: "nspawn", bootstrap: `{"isolation":"nspawn"}`, want: isolation.ModeNSpawn},
		{name: "none", bootstrap: `{"isolation":"none"}`, want: isolation.ModeNone},
		{name: "container with future opt in unsupported", bootstrap: `{"isolation":"container","experimentalContainerIsolation":true}`, wantErr: true},
		{name: "container unsupported", bootstrap: `{"isolation":"container"}`, wantErr: true},
		{name: "unknown", bootstrap: `{"isolation":"invalid"}`, wantErr: true},
		{name: "non-string", bootstrap: `{"isolation":42}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := fmt.Sprintf(`{
				"azure": {
					"managedIdentity": {},
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster"
					}
				},
				"agent": {"nodeName":"isolation-test-node"},
				"bootstrap": %s
			}`, tc.bootstrap)
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadConfig = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if cfg.Bootstrap.Isolation != tc.want {
				t.Fatalf("stored mode = %q, want %q", cfg.Bootstrap.Isolation, tc.want)
			}

			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var public struct {
				Bootstrap struct {
					Isolation string `json:"isolation"`
				} `json:"bootstrap"`
			}
			if err := json.Unmarshal(encoded, &public); err != nil {
				t.Fatal(err)
			}
			if public.Bootstrap.Isolation != string(tc.want) {
				t.Fatalf("JSON isolation = %q, want %q", public.Bootstrap.Isolation, tc.want)
			}
			copiedCfg := cfg.DeepCopy()
			if copiedCfg == nil || copiedCfg.Bootstrap.Isolation != tc.want {
				t.Fatal("deep copy lost isolation")
			}

			ac := ToAgentConfig(copiedCfg, "kube1")
			if ac.Isolation != string(tc.want) {
				t.Fatalf("isolation = %q, want %q", ac.Isolation, tc.want)
			}
			if ac.NSpawnLifecycle == nil {
				t.Fatal("adapter did not preserve nspawn lifecycle configuration")
			}
			want := flexNodeBinaryPath
			if tc.want == isolation.ModeNone {
				// Unlike host-agent clients, persisted kubelet credentials must not
				// resolve to a legacy directory that the host-root move removes.
				want = filepath.Join(hostroot.Path, hostBinaryRelativePath)
			}
			if ac.Kubelet.Auth.ExecCredential == nil || ac.Kubelet.Auth.ExecCredential.Command != want {
				t.Fatalf("exec plugin must use %s", want)
			}
		})
	}
}
