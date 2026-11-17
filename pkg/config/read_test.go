package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadConfigDecodesLegacyFieldsAndDefaultsWithoutValidation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
		"azure": {"arc": {"enabled": true}},
		"agent": {"logLevel": "invalid"},
		"kubernetes": {"version": "1.34.0"},
		"containerd": {"version": "2.0.0"},
		"cni": {"version": "1.6.0"},
		"node": {"kubelet": {"serverURL": "https://cluster.example", "dnsServiceIP": "10.5.0.10"}}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Components.Kubernetes != "1.34.0" || cfg.Components.Containerd != "2.0.0" || cfg.Networking.CNIVersion != "1.6.0" {
		t.Fatal("legacy component versions were not decoded")
	}
	if cfg.Networking.DNSServiceIP != "10.5.0.10" || cfg.Node.Kubelet.ClusterFQDN != "cluster.example" {
		t.Fatal("legacy networking settings were not decoded")
	}
	if cfg.Bootstrap.Isolation != DefaultIsolationMode || cfg.Node.MaxPods != defaultMaxPods || cfg.Components.Runc != "1.1.12" {
		t.Fatal("configuration defaults were not applied")
	}
	if !cfg.IsARCEnabled() || cfg.Agent.LogLevel != "invalid" || cfg.Agent.NodeName != "" {
		t.Fatal("ReadConfig validated or changed runtime intent")
	}
	if cfg.Agent.RequireMachineRegistration == nil || !*cfg.Agent.RequireMachineRegistration {
		t.Fatal("ARC registration default was not applied")
	}
	if _, err := LoadConfig(path); err == nil || !strings.HasPrefix(err.Error(), "config validation failed:") {
		t.Fatalf("LoadConfig error = %v, want runtime validation failure", err)
	}
}

func TestReadConfigDoesNotReadReferencedCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	credential := filepath.Join(dir, "missing-credential")
	path := filepath.Join(dir, "config.json")
	data := fmt.Sprintf(`{
		"azure": {
			"targetCluster": {
				"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster"
			},
			"servicePrincipal": {
				"tenantId": "tenant",
				"clientId": "client",
				"clientSecretFile": %q
			}
		},
		"agent": {"nodeName": "test-node"}
	}`, credential)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Azure.ServicePrincipal.ClientSecretFile != credential || cfg.Azure.ServicePrincipal.ClientSecret != "" {
		t.Fatal("ReadConfig changed the referenced credential")
	}
	if _, err := LoadConfig(path); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "clientSecretFile") {
		t.Fatalf("LoadConfig error = %v, want missing credential validation error", err)
	}
}

func TestReadConfigAndLoadConfigShareDefaults(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"", "/offline/artifacts.json"} {
		name := "online"
		if source != "" {
			name = "offline"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.json")
			data := fmt.Sprintf(`{
				"azure": {
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster"
					},
					"managedIdentity": {}
				},
				"agent": {"nodeName": "test-node"},
				"bootstrap": {"offlineArtifacts": {"source": %q}},
				"runc": {"version": "1.2.0"}
			}`, source)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}

			read, err := ReadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			// These derived resource coordinates belong to runtime validation.
			populateTargetClusterInfoFromConfig(read)
			if !reflect.DeepEqual(read, loaded) {
				t.Fatal("ReadConfig and LoadConfig applied different defaults")
			}
		})
	}
}

func TestConfigReadersShareDecodeErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		data string
		path string
		want string
	}{
		{
			name: "empty path",
			want: "config file path is required",
		},
		{
			name: "missing file",
			path: "missing.json",
			want: "failed to read config file",
		},
		{
			name: "invalid JSON",
			path: "config.json",
			data: `{`,
			want: "error unmarshaling config",
		},
		{
			name: "invalid legacy field",
			path: "config.json",
			data: `{"kubernetes": "invalid"}`,
			want: "adapt legacy config data",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := ""
			if tc.path != "" {
				path = filepath.Join(t.TempDir(), tc.path)
			}
			if tc.data != "" {
				if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			_, readErr := ReadConfig(path)
			_, loadErr := LoadConfig(path)
			if readErr == nil || loadErr == nil || readErr.Error() != loadErr.Error() || !strings.Contains(readErr.Error(), tc.want) {
				t.Fatalf("ReadConfig error = %v, LoadConfig error = %v, want %q", readErr, loadErr, tc.want)
			}
		})
	}
}
