package localdns

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCorefileTemplateGolden(t *testing.T) {
	t.Parallel()

	profile := &LocalDNSProfile{
		Mode: LocalDNSModeRequired,
		VnetDNSOverrides: map[string]LocalDNSOverride{
			".": {
				QueryLogging: "Log", Protocol: "PreferUDP", ForwardDestination: "VnetDNS",
				ForwardPolicy: "Sequential", MaxConcurrent: 1000, CacheDurationInSeconds: 30,
				ServeStaleDurationInSeconds: 60, ServeStale: "Immediate",
			},
			"cluster.local": {
				QueryLogging: "Error", Protocol: "ForceTCP", ForwardDestination: "ClusterCoreDNS",
				ForwardPolicy: "RoundRobin", MaxConcurrent: 2000, CacheDurationInSeconds: 45,
				ServeStale: "Disable",
			},
		},
		KubeDNSOverrides: map[string]LocalDNSOverride{
			".": {
				QueryLogging: "Error", Protocol: "PreferUDP", ForwardDestination: "ClusterCoreDNS",
				ForwardPolicy: "Random", MaxConcurrent: 1500, CacheDurationInSeconds: 90,
				ServeStaleDurationInSeconds: 120, ServeStale: "Verify",
			},
		},
	}

	got, err := profile.CorefileTemplate()
	if err != nil {
		t.Fatalf("CorefileTemplate() error = %v", err)
	}
	goldenPath := filepath.Join("testdata", "localdns.corefile.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("Corefile mismatch (-want +got):\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func TestLocalDNSDefaultBlocks(t *testing.T) {
	t.Parallel()

	vnet := localDNSCorefileBlocks(defaultLocalDNSOverrides(), "node-listener", "VnetDNS")
	kube := localDNSCorefileBlocks(defaultLocalDNSOverrides(), "cluster-listener", "ClusterCoreDNS")
	if len(vnet) != 2 || len(kube) != 2 {
		t.Fatalf("default blocks: VnetDNS=%d KubeDNS=%d, want 2 each", len(vnet), len(kube))
	}

	if vnet[0].Zone != "." || vnet[0].Upstream != "{{ .NodeUpstreamIPsJoined }}" || vnet[0].ForceTCP {
		t.Errorf("VnetDNS root block = %#v, want node upstream over PreferUDP", vnet[0])
	}
	if vnet[1].Zone != "cluster.local" || vnet[1].Upstream != "{{ .ClusterDNSServiceIP }}" || !vnet[1].ForceTCP {
		t.Errorf("VnetDNS cluster block = %#v, want cluster upstream over ForceTCP", vnet[1])
	}
	if kube[0].Zone != "." || kube[0].Upstream != "{{ .ClusterDNSServiceIP }}" || kube[0].ForceTCP {
		t.Errorf("KubeDNS root block = %#v, want cluster upstream over PreferUDP", kube[0])
	}
	if kube[1].Zone != "cluster.local" || kube[1].Upstream != "{{ .ClusterDNSServiceIP }}" || !kube[1].ForceTCP {
		t.Errorf("KubeDNS cluster block = %#v, want cluster upstream over ForceTCP", kube[1])
	}
}

func TestLocalDNSCorefileBlocksAgentBakerRouting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		zone               string
		destination        string
		defaultDestination string
		wantUpstream       string
		wantNSID           string
	}{
		{name: "VnetDNS root stays on node DNS", zone: ".", destination: "ClusterCoreDNS", defaultDestination: "VnetDNS", wantUpstream: "{{ .NodeUpstreamIPsJoined }}", wantNSID: "localdns"},
		{name: "VnetDNS cluster suffix uses cluster DNS", zone: "svc.cluster.local", destination: "VnetDNS", defaultDestination: "VnetDNS", wantUpstream: "{{ .ClusterDNSServiceIP }}", wantNSID: "localdns"},
		{name: "KubeDNS cluster suffix uses cluster DNS", zone: "svc.cluster.local", destination: "VnetDNS", defaultDestination: "ClusterCoreDNS", wantUpstream: "{{ .ClusterDNSServiceIP }}", wantNSID: "localdns-pod"},
		{name: "KubeDNS custom zone honors VnetDNS", zone: "example.com", destination: "VnetDNS", defaultDestination: "ClusterCoreDNS", wantUpstream: "{{ .NodeUpstreamIPsJoined }}", wantNSID: "localdns-pod"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			blocks := localDNSCorefileBlocks(map[string]LocalDNSOverride{
				test.zone: {ForwardDestination: test.destination},
			}, "listener", test.defaultDestination)
			if len(blocks) != 1 {
				t.Fatalf("localDNSCorefileBlocks() returned %d blocks, want 1", len(blocks))
			}
			if blocks[0].Upstream != test.wantUpstream {
				t.Errorf("Upstream = %q, want %q", blocks[0].Upstream, test.wantUpstream)
			}
			if blocks[0].NSID != test.wantNSID {
				t.Errorf("NSID = %q, want %q", blocks[0].NSID, test.wantNSID)
			}
		})
	}
}
