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
		cfg         config.KubeletConfig
		preContent  string
		wantContent string
		wantFile    bool
		wantErr     string
	}{
		"empty node ip skips file": {
			cfg: config.KubeletConfig{},
		},
		"empty node ip removes managed file": {
			cfg:        config.KubeletConfig{},
			preContent: kubeletDefaultsHeader + "KUBELET_EXTRA_ARGS=--node-ip=10.247.1.4\n",
		},
		"empty node ip preserves unmanaged file": {
			cfg:         config.KubeletConfig{},
			preContent:  "KUBELET_EXTRA_ARGS=--v=2\n",
			wantFile:    true,
			wantContent: "KUBELET_EXTRA_ARGS=--v=2\n",
		},
		"writes ipv4 node ip": {
			cfg:         config.KubeletConfig{NodeIP: "10.247.1.4"},
			wantFile:    true,
			wantContent: kubeletDefaultsHeader + "KUBELET_EXTRA_ARGS=--node-ip=10.247.1.4\n",
		},
		"normalizes ipv6 node ip": {
			cfg:         config.KubeletConfig{NodeIP: "2001:db8:0:0:0:0:0:1"},
			wantFile:    true,
			wantContent: kubeletDefaultsHeader + "KUBELET_EXTRA_ARGS=--node-ip=2001:db8::1\n",
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
			path := filepath.Join(machineDir, kubeletDefaultsPath)
			if tt.preContent != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(path, []byte(tt.preContent), 0o644); err != nil {
					t.Fatalf("WriteFile(%s): %v", path, err)
				}
			}

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
			if string(got) != tt.wantContent {
				t.Fatalf("defaults file = %q, want %q", got, tt.wantContent)
			}
		})
	}
}
