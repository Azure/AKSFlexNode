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
	for _, plugin := range []string{"log", "nsid"} {
		if !strings.Contains(strings.Join(got.LocalDNS.RequiredPlugins, ","), plugin) {
			t.Errorf("RequiredPlugins = %v, want %q", got.LocalDNS.RequiredPlugins, plugin)
		}
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
}
