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
	case resourceManagerEndpointHost(resourceManagerService(cloud.AzurePublic).Endpoint):
		env.Audience = config.DefaultResourceManagerAudience
	case resourceManagerEndpointHost(resourceManagerService(cloud.AzureGovernment).Endpoint):
		env = resourceManagerEnvironmentFromCloud(cloud.AzureGovernment)
		env.AzcmagentCloudName = azcmagentAzureGovernmentCloud
	case resourceManagerEndpointHost(resourceManagerService(cloud.AzureChina).Endpoint):
		env = resourceManagerEnvironmentFromCloud(cloud.AzureChina)
		env.AzcmagentCloudName = azcmagentAzureChinaCloud
	}

	return env
}

func resourceManagerEnvironmentFromCloud(cfg cloud.Configuration) ResourceManagerEnvironment {
	service := resourceManagerService(cfg)
	return ResourceManagerEnvironment{
		Endpoint:      strings.TrimRight(service.Endpoint, "/"),
		Audience:      strings.TrimRight(service.Audience, "/"),
		AuthorityHost: cfg.ActiveDirectoryAuthorityHost,
	}
}

func resourceManagerService(cfg cloud.Configuration) cloud.ServiceConfiguration {
	return cfg.Services[cloud.ResourceManager]
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
