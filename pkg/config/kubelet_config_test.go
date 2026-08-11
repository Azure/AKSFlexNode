package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKubeletConfigJSON(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"imageCredentialProvider": {
			"configPath": "/etc/kubernetes/credential-provider.yaml",
			"binDir": "/usr/local/lib/kubelet-credential-providers"
		}
	}`)
	var config KubeletConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if config.ImageCredentialProvider == nil {
		t.Fatal("ImageCredentialProvider=nil, want provider")
	}
	if config.ImageCredentialProvider.ConfigPath != "/etc/kubernetes/credential-provider.yaml" || config.ImageCredentialProvider.BinDir != "/usr/local/lib/kubelet-credential-providers" {
		t.Fatalf("ImageCredentialProvider=%#v", config.ImageCredentialProvider)
	}
}

func TestKubeletConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		config  KubeletConfig
		wantErr string
	}{
		{name: "empty"},
		{
			name: "negative verbosity",
			config: KubeletConfig{
				Verbosity: -1,
			},
			wantErr: "verbosity must be between 0",
		},
		{
			name: "image GC high threshold above 100",
			config: KubeletConfig{
				ImageGCHighThreshold: 101,
			},
			wantErr: "imageGCHighThreshold must be between 0 and 100",
		},
		{
			name: "negative image GC low threshold",
			config: KubeletConfig{
				ImageGCLowThreshold: -1,
			},
			wantErr: "imageGCLowThreshold must be between 0 and 100",
		},
		{
			name: "image GC low threshold equals high threshold",
			config: KubeletConfig{
				ImageGCHighThreshold: 90,
				ImageGCLowThreshold:  90,
			},
			wantErr: "imageGCLowThreshold must be less than node.kubelet.imageGCHighThreshold",
		},
		{
			name: "image GC low threshold above high threshold",
			config: KubeletConfig{
				ImageGCHighThreshold: 80,
				ImageGCLowThreshold:  90,
			},
			wantErr: "imageGCLowThreshold must be less than node.kubelet.imageGCHighThreshold",
		},
		{
			name: "image credential provider",
			config: KubeletConfig{
				ImageCredentialProvider: &ImageCredentialProviderConfig{ConfigPath: "/etc/kubernetes/credential-provider.yaml",
					BinDir: "/usr/local/lib/kubelet-credential-providers",
				},
			},
		},
		{
			name: "missing provider config path",
			config: KubeletConfig{
				ImageCredentialProvider: &ImageCredentialProviderConfig{BinDir: "/usr/bin"},
			},
			wantErr: "ConfigPath",
		},
		{
			name: "relative provider binary directory",
			config: KubeletConfig{
				ImageCredentialProvider: &ImageCredentialProviderConfig{
					ConfigPath: "/etc/kubernetes/credential-provider.yaml",
					BinDir:     "bin",
				},
			},
			wantErr: "absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{Node: NodeConfig{Kubelet: tt.config}}
			cfg.setNodeDefaults()
			err := cfg.Node.Kubelet.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate error=%v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestNodeConfigValidateMaxPods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		maxPods int
		wantErr bool
	}{
		{name: "zero"},
		{name: "positive", maxPods: 110},
		{name: "negative", maxPods: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := NodeConfig{
				MaxPods: tt.maxPods,
				Kubelet: KubeletConfig{
					Verbosity:            2,
					ImageGCHighThreshold: 85,
					ImageGCLowThreshold:  80,
				},
			}
			err := cfg.validate()
			if tt.wantErr && err == nil {
				t.Fatal("validate succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}
