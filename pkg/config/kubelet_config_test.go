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

			err := tt.config.validate()
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
