package daemon

import (
	"errors"
	"log/slog"
	"os"
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
		// Install scripts put the plain binary there on fresh hosts too.
		{name: "compatibility link", path: legacy.BinaryPath, marker: false},
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

// TestInstallHostBinary covers the copy that seeds the layout under the host
// root: made only when no usable binary is there, from the running one.
func TestInstallHostBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(t *testing.T, target string)
		wantCopy bool
	}{
		{name: "fresh host", setup: func(*testing.T, string) {}, wantCopy: true},
		{
			name: "a usable binary is kept",
			setup: func(t *testing.T, target string) {
				writeExecutable(t, target, "installed")
			},
		},
		{
			name: "a managed link to a usable slot is kept",
			setup: func(t *testing.T, target string) {
				slot := filepath.Join(filepath.Dir(target), "slot")
				writeExecutable(t, slot, "installed")
				if err := os.Symlink(slot, target); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a dangling link is replaced",
			setup: func(t *testing.T, target string) {
				if err := os.Symlink(filepath.Join(filepath.Dir(target), "missing"), target); err != nil {
					t.Fatal(err)
				}
			},
			wantCopy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			self := filepath.Join(dir, "running")
			writeExecutable(t, self, "running")
			target := filepath.Join(dir, "root", "bin", binaryName)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, target)

			prepared := false
			err := installHostBinary(slog.New(slog.DiscardHandler), target,
				func() error { prepared = true; return nil },
				func() (string, error) { return self, nil })
			if err != nil {
				t.Fatalf("installHostBinary() error = %v", err)
			}

			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if copied := string(got) == "running"; copied != tt.wantCopy || prepared != tt.wantCopy {
				t.Fatalf("copied = %v, prepared = %v, want %v", copied, prepared, tt.wantCopy)
			}
			if tt.wantCopy {
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != agentUpgradeBinaryMode {
					t.Fatalf("installed binary mode = %v, want %v", info.Mode().Perm(), os.FileMode(agentUpgradeBinaryMode))
				}
			}
		})
	}
}

// TestRemoveLegacySeed removes the copy an install script left in
// /usr/local/bin only once the host is fully installed under a real host root,
// and only when it is a regular file. The library reports a missing root, one
// linked to /usr/local, and one partway through a move as not installed; in the
// last the daemon may still run from /usr/local.
func TestRemoveLegacySeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		installed    bool
		installedErr error
		seed         string
		wantKept     bool
		wantErr      bool
	}{
		{name: "installed under the host root", installed: true, seed: "file"},
		{name: "not installed: absent, linked, or moving", installed: false, seed: "file", wantKept: true},
		{name: "the host root cannot be inspected", installedErr: errors.New("permission denied"), seed: "file", wantKept: true, wantErr: true},
		{name: "a link is not a seed", installed: true, seed: "link", wantKept: true},
		{name: "no seed", installed: true, seed: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seed := filepath.Join(t.TempDir(), "usr", "local", "bin", binaryName)
			if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
				t.Fatal(err)
			}

			switch tt.seed {
			case "file":
				writeExecutable(t, seed, "seed")
			case "link":
				if err := os.Symlink("/bin/true", seed); err != nil {
					t.Fatal(err)
				}
			}

			installed := func() (bool, error) { return tt.installed, tt.installedErr }
			err := removeLegacySeed(slog.New(slog.DiscardHandler), installed, seed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("removeLegacySeed() error = %v, want error %v", err, tt.wantErr)
			}

			_, err = os.Lstat(seed)
			if kept := err == nil; kept != (tt.wantKept && tt.seed != "") {
				t.Fatalf("seed kept = %v, want %v", kept, tt.wantKept)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
