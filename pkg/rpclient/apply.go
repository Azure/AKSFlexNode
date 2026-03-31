package rpclient

import "github.com/Azure/AKSFlexNode/pkg/config"

// ApplyToConfig populates a Config from bootstrap data returned by the RP.
// Fields already set in cfg (from the config file) are NOT overwritten,
// allowing the operator to override RP defaults.
func (bd *BootstrapData) ApplyToConfig(cfg *config.Config) {
	if cfg == nil || bd == nil {
		return
	}

	// Kubernetes version
	if cfg.Kubernetes.Version == "" && bd.KubernetesVersion != "" {
		cfg.Kubernetes.Version = bd.KubernetesVersion
	}

	// Server URL + CA cert
	if cfg.Node.Kubelet.ServerURL == "" && bd.ClusterFQDN != "" {
		cfg.Node.Kubelet.ServerURL = "https://" + bd.ClusterFQDN + ":443"
	}
	if cfg.Node.Kubelet.CACertData == "" && bd.CACertData != "" {
		cfg.Node.Kubelet.CACertData = bd.CACertData
	}

	// DNS
	if cfg.Node.Kubelet.DNSServiceIP == "" && bd.ClusterDNS != "" {
		cfg.Node.Kubelet.DNSServiceIP = bd.ClusterDNS
	}

	// Node config
	if bd.Node != nil {
		if cfg.Node.MaxPods == 0 && bd.Node.MaxPods != nil {
			cfg.Node.MaxPods = *bd.Node.MaxPods
		}
		if bd.Node.Labels != nil && cfg.Node.Labels == nil {
			cfg.Node.Labels = make(map[string]string)
		}
		for k, v := range bd.Node.Labels {
			if _, exists := cfg.Node.Labels[k]; !exists {
				cfg.Node.Labels[k] = v
			}
		}
		if bd.Node.KubeletConfig != nil {
			if cfg.Node.Kubelet.KubeReserved == nil {
				cfg.Node.Kubelet.KubeReserved = make(map[string]string)
			}
			if v, ok := bd.Node.KubeletConfig["kube-reserved"]; ok {
				if len(cfg.Node.Kubelet.KubeReserved) == 0 {
					// Parse "cpu=100m,memory=1Gi" format
					for _, pair := range splitCSV(v) {
						parts := splitKV(pair)
						if len(parts) == 2 {
							cfg.Node.Kubelet.KubeReserved[parts[0]] = parts[1]
						}
					}
				}
			}
		}
	}

	// Binaries
	if bd.Binaries != nil {
		if bd.Binaries.Containerd != nil {
			if cfg.Containerd.Version == "" {
				cfg.Containerd.Version = bd.Binaries.Containerd.Version
			}
		}
		if bd.Binaries.Runc != nil {
			if cfg.Runc.Version == "" {
				cfg.Runc.Version = bd.Binaries.Runc.Version
			}
			if cfg.Runc.URL == "" {
				cfg.Runc.URL = bd.Binaries.Runc.URL
			}
		}
	}

	// CNI
	if bd.CNI != nil {
		if cfg.CNI.Version == "" {
			cfg.CNI.Version = bd.CNI.Version
		}
	}

	// Images
	if bd.Images != nil {
		if cfg.Containerd.PauseImage == "" && bd.Images.Pause != "" {
			cfg.Containerd.PauseImage = bd.Images.Pause
		}
	}

	// Bootstrap token
	if bd.BootstrapToken != "" {
		if cfg.Azure.BootstrapToken == nil {
			cfg.Azure.BootstrapToken = &config.BootstrapTokenConfig{}
		}
		if cfg.Azure.BootstrapToken.Token == "" {
			cfg.Azure.BootstrapToken.Token = bd.BootstrapToken
		}
	}
}

func splitCSV(s string) []string {
	var result []string
	for _, part := range split(s, ',') {
		result = append(result, part)
	}
	return result
}

func splitKV(s string) []string {
	return split(s, '=')
}

func split(s string, sep byte) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	result = append(result, s[start:])
	return result
}
