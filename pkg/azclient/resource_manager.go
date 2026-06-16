package azclient

import (
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const (
	azureGovernmentResourceManagerEndpoint = "https://management.usgovcloudapi.net"
	azureGovernmentResourceManagerAudience = "https://management.core.usgovcloudapi.net"
	azureChinaResourceManagerEndpoint      = "https://management.chinacloudapi.cn"
	azureChinaResourceManagerAudience      = "https://management.core.chinacloudapi.cn"

	azcmagentAzureGovernmentCloud = "AzureUSGovernment"
	azcmagentAzureChinaCloud      = "AzureChinaCloud"
)

// ResourceManagerEnvironment is the Azure SDK configuration derived from the
// configured ARM endpoint. The public cloud defaults preserve existing behavior;
// known sovereign endpoints switch authority host and token audience together.
type ResourceManagerEnvironment struct {
	Endpoint           string
	Audience           string
	AuthorityHost      string
	AzcmagentCloudName string
}

func ResourceManagerEnvironmentFromConfig(cfg *config.Config) ResourceManagerEnvironment {
	endpoint := config.DefaultResourceManagerEndpointURL
	if cfg != nil {
		if configured := strings.TrimSpace(cfg.Azure.ResourceManagerEndpointURL); configured != "" {
			endpoint = strings.TrimRight(configured, "/")
		}
	}

	env := ResourceManagerEnvironment{
		Endpoint:      endpoint,
		Audience:      strings.TrimRight(endpoint, "/"),
		AuthorityHost: cloud.AzurePublic.ActiveDirectoryAuthorityHost,
	}

	switch resourceManagerEndpointHost(endpoint) {
	case "management.azure.com":
		env.Audience = config.DefaultResourceManagerAudience
	case "management.usgovcloudapi.net":
		env.Endpoint = azureGovernmentResourceManagerEndpoint
		env.Audience = azureGovernmentResourceManagerAudience
		env.AuthorityHost = cloud.AzureGovernment.ActiveDirectoryAuthorityHost
		env.AzcmagentCloudName = azcmagentAzureGovernmentCloud
	case "management.chinacloudapi.cn":
		env.Endpoint = azureChinaResourceManagerEndpoint
		env.Audience = azureChinaResourceManagerAudience
		env.AuthorityHost = cloud.AzureChina.ActiveDirectoryAuthorityHost
		env.AzcmagentCloudName = azcmagentAzureChinaCloud
	}

	return env
}

func ClientOptionsFromConfig(cfg *config.Config) azcore.ClientOptions {
	env := ResourceManagerEnvironmentFromConfig(cfg)
	return azcore.ClientOptions{
		Cloud: cloud.Configuration{
			ActiveDirectoryAuthorityHost: env.AuthorityHost,
			Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
				cloud.ResourceManager: {
					Audience: env.Audience,
					Endpoint: env.Endpoint,
				},
			},
		},
	}
}

func ARMClientOptionsFromConfig(cfg *config.Config) *arm.ClientOptions {
	return &arm.ClientOptions{ClientOptions: ClientOptionsFromConfig(cfg)}
}

func ResourceManagerTokenScopeFromConfig(cfg *config.Config) string {
	env := ResourceManagerEnvironmentFromConfig(cfg)
	return strings.TrimRight(env.Audience, "/") + "/.default"
}

func AzcmagentCloudNameFromConfig(cfg *config.Config) string {
	return ResourceManagerEnvironmentFromConfig(cfg).AzcmagentCloudName
}

func resourceManagerEndpointHost(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err == nil && parsed.Host != "" {
		return strings.ToLower(parsed.Host)
	}
	return strings.ToLower(strings.Trim(endpoint, "/"))
}
