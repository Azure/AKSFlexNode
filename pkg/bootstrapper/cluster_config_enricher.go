package bootstrapper

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/sirupsen/logrus"
	"sigs.k8s.io/yaml"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

// clusterConfigEnricher is an early bootstrap step that populates
// cfg.Node.Kubelet.ServerURL and cfg.Node.Kubelet.CACertData from the AKS
// cluster admin credentials for non-bootstrap-token auth modes (Arc, SP, MI).
// Bootstrap token mode already requires these fields in the config file.
type clusterConfigEnricher struct {
	cfg    *config.Config
	logger *logrus.Logger
}

func newClusterConfigEnricher(cfg *config.Config, logger *logrus.Logger) *clusterConfigEnricher {
	return &clusterConfigEnricher{
		cfg:    cfg,
		logger: logger,
	}
}

func (e *clusterConfigEnricher) GetName() string {
	return "enrich-cluster-config"
}

// IsCompleted returns true if both ServerURL and CACertData are already
// populated (either from the config file or a previous execution of this step),
// making the ARM fetch unnecessary.
func (e *clusterConfigEnricher) IsCompleted(_ context.Context) bool {
	return e.cfg.Node.Kubelet.ServerURL != "" && e.cfg.Node.Kubelet.CACertData != ""
}

// Execute fetches cluster admin credentials from the AKS management plane and
// writes ServerURL and CACertData into the live config singleton so that later
// bootstrap steps (start-kubelet, start-npd) can use them.
func (e *clusterConfigEnricher) Execute(ctx context.Context) error {
	if e.cfg.IsBootstrapTokenConfigured() {
		// Bootstrap token mode: ServerURL and CACertData are required fields
		// already validated at config load time — nothing to do.
		return nil
	}

	e.logger.Info("Fetching cluster admin credentials to populate server URL and CA cert data")

	cred, err := auth.NewAuthProvider().UserCredential(e.cfg)
	if err != nil {
		return fmt.Errorf("failed to get credential: %w", err)
	}

	clusterSubID := e.cfg.GetTargetClusterSubscriptionID()
	mcClient, err := armcontainerservice.NewManagedClustersClient(clusterSubID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create managed clusters client: %w", err)
	}

	clusterRG := e.cfg.GetTargetClusterResourceGroup()
	clusterName := e.cfg.GetTargetClusterName()

	resp, err := mcClient.ListClusterAdminCredentials(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return fmt.Errorf("failed to list cluster admin credentials for %s/%s: %w", clusterRG, clusterName, err)
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
		return fmt.Errorf("failed to extract cluster info from kubeconfig: %w", err)
	}

	e.cfg.Node.Kubelet.ServerURL = serverURL
	e.cfg.Node.Kubelet.CACertData = caCertData
	e.logger.Infof("Cluster config enriched: serverURL=%s", serverURL)
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
		return "", "", fmt.Errorf("failed to parse kubeconfig YAML: %w", err)
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
