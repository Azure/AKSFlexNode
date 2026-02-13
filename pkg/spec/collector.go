package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
)

// ManagedClusterClient is the subset of the Azure SDK managed clusters client we need.
// It exists to allow lightweight mocking in unit tests.
type ManagedClusterClient interface {
	Get(ctx context.Context, resourceGroupName, resourceName string, options *armcontainerservice.ManagedClustersClientGetOptions) (armcontainerservice.ManagedClustersClientGetResponse, error)
}

// ManagedClusterSpecEnricher enriches the collected spec snapshot based on the managed cluster response.
// It enables adding more spec signals in the future without changing the collector control-flow.
type ManagedClusterSpecEnricher func(spec *ManagedClusterSpec, resp armcontainerservice.ManagedClustersClientGetResponse) error

// ManagedClusterSpecCollector collects spec from the target AKS managed cluster
// and persists it to a local file for later checking.
type ManagedClusterSpecCollector struct {
	cfg    *config.Config
	logger *logrus.Logger

	authProvider *auth.AuthProvider
	client       ManagedClusterClient

	outputPath string

	enrichers []ManagedClusterSpecEnricher
}

// NewManagedClusterSpecCollector creates a collector that writes to the default spec path.
// The Azure client is created lazily on the first Collect call.
func NewManagedClusterSpecCollector(cfg *config.Config, logger *logrus.Logger) *ManagedClusterSpecCollector {
	c := &ManagedClusterSpecCollector{
		cfg:          cfg,
		logger:       logger,
		authProvider: auth.NewAuthProvider(),
		outputPath:   GetManagedClusterSpecFilePath(),
	}
	// Keep KubernetesVersion, fqdn required for now; more enrichers can be added over time.
	c.enrichers = []ManagedClusterSpecEnricher{enrichKubernetesVersionRequired, enrichFQDNRequired}
	return c
}

// NewManagedClusterSpecCollectorWithClient allows injecting a ManagedClusterClient and output path (primarily for tests).
func NewManagedClusterSpecCollectorWithClient(cfg *config.Config, logger *logrus.Logger, client ManagedClusterClient, outputPath string) *ManagedClusterSpecCollector {
	c := NewManagedClusterSpecCollector(cfg, logger)
	c.client = client
	if outputPath != "" {
		c.outputPath = outputPath
	}
	return c
}

// AddEnricher appends a spec enricher.
func (c *ManagedClusterSpecCollector) AddEnricher(enricher ManagedClusterSpecEnricher) {
	if c == nil || enricher == nil {
		return
	}
	c.enrichers = append(c.enrichers, enricher)
}

// Collect queries the Azure managed cluster resource to retrieve a spec snapshot.
// It writes a JSON payload to the configured output path and returns the collected spec.
func (c *ManagedClusterSpecCollector) Collect(ctx context.Context) (*ManagedClusterSpec, error) {
	if c == nil {
		return nil, fmt.Errorf("collector is nil")
	}
	if c.cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if c.logger == nil {
		c.logger = logrus.New()
	}

	clusterName := c.cfg.GetTargetClusterName()
	clusterRG := c.cfg.GetTargetClusterResourceGroup()
	if clusterName == "" || clusterRG == "" {
		return nil, fmt.Errorf("target cluster name/resourceGroup missing (name=%q, resourceGroup=%q)", clusterName, clusterRG)
	}

	if c.client == nil {
		subscriptionID := c.cfg.GetTargetClusterSubscriptionID()
		if subscriptionID == "" {
			subscriptionID = c.cfg.GetSubscriptionID()
		}
		if subscriptionID == "" {
			return nil, fmt.Errorf("subscription ID missing")
		}

		cred, err := c.authProvider.UserCredential(c.cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to get credential: %w", err)
		}

		mcClient, err := armcontainerservice.NewManagedClustersClient(subscriptionID, cred, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create managed clusters client: %w", err)
		}
		c.client = mcClient
	}

	c.logger.Infof("Collecting managed cluster spec for %s/%s", clusterRG, clusterName)
	resp, err := c.client.Get(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get AKS managed cluster via SDK: %w", err)
	}

	spec := &ManagedClusterSpec{
		SchemaVersion:     ManagedClusterSpecSchemaVersion,
		ClusterResourceID: c.cfg.GetTargetClusterID(),
		ClusterName:       clusterName,
		ResourceGroup:     clusterRG,
		CollectedAt:       time.Now().UTC(),
	}

	for _, enricher := range c.enrichers {
		if enricher == nil {
			continue
		}
		if err := enricher(spec, resp); err != nil {
			return nil, err
		}
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec JSON: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(c.outputPath), 0o750); err != nil {
		return nil, fmt.Errorf("failed to create spec output directory: %w", err)
	}
	if err := utilio.WriteFile(c.outputPath, data, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write managed cluster spec file: %w", err)
	}

	return spec, nil
}

func enrichKubernetesVersionRequired(spec *ManagedClusterSpec, resp armcontainerservice.ManagedClustersClientGetResponse) error {
	if spec == nil {
		return fmt.Errorf("spec is nil")
	}
	// set kubernetesVersion
	if resp.Properties == nil || resp.Properties.KubernetesVersion == nil || *resp.Properties.KubernetesVersion == "" {
		return fmt.Errorf("managed cluster kubernetesVersion is empty")
	}
	spec.KubernetesVersion = *resp.Properties.KubernetesVersion

	// set currentKubernetesVersion
	if resp.Properties == nil || resp.Properties.CurrentKubernetesVersion == nil || *resp.Properties.CurrentKubernetesVersion == "" {
		return fmt.Errorf("managed cluster currentKubernetesVersion is empty")
	}
	spec.CurrentKubernetesVersion = *resp.Properties.CurrentKubernetesVersion
	return nil
}

func enrichFQDNRequired(spec *ManagedClusterSpec, resp armcontainerservice.ManagedClustersClientGetResponse) error {
	if spec == nil {
		return fmt.Errorf("spec is nil")
	}
	if resp.Properties == nil || resp.Properties.Fqdn == nil || *resp.Properties.Fqdn == "" {
		return fmt.Errorf("managed cluster FQDN is empty")
	}
	spec.Fqdn = *resp.Properties.Fqdn
	return nil
}
