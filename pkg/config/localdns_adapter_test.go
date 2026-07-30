package config

import (
	"strings"
	"testing"
)

func TestToAgentConfigLocalDNS(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Networking: NetworkingConfig{
			DNSServiceIP: "10.0.0.10",
			LocalDNS: &LocalDNSProfile{
				Mode: LocalDNSModeRequired,
				KubeDNSOverrides: map[string]LocalDNSOverride{
					".": {ForwardDestination: "ClusterCoreDNS"},
				},
			},
		},
		Node: NodeConfig{Labels: map[string]string{"example": "value"}},
	}

	got := ToAgentConfig(cfg, "kube1")
	if got.LocalDNS == nil || !got.LocalDNS.Enabled {
		t.Fatalf("LocalDNS = %#v, want enabled", got.LocalDNS)
	}
	if !strings.Contains(got.LocalDNS.CorefileTemplate, "{{ .ClusterDNSServiceIP }}") {
		t.Fatalf("CorefileTemplate missing cluster DNS placeholder:\n%s", got.LocalDNS.CorefileTemplate)
	}
	if got.Kubelet.Labels["kubernetes.azure.com/localdns-state"] != "enabled" {
		t.Fatalf("LocalDNS node label missing: %#v", got.Kubelet.Labels)
	}
	if got.Cluster.ClusterDNS != "10.0.0.10" {
		t.Fatalf("ClusterDNS = %q, want original service IP", got.Cluster.ClusterDNS)
	}
}

func TestToAgentConfigDisabledLocalDNS(t *testing.T) {
	t.Parallel()

	cfg := &Config{Networking: NetworkingConfig{
		DNSServiceIP: "10.0.0.10",
		LocalDNS:     &LocalDNSProfile{Mode: LocalDNSModeDisabled},
	}}
	got := ToAgentConfig(cfg, "kube1")
	if got.LocalDNS == nil || got.LocalDNS.Enabled {
		t.Fatalf("LocalDNS = %#v, want explicitly disabled", got.LocalDNS)
	}
	if got.Kubelet.Labels["kubernetes.azure.com/localdns-state"] != "disabled" {
		t.Fatalf("LocalDNS disabled node label missing: %#v", got.Kubelet.Labels)
	}
}
