package azclient

import (
	"strings"
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
			wantEndpoint:  strings.TrimRight(cloud.AzureGovernment.Services[cloud.ResourceManager].Endpoint, "/"),
			wantAudience:  strings.TrimRight(cloud.AzureGovernment.Services[cloud.ResourceManager].Audience, "/"),
			wantAuthority: cloud.AzureGovernment.ActiveDirectoryAuthorityHost,
			wantScope:     strings.TrimRight(cloud.AzureGovernment.Services[cloud.ResourceManager].Audience, "/") + "/.default",
			wantAzcmagent: azcmagentAzureGovernmentCloud,
		},
		{
			name:          "azure china endpoint",
			endpoint:      "https://management.chinacloudapi.cn/",
			wantEndpoint:  strings.TrimRight(cloud.AzureChina.Services[cloud.ResourceManager].Endpoint, "/"),
			wantAudience:  strings.TrimRight(cloud.AzureChina.Services[cloud.ResourceManager].Audience, "/"),
			wantAuthority: cloud.AzureChina.ActiveDirectoryAuthorityHost,
			wantScope:     strings.TrimRight(cloud.AzureChina.Services[cloud.ResourceManager].Audience, "/") + "/.default",
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
	wantService := cloud.AzureGovernment.Services[cloud.ResourceManager]
	if service.Endpoint != strings.TrimRight(wantService.Endpoint, "/") {
		t.Fatalf("ResourceManager endpoint = %q, want %q", service.Endpoint, strings.TrimRight(wantService.Endpoint, "/"))
	}
	if service.Audience != strings.TrimRight(wantService.Audience, "/") {
		t.Fatalf("ResourceManager audience = %q, want %q", service.Audience, strings.TrimRight(wantService.Audience, "/"))
	}
	if opts.Cloud.ActiveDirectoryAuthorityHost != cloud.AzureGovernment.ActiveDirectoryAuthorityHost {
		t.Fatalf("authority host = %q, want Azure Government", opts.Cloud.ActiveDirectoryAuthorityHost)
	}
}
