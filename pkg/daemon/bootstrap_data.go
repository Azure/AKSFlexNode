package daemon

import (
	"context"
	"fmt"

	"github.com/Azure/AKSFlexNode/pkg/azclient"
	"github.com/Azure/AKSFlexNode/pkg/bootstrapdata"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

type bootstrapDataRefresher interface {
	Fetch(context.Context, *config.Config) (*bootstrapdata.Data, error)
}

type noopBootstrapDataRefresher struct{}

func (noopBootstrapDataRefresher) Fetch(context.Context, *config.Config) (*bootstrapdata.Data, error) {
	return nil, nil
}

type aksBootstrapDataRefresher struct{}

func (aksBootstrapDataRefresher) Fetch(ctx context.Context, cfg *config.Config) (*bootstrapdata.Data, error) {
	options, err := bootstrapDataOptionsFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return bootstrapdata.Fetch(ctx, options)
}

func bootstrapDataOptionsFromConfig(cfg *config.Config) (bootstrapdata.Options, error) {
	if cfg == nil || cfg.Azure.TargetCluster == nil {
		return bootstrapdata.Options{}, fmt.Errorf("target AKS cluster is not configured")
	}
	environment := azclient.ResourceManagerEnvironmentFromConfig(cfg)
	options := bootstrapdata.Options{
		ClusterResourceID:       cfg.Azure.TargetCluster.ResourceID,
		AgentPoolName:           cfg.Azure.TargetAgentPoolName,
		ResourceManagerEndpoint: environment.Endpoint,
		AuthorityHost:           environment.AuthorityHost,
		APIVersion:              bootstrapdata.DefaultAPIVersion,
	}

	switch {
	case cfg.IsMIConfigured():
		options.AuthMode = "managed-identity"
		options.MSIClientID = cfg.Azure.ManagedIdentity.ClientID
	case cfg.IsSPConfigured():
		options.AuthMode = "service-principal"
		options.SPTenantID = cfg.Azure.ServicePrincipal.TenantID
		options.SPClientID = cfg.Azure.ServicePrincipal.ClientID
		if cfg.Azure.ServicePrincipal.ClientSecretFile != "" {
			// Config validation leaves ClientSecretFile populated only when it
			// contains a certificate. Secret files are loaded into ClientSecret.
			options.SPClientCertificateFile = cfg.Azure.ServicePrincipal.ClientSecretFile
		} else {
			options.SPClientSecret = cfg.Azure.ServicePrincipal.ClientSecret
		}
	default:
		return bootstrapdata.Options{}, fmt.Errorf("bootstrap-data refresh requires managed identity or service-principal authentication")
	}

	return options, nil
}

func bootstrapDataRefresherForConfig(cfg *config.Config) bootstrapDataRefresher {
	if cfg != nil && cfg.IsBootstrapTokenConfigured() && (cfg.IsMIConfigured() || cfg.IsSPConfigured()) {
		return aksBootstrapDataRefresher{}
	}
	return noopBootstrapDataRefresher{}
}
