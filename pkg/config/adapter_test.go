package config

import (
	"testing"
)

func TestToAgentConfig(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			BootstrapToken: &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
		Kubernetes: KubernetesConfig{Version: "1.30.0"},
		Node: NodeConfig{
			Labels: map[string]string{"env": "test"},
			Taints: []string{"dedicated=infra:NoSchedule"},
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "dGVzdC1jYS1kYXRh", // base64("test-ca-data")
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.MachineName != "kube1" {
		t.Fatalf("MachineName=%q, want %q", ac.MachineName, "kube1")
	}
	if ac.Cluster.Version != "1.30.0" {
		t.Fatalf("Cluster.Version=%q, want %q", ac.Cluster.Version, "1.30.0")
	}
	if ac.Cluster.ClusterDNS != "10.0.0.10" {
		t.Fatalf("Cluster.ClusterDNS=%q, want %q", ac.Cluster.ClusterDNS, "10.0.0.10")
	}
	if ac.Cluster.CaCertBase64 != "dGVzdC1jYS1kYXRh" {
		t.Fatalf("Cluster.CaCertBase64=%q, want %q", ac.Cluster.CaCertBase64, "dGVzdC1jYS1kYXRh")
	}
	if ac.Kubelet.ApiServer != "https://api.example.com:6443" {
		t.Fatalf("Kubelet.ApiServer=%q, want %q", ac.Kubelet.ApiServer, "https://api.example.com:6443")
	}
	if ac.Kubelet.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("Kubelet.BootstrapToken=%q, want %q", ac.Kubelet.BootstrapToken, "abcdef.0123456789abcdef")
	}
	if len(ac.Kubelet.Labels) != 1 || ac.Kubelet.Labels["env"] != "test" {
		t.Fatalf("Kubelet.Labels=%v, want map[env:test]", ac.Kubelet.Labels)
	}
	if len(ac.Kubelet.RegisterWithTaints) != 1 || ac.Kubelet.RegisterWithTaints[0] != "dedicated=infra:NoSchedule" {
		t.Fatalf("Kubelet.RegisterWithTaints=%v, want [dedicated=infra:NoSchedule]", ac.Kubelet.RegisterWithTaints)
	}
}

func TestToAgentConfig_NoBootstrapToken(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			ServicePrincipal: &ServicePrincipalConfig{
				TenantID:     "tenant",
				ClientID:     "client",
				ClientSecret: "secret",
			},
		},
		Kubernetes: KubernetesConfig{Version: "1.31.0"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "ca-data",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube2")

	// Without bootstrap token configured, BootstrapToken should be empty.
	if ac.Kubelet.BootstrapToken != "" {
		t.Fatalf("Kubelet.BootstrapToken=%q, want empty", ac.Kubelet.BootstrapToken)
	}
	if ac.MachineName != "kube2" {
		t.Fatalf("MachineName=%q, want %q", ac.MachineName, "kube2")
	}
}
