package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestConfigureKubeletDefaults(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cfg          config.KubeletConfig
		wantFile     bool
		wantContains []string
		wantErr      string
	}{
		"empty node ip skips file": {
			cfg: config.KubeletConfig{},
		},
		"writes ipv4 node ip": {
			cfg:      config.KubeletConfig{NodeIP: "10.247.1.4"},
			wantFile: true,
			wantContains: []string{
				"# Managed by aks-flex-node. Do not edit directly.",
				"KUBELET_EXTRA_ARGS=--node-ip=10.247.1.4",
			},
		},
		"normalizes ipv6 node ip": {
			cfg:      config.KubeletConfig{NodeIP: "2001:db8:0:0:0:0:0:1"},
			wantFile: true,
			wantContains: []string{
				"KUBELET_EXTRA_ARGS=--node-ip=2001:db8::1",
			},
		},
		"rejects invalid node ip": {
			cfg:     config.KubeletConfig{NodeIP: "eth0; rm -rf /"},
			wantErr: "node.kubelet.nodeIP",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			machineDir := t.TempDir()
			task := (&configureKubeletDefaultsTask{
				cfg:        tt.cfg,
				machineDir: machineDir,
			})
			if task.Name() != "configure-kubelet-defaults" {
				t.Fatalf("Name() = %q, want configure-kubelet-defaults", task.Name())
			}

			err := task.Do(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Do() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Do(): %v", err)
			}

			path := filepath.Join(machineDir, kubeletDefaultsPath)
			got, err := os.ReadFile(path)
			if !tt.wantFile {
				if !os.IsNotExist(err) {
					t.Fatalf("ReadFile(%s) error = %v, want not exist", path, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(string(got), want) {
					t.Fatalf("defaults file = %q, want containing %q", got, want)
				}
			}
		})
	}
}
