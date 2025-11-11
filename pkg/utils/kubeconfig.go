package utils

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// KubeconfigCluster represents a cluster in kubeconfig
type KubeconfigCluster struct {
	CertificateAuthorityData string `yaml:"certificate-authority-data,omitempty"`
	Server                   string `yaml:"server"`
}

// KubeconfigClusterEntry represents a cluster entry in kubeconfig
type KubeconfigClusterEntry struct {
	Name    string            `yaml:"name"`
	Cluster KubeconfigCluster `yaml:"cluster"`
}

// Kubeconfig represents the structure of a kubeconfig file
type Kubeconfig struct {
	Clusters []KubeconfigClusterEntry `yaml:"clusters"`
}

// ParseKubeconfig parses kubeconfig YAML data and extracts cluster information
func ParseKubeconfig(kubeconfigData []byte) (*Kubeconfig, error) {
	var kubeconfig Kubeconfig
	if err := yaml.Unmarshal(kubeconfigData, &kubeconfig); err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig YAML: %w", err)
	}
	return &kubeconfig, nil
}

// GetClusterInfo extracts cluster server URL and CA certificate from kubeconfig data
func GetClusterInfo(kubeconfigData []byte) (serverURL, caCertData string, err error) {
	kubeconfig, err := ParseKubeconfig(kubeconfigData)
	if err != nil {
		return "", "", err
	}

	if len(kubeconfig.Clusters) == 0 {
		return "", "", fmt.Errorf("no clusters found in kubeconfig")
	}

	// Use the first cluster (typically the main cluster)
	cluster := kubeconfig.Clusters[0]
	serverURL = strings.TrimSpace(cluster.Cluster.Server)
	caCertData = strings.TrimSpace(cluster.Cluster.CertificateAuthorityData)

	if serverURL == "" {
		return "", "", fmt.Errorf("cluster server URL is empty")
	}

	return serverURL, caCertData, nil
}