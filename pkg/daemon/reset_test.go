package daemon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// TestResetNodeCleansUpLocalDNS pins that reset runs the library's full
// network cleanup. It used to call the interface and route cleanups directly
// and skip the LocalDNS one, so every reset of a LocalDNS node left the unit,
// the nft table, the dummy interface and the helper on the host.
func TestResetNodeCleansUpLocalDNS(t *testing.T) {
	t.Parallel()

	name := ResetNode(slog.New(slog.DiscardHandler)).Name()

	for _, want := range []string{
		"cleanup-localdns-rules",
		"remove-network-interfaces",
		"cleanup-routes",
		"remove-host-helpers",
	} {
		if !strings.Contains(name, want) {
			t.Fatalf("ResetNode does not run %s: %s", want, name)
		}
	}
}

// TestHostHelperPaths covers the library helpers reset removes. Reset does not
// migrate the host root, so the legacy root is swept too, including the LocalDNS
// helper, which CleanupNetwork removes under the resolved root only.
func TestHostHelperPaths(t *testing.T) {
	t.Parallel()

	legacy := goalstates.LegacyHostPaths()

	tests := []struct {
		name     string
		resolved goalstates.HostPaths
		want     []string
	}{
		{
			name:     "host root also sweeps the legacy root",
			resolved: goalstates.HostPaths{Root: "/opt/unbounded", NSpawnLifecycleBinary: "/opt/unbounded/bin/unbounded-agent-nspawn-lifecycle"},
			want: []string{
				"/opt/unbounded/bin/unbounded-agent-nspawn-lifecycle",
				"/usr/local/bin/unbounded-agent-nspawn-lifecycle",
				"/usr/local/libexec/unbounded-localdns-network",
			},
		},
		{
			name:     "migrated host sweeps the legacy root once",
			resolved: legacy,
			want:     []string{"/usr/local/bin/unbounded-agent-nspawn-lifecycle"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := hostHelperPathsFor(tt.resolved, legacy)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("hostHelperPathsFor(%q) = %v, want %v", tt.resolved.Root, got, tt.want)
			}
		})
	}
}

// TestRemoveFilesTaskIsIdempotent runs the removal against a temp tree twice.
// Reset can be retried after a partial failure, so a second pass over files
// that are already gone must succeed.
func TestRemoveFilesTaskIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	task := &removeFilesTask{
		name:  "test",
		log:   slog.New(slog.DiscardHandler),
		paths: []string{present, filepath.Join(dir, "absent")},
	}

	for pass := 1; pass <= 2; pass++ {
		if err := task.Do(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if _, err := os.Lstat(present); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was not removed: %v", err)
	}
}
