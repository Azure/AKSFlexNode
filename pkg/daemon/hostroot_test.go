package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// root: made only when no usable binary is there, from the running one, and
// only kept when it runs.
func TestInstallHostBinary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setup     func(t *testing.T, target string)
		verifyErr error
		wantCopy  bool
		wantErr   string
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
		{
			// As under /opt mounted noexec.
			name:      "a copy that cannot run is removed",
			setup:     func(*testing.T, string) {},
			verifyErr: errors.New("permission denied"),
			wantErr:   "must not be mounted noexec",
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
			verified := ""
			err := installHostBinary(t.Context(), slog.New(slog.DiscardHandler), target,
				func() error { prepared = true; return nil },
				func() (string, error) { return self, nil },
				func(_ context.Context, path string) error { verified = path; return tt.verifyErr })

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, tt.verifyErr) {
					t.Fatalf("installHostBinary() error = %v, want one containing %q and the cause", err, tt.wantErr)
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a copy that cannot run was left at %s: %v", target, err)
				}
				return
			}
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
			if wantVerified := map[bool]string{true: target}[tt.wantCopy]; verified != wantVerified {
				t.Fatalf("verified %q, want %q", verified, wantVerified)
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
// /usr/local/bin only once nothing the agent runs is under /usr/local, and only
// when it is a regular file. The library reports a missing root, one linked to
// /usr/local, and one partway through a move as not released; in the last the
// daemon may still run from /usr/local.
func TestRemoveLegacySeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		released    bool
		releasedErr error
		seed        string
		wantKept    bool
		wantErr     bool
	}{
		{name: "installed under the host root, or linked elsewhere", released: true, seed: "file"},
		{name: "not released: absent, linked, or moving", released: false, seed: "file", wantKept: true},
		{name: "the host root cannot be inspected", releasedErr: errors.New("permission denied"), seed: "file", wantKept: true, wantErr: true},
		{name: "a link is not a seed", released: true, seed: "link", wantKept: true},
		{name: "no seed", released: true, seed: ""},
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

			released := func() (bool, error) { return tt.released, tt.releasedErr }
			err := removeLegacySeed(slog.New(slog.DiscardHandler), released, seed)
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

// TestGuardLegacySeed covers what runs before an AgentUpgrade switches the
// binary: the seed goes, and on a host that no longer runs from /usr/local the
// switch is refused while something there would still be taken for a binary by
// a release before the host root.
func TestGuardLegacySeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		released bool
		// setup puts something at the seed path; dir is a scratch directory.
		setup    func(t *testing.T, seed, dir string)
		wantErr  bool
		wantKept bool
	}{
		{name: "nothing there", released: true, setup: func(*testing.T, string, string) {}},
		{
			name:     "a seed is removed",
			released: true,
			setup:    func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
		},
		{
			name:     "a link to an executable is refused",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				target := filepath.Join(dir, "agent")
				writeExecutable(t, target, "agent")
				if err := os.Symlink(target, seed); err != nil {
					t.Fatal(err)
				}
			},
			wantErr:  true,
			wantKept: true,
		},
		{
			// An earlier release's compatibility link, once the move removed
			// what it led to.
			name:     "a dangling link is allowed",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				if err := os.Symlink(filepath.Join(dir, "missing"), seed); err != nil {
					t.Fatal(err)
				}
			},
			wantKept: true,
		},
		{
			name:     "a link to a file that cannot run is allowed",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				target := filepath.Join(dir, "notes")
				if err := os.WriteFile(target, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, seed); err != nil {
					t.Fatal(err)
				}
			},
			wantKept: true,
		},
		{
			// The release before the host root that runs there needs it.
			name:     "a host still running from /usr/local is left alone",
			released: false,
			setup:    func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
			wantKept: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			seed := filepath.Join(dir, "usr", "local", "bin", binaryName)
			if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, seed, dir)

			err := guardLegacySeed(slog.New(slog.DiscardHandler), func() (bool, error) { return tt.released, nil }, seed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("guardLegacySeed() error = %v, want error %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "/usr/local/sbin") {
				t.Fatalf("the refusal does not say how to keep the agent on PATH: %v", err)
			}

			_, err = os.Lstat(seed)
			if kept := err == nil; kept != tt.wantKept {
				t.Fatalf("seed path kept = %v, want %v", kept, tt.wantKept)
			}
		})
	}

	if err := guardLegacySeed(slog.New(slog.DiscardHandler), func() (bool, error) { return false, errors.New("permission denied") },
		filepath.Join(t.TempDir(), binaryName)); err == nil {
		t.Fatal("guardLegacySeed() allowed a switch on a host root it could not inspect")
	}
}

// TestRecoveryAgentUpgradePaths: recovery restores the layout the recovery
// unit ran it from, a blue or green slot, whatever the host root resolves to.
func TestRecoveryAgentUpgradePaths(t *testing.T) {
	t.Parallel()

	moved := agentUpgradePathsUnder(hostroot.Path)
	legacy := agentUpgradePathsUnder(hostroot.LegacyPath)

	tests := []struct {
		name     string
		running  string
		runErr   error
		resolved string
		want     agentUpgradePaths
	}{
		{name: "blue slot under the host root", running: moved.BluePath, resolved: hostroot.LegacyPath, want: moved},
		{name: "green slot under the legacy root", running: legacy.GreenPath, resolved: hostroot.Path, want: legacy},
		{name: "not a slot", running: "/tmp/aks-flex-node", resolved: hostroot.Path, want: moved},
		{name: "a name that only looks like a slot", running: "/srv/lib/aks-flex-node/aks-flex-node-blue-old", resolved: hostroot.Path, want: moved},
		{name: "unknown executable", runErr: errors.New("no /proc"), resolved: hostroot.LegacyPath, want: legacy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := recoveryAgentUpgradePaths(
				func() (string, error) { return tt.running, tt.runErr },
				func() string { return tt.resolved })
			if got != tt.want {
				t.Fatalf("recoveryAgentUpgradePaths() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestVerifyMovedAgent runs the current link of a copied layout, which a move
// does before any unit names the copy.
func TestVerifyMovedAgent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	paths := agentUpgradePathsUnder(root)
	if err := os.MkdirAll(filepath.Dir(paths.CurrentPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := verifyMovedAgent(t.Context(), root); err == nil {
		t.Fatal("verifyMovedAgent() passed a layout with no current binary")
	}

	writeExecutable(t, paths.GreenPath, "#!/bin/sh\n[ \"$1\" = version ]\n")
	if err := os.Symlink(paths.GreenPath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := verifyMovedAgent(t.Context(), root); err != nil {
		t.Fatalf("verifyMovedAgent() error = %v", err)
	}

	if err := os.Chmod(paths.GreenPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyMovedAgent(t.Context(), root); err == nil {
		t.Fatal("verifyMovedAgent() passed a binary that cannot run")
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
