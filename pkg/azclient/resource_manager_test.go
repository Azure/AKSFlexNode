package azclient

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestResourceManagerEnvironmentFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		endpoint      string
		wantEndpoint  string
		wantAudience  string
		wantAuthority string
		wantScope     string
		wantAzcmagent string
	}{
		{
			name:          "public default",
			wantEndpoint:  config.DefaultResourceManagerEndpointURL,
			wantAudience:  config.DefaultResourceManagerAudience,
			wantAuthority: cloud.AzurePublic.ActiveDirectoryAuthorityHost,
			wantScope:     config.DefaultResourceManagerTokenScope,
		},
		{
			name:          "custom endpoint",
			endpoint:      "https://management.example.test/",
			wantEndpoint:  "https://management.example.test",
			wantAudience:  "https://management.example.test",
			wantAuthority: cloud.AzurePublic.ActiveDirectoryAuthorityHost,
			wantScope:     "https://management.example.test/.default",
		},
		{
			name:          "azure government endpoint",
			endpoint:      "https://management.usgovcloudapi.net/",
			wantEndpoint:  azureGovernmentResourceManagerEndpoint,
			wantAudience:  azureGovernmentResourceManagerAudience,
			wantAuthority: cloud.AzureGovernment.ActiveDirectoryAuthorityHost,
			wantScope:     azureGovernmentResourceManagerAudience + "/.default",
			wantAzcmagent: azcmagentAzureGovernmentCloud,
		},
		{
			name:          "azure china endpoint",
			endpoint:      "https://management.chinacloudapi.cn/",
			wantEndpoint:  azureChinaResourceManagerEndpoint,
			wantAudience:  azureChinaResourceManagerAudience,
			wantAuthority: cloud.AzureChina.ActiveDirectoryAuthorityHost,
			wantScope:     azureChinaResourceManagerAudience + "/.default",
			wantAzcmagent: azcmagentAzureChinaCloud,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{}
			cfg.Azure.ResourceManagerEndpointURL = tt.endpoint

			got := ResourceManagerEnvironmentFromConfig(cfg)
			if got.Endpoint != tt.wantEndpoint {
				t.Fatalf("Endpoint = %q, want %q", got.Endpoint, tt.wantEndpoint)
			}
			if got.Audience != tt.wantAudience {
				t.Fatalf("Audience = %q, want %q", got.Audience, tt.wantAudience)
			}
			if got.AuthorityHost != tt.wantAuthority {
				t.Fatalf("AuthorityHost = %q, want %q", got.AuthorityHost, tt.wantAuthority)
			}
			if scope := ResourceManagerTokenScopeFromConfig(cfg); scope != tt.wantScope {
				t.Fatalf("ResourceManagerTokenScopeFromConfig() = %q, want %q", scope, tt.wantScope)
			}
			if cloudName := AzcmagentCloudNameFromConfig(cfg); cloudName != tt.wantAzcmagent {
				t.Fatalf("AzcmagentCloudNameFromConfig() = %q, want %q", cloudName, tt.wantAzcmagent)
			}
		})
	}
}

func TestClientOptionsFromConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Azure.ResourceManagerEndpointURL = "https://management.usgovcloudapi.net/"

	opts := ClientOptionsFromConfig(cfg)
	service, ok := opts.Cloud.Services[cloud.ResourceManager]
	if !ok {
		t.Fatal("ResourceManager cloud service is missing")
	}
	if service.Endpoint != azureGovernmentResourceManagerEndpoint {
		t.Fatalf("ResourceManager endpoint = %q, want %q", service.Endpoint, azureGovernmentResourceManagerEndpoint)
	}
	if service.Audience != azureGovernmentResourceManagerAudience {
		t.Fatalf("ResourceManager audience = %q, want %q", service.Audience, azureGovernmentResourceManagerAudience)
	}
	if opts.Cloud.ActiveDirectoryAuthorityHost != cloud.AzureGovernment.ActiveDirectoryAuthorityHost {
		t.Fatalf("authority host = %q, want Azure Government", opts.Cloud.ActiveDirectoryAuthorityHost)
	}
}
