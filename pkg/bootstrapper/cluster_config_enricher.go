package bootstrapper

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"sigs.k8s.io/yaml"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

// EnrichClusterConfig populates cfg.Node.Kubelet.ServerURL and
// cfg.Node.Kubelet.CACertData from the AKS cluster admin credentials.
// It is a no-op when these fields are already set or when bootstrap token
// auth is configured (which requires them in the config file).
func EnrichClusterConfig(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	if cfg.Node.Kubelet.ServerURL != "" && cfg.Node.Kubelet.CACertData != "" {
		return nil
	}

	if cfg.IsBootstrapTokenConfigured() {
		return nil
	}

	logger.Info("fetching cluster admin credentials to populate server URL and CA cert data")

	cred, err := auth.NewAuthProvider().UserCredential(cfg)
	if err != nil {
		return fmt.Errorf("get credential: %w", err)
	}

	clusterSubID := cfg.Azure.TargetCluster.SubscriptionID
	mcClient, err := armcontainerservice.NewManagedClustersClient(clusterSubID, cred, nil)
	if err != nil {
		return fmt.Errorf("create managed clusters client: %w", err)
	}

	clusterRG := cfg.Azure.TargetCluster.ResourceGroup
	clusterName := cfg.Azure.TargetCluster.Name

	resp, err := mcClient.ListClusterAdminCredentials(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return fmt.Errorf("list cluster admin credentials for %s/%s: %w", clusterRG, clusterName, err)
	}

	if len(resp.Kubeconfigs) == 0 {
		return fmt.Errorf("no kubeconfig returned in cluster admin credentials response")
	}

	kubeconfig := resp.Kubeconfigs[0]
	if kubeconfig == nil || len(kubeconfig.Value) == 0 {
		return fmt.Errorf("kubeconfig value is empty in cluster admin credentials response")
	}

	serverURL, caCertData, err := extractClusterInfoFromKubeconfig(kubeconfig.Value)
	if err != nil {
		return fmt.Errorf("extract cluster info from kubeconfig: %w", err)
	}

	cfg.Node.Kubelet.ServerURL = serverURL
	cfg.Node.Kubelet.CACertData = caCertData
	logger.Info("cluster config enriched", "serverURL", serverURL)
	return nil
}

// minimalKubeconfig holds just the fields we need from an admin kubeconfig.
// sigs.k8s.io/yaml converts YAML to JSON first and then uses encoding/json,
// so json: tags (not yaml: tags) are required for correct field mapping.
type minimalKubeconfig struct {
	Clusters []struct {
		Cluster struct {
			Server                   string `json:"server"`
			CertificateAuthorityData string `json:"certificate-authority-data"`
		} `json:"cluster"`
	} `json:"clusters"`
}

// extractClusterInfoFromKubeconfig parses a kubeconfig YAML and returns the
// server URL and base64-encoded CA certificate data from the first cluster entry.
func extractClusterInfoFromKubeconfig(data []byte) (serverURL, caCertData string, err error) {
	var kc minimalKubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return "", "", fmt.Errorf("parse kubeconfig YAML: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return "", "", fmt.Errorf("no clusters found in kubeconfig")
	}
	cluster := kc.Clusters[0].Cluster
	if cluster.Server == "" {
		return "", "", fmt.Errorf("server URL is empty in kubeconfig")
	}
	return cluster.Server, cluster.CertificateAuthorityData, nil
}
