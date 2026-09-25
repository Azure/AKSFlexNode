package daemon

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// TestHostRootMarkersAreTheReleasedBinaryLayout ties the markers to the layout
// released versions installed under the legacy root. A marker that does not
// match it would leave those hosts unmigrated, and the new agent would install a
// second layout beside the one systemd runs.
func TestHostRootMarkersAreTheReleasedBinaryLayout(t *testing.T) {
	t.Parallel()

	legacy := agentUpgradePathsUnder(hostroot.LegacyPath)
	markers := HostRootMarkers()

	tests := []struct {
		name   string
		path   string
		marker bool
	}{
		{name: "compatibility link", path: legacy.BinaryPath, marker: true},
		{name: "blue slot", path: legacy.BluePath, marker: true},
		{name: "green slot", path: legacy.GreenPath, marker: true},
		{name: "current link", path: legacy.CurrentPath, marker: true},
		{name: "last-good link", path: legacy.LastGoodPath, marker: true},
		// Not part of the binary layout, and an older reset can leave the
		// library helpers behind.
		{name: "recovery script", path: recoveryScriptPathUnder(hostroot.LegacyPath), marker: false},
		{name: "nspawn lifecycle helper", path: "/usr/local/bin/unbounded-agent-nspawn-lifecycle", marker: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rel, err := filepath.Rel(hostroot.LegacyPath, tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(markers, rel); got != tt.marker {
				t.Fatalf("%s is a marker = %v, want %v", rel, got, tt.marker)
			}
		})
	}
}
