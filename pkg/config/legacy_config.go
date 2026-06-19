package config

import (
	"encoding/json"
	"net/url"
	"strings"
)

type legacyConfigData struct {
	Containerd *legacyVersionConfig `json:"containerd,omitempty"`
	Kubernetes *legacyVersionConfig `json:"kubernetes,omitempty"`
	CNI        *legacyVersionConfig `json:"cni,omitempty"`
	Runc       *legacyVersionConfig `json:"runc,omitempty"`
	Node       *legacyNodeConfig    `json:"node,omitempty"`
}

type legacyVersionConfig struct {
	Version string `json:"version,omitempty"`
}

type legacyNodeConfig struct {
	Kubelet *legacyKubeletConfig `json:"kubelet,omitempty"`
}

type legacyKubeletConfig struct {
	DNSServiceIP string `json:"dnsServiceIP,omitempty"`
	ServerURL    string `json:"serverURL,omitempty"`
}

// adaptLegacyConfigData keeps pre-RP config files working while the runtime
// Config shape follows the AKS RP bootstrap contract.
func adaptLegacyConfigData(data []byte, cfg *Config) error {
	if cfg == nil {
		return nil
	}

	var legacy legacyConfigData
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}

	if legacy.Kubernetes != nil && cfg.Components.Kubernetes == "" {
		cfg.Components.Kubernetes = legacy.Kubernetes.Version
	}
	if legacy.Containerd != nil && cfg.Components.Containerd == "" {
		cfg.Components.Containerd = legacy.Containerd.Version
	}
	if legacy.Runc != nil && cfg.Components.Runc == "" {
		cfg.Components.Runc = legacy.Runc.Version
	}
	if legacy.CNI != nil && cfg.Networking.CNIVersion == "" {
		cfg.Networking.CNIVersion = legacy.CNI.Version
	}
	if legacy.Node != nil && legacy.Node.Kubelet != nil {
		if cfg.Networking.DNSServiceIP == "" {
			cfg.Networking.DNSServiceIP = legacy.Node.Kubelet.DNSServiceIP
		}
		if cfg.Node.Kubelet.ClusterFQDN == "" {
			cfg.Node.Kubelet.ClusterFQDN = clusterFQDNFromServerURL(legacy.Node.Kubelet.ServerURL)
		}
	}

	return nil
}

func clusterFQDNFromServerURL(serverURL string) string {
	serverURL = strings.TrimSpace(serverURL)
	if serverURL == "" {
		return ""
	}
	parsed, err := url.Parse(serverURL)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Host
	}
	return serverURL
}

func serverURLFromClusterFQDN(clusterFQDN string) string {
	clusterFQDN = strings.TrimSpace(clusterFQDN)
	if clusterFQDN == "" {
		return ""
	}
	if strings.Contains(clusterFQDN, "://") {
		return clusterFQDN
	}
	if strings.Contains(clusterFQDN, ":") {
		return "https://" + clusterFQDN
	}
	return "https://" + clusterFQDN + ":443"
}
