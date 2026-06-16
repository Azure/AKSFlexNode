package config

import (
	"encoding/json"
	"strings"
)

// poolBootstrapData is the AKS RP-generated bootstrap config shape. The agent
// accepts it at load time and normalizes it into the runtime Config shape.
// this is based on v20260502preview AKS RP API version
type poolBootstrapData struct {
	Components *poolBootstrapComponents `json:"components,omitempty"`
	Networking *poolBootstrapNetworking `json:"networking,omitempty"`
	Node       *poolBootstrapNode       `json:"node,omitempty"`
}

type poolBootstrapComponents struct {
	Kubernetes string `json:"kubernetes,omitempty"`
	Containerd string `json:"containerd,omitempty"`
	Runc       string `json:"runc,omitempty"`
}

type poolBootstrapNetworking struct {
	DNSServiceIP string `json:"dnsServiceIP,omitempty"`
	CNIVersion   string `json:"cniVersion,omitempty"`
}

type poolBootstrapNode struct {
	MaxPods *int                  `json:"maxPods,omitempty"`
	Labels  map[string]string     `json:"labels,omitempty"`
	Taints  []string              `json:"taints,omitempty"`
	Kubelet *poolBootstrapKubelet `json:"kubelet,omitempty"`
}

type poolBootstrapKubelet struct {
	ClusterFQDN string `json:"clusterFQDN,omitempty"`
	CACertData  string `json:"caCertData,omitempty"`
}

// adaptPoolBootstrapData copies AKS RP bootstrap fields into canonical Config
// fields without overriding values already provided in the runtime config shape.
func adaptPoolBootstrapData(data []byte, cfg *Config) error {
	if cfg == nil {
		return nil
	}

	var bootstrap poolBootstrapData
	if err := json.Unmarshal(data, &bootstrap); err != nil {
		return err
	}

	// The AKS RP bootstrap response is an input contract, not the runtime config
	// shape. Normalize it here so the rest of the agent only sees Config fields.
	if bootstrap.Components != nil {
		if cfg.Kubernetes.Version == "" {
			cfg.Kubernetes.Version = bootstrap.Components.Kubernetes
		}
		if cfg.Containerd.Version == "" {
			cfg.Containerd.Version = bootstrap.Components.Containerd
		}
		if cfg.Runc.Version == "" {
			cfg.Runc.Version = bootstrap.Components.Runc
		}
	}
	if bootstrap.Networking != nil {
		if cfg.Node.Kubelet.DNSServiceIP == "" {
			cfg.Node.Kubelet.DNSServiceIP = bootstrap.Networking.DNSServiceIP
		}
		if cfg.CNI.Version == "" {
			cfg.CNI.Version = bootstrap.Networking.CNIVersion
		}
	}
	if bootstrap.Node != nil {
		if cfg.Node.MaxPods == 0 && bootstrap.Node.MaxPods != nil {
			cfg.Node.MaxPods = *bootstrap.Node.MaxPods
		}
		if cfg.Node.Labels == nil && bootstrap.Node.Labels != nil {
			cfg.Node.Labels = bootstrap.Node.Labels
		}
		if cfg.Node.Taints == nil && bootstrap.Node.Taints != nil {
			cfg.Node.Taints = bootstrap.Node.Taints
		}
		if bootstrap.Node.Kubelet != nil {
			if cfg.Node.Kubelet.ServerURL == "" {
				cfg.Node.Kubelet.ServerURL = serverURLFromClusterFQDN(bootstrap.Node.Kubelet.ClusterFQDN)
			}
			if cfg.Node.Kubelet.CACertData == "" {
				cfg.Node.Kubelet.CACertData = bootstrap.Node.Kubelet.CACertData
			}
		}
	}

	return nil
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
