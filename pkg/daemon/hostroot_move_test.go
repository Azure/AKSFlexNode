package daemon

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// TestHostLayout ties the files a move copies to every path the daemon and the
// agent library build under the host root.
func TestHostLayout(t *testing.T) {
	t.Parallel()

	paths := agentUpgradePathsUnder(hostroot.Path)
	library := goalstates.LegacyHostPaths()
	want := []string{
		paths.BinaryPath, paths.BluePath, paths.GreenPath, paths.CurrentPath, paths.LastGoodPath,
		recoveryScriptPathUnder(hostroot.Path),
		filepath.Join(hostroot.Path, strings.TrimPrefix(library.NSpawnLifecycleBinary, hostroot.LegacyPath)),
		filepath.Join(hostroot.Path, strings.TrimPrefix(library.LocalDNSNetworkHelper, hostroot.LegacyPath)),
	}

	var got []string
	for _, rel := range hostLayout() {
		got = append(got, filepath.Join(hostroot.Path, rel))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("hostLayout() = %v, want %v", got, want)
	}

	for _, marker := range HostRootMarkers() {
		if !slices.Contains(hostLayout(), marker) {
			t.Fatalf("marker %s is not part of the layout a move copies", marker)
		}
	}
}

func TestSyncDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")

	tests := []struct {
		name    string
		dirs    []string
		wantErr string
	}{
		{name: "nothing to sync"},
		{name: "existing directories, repeated", dirs: []string{dir, dir, t.TempDir()}},
		{name: "a missing directory is reported", dirs: []string{dir, missing}, wantErr: missing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := syncDirs(tt.dirs...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("syncDirs() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("syncDirs() error = %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}
