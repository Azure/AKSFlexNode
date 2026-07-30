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
				QueryLogging: "Error", Protocol: "ForceTCP", ForwardDestination: "ClusterCoreDNS",
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
