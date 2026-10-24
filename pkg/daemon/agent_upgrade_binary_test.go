package daemon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
)

func TestEnsureAgentUpgradeLayoutMigratesLegacyBinaryIdempotently(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.BinaryPath, []byte("legacy"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for range 2 {
		if err := ensureAgentUpgradeLayout(t.Context(), slog.Default(), paths); err != nil {
			t.Fatalf("ensureAgentUpgradeLayout: %v", err)
		}
	}

	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
	assertResolvedPath(t, paths.BinaryPath, paths.BluePath)
	data, err := os.ReadFile(paths.BluePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "legacy" {
		t.Fatalf("blue slot = %q, want legacy", data)
	}
}

func TestEnsureAgentUpgradeLayoutRecoversInterruptedLastGoodInitialization(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.BluePath, []byte("active"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink current: %v", err)
	}

	if err := ensureAgentUpgradeLayout(t.Context(), slog.Default(), paths); err != nil {
		t.Fatalf("ensureAgentUpgradeLayout: %v", err)
	}

	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
	assertResolvedPath(t, paths.BinaryPath, paths.BluePath)
}

func TestSecureAgentInstallOptions(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	opts, err := secureAgentInstallOptions("https://example.com/agent.tar.gz?sig=secret", digest)
	if err != nil {
		t.Fatalf("secureAgentInstallOptions: %v", err)
	}
	wantMember, err := config.AgentBinaryArchiveMember(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("AgentBinaryArchiveMember: %v", err)
	}
	if opts.ExpectedMember != wantMember {
		t.Fatalf("ExpectedMember = %q, want %q", opts.ExpectedMember, wantMember)
	}
	if opts.MaxArchiveBytes != agentUpgradeMaxArchiveBytes || opts.MaxExtractedBytes != agentUpgradeMaxBinaryBytes {
		t.Fatalf("size limits = %d, %d", opts.MaxArchiveBytes, opts.MaxExtractedBytes)
	}
	if !opts.ExactMember {
		t.Fatal("ExactMember = false")
	}
	if opts.ExpectedSHA256 != digest {
		t.Fatalf("ExpectedSHA256 = %q, want %q", opts.ExpectedSHA256, digest)
	}
	if _, err := secureAgentInstallOptions("http://127.0.0.1/agent.tar.gz", digest); err != nil {
		t.Fatalf("secureAgentInstallOptions with HTTP: %v", err)
	}
}

func TestSecureAgentInstallOptionsRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		url    string
		digest string
	}{
		"unsupported scheme": {
			url:    "ftp://example.com/agent.tar.gz",
			digest: strings.Repeat("a", 64),
		},
		"missing digest": {
			url: "https://example.com/agent.tar.gz",
		},
		"blank digest": {
			url:    "https://example.com/agent.tar.gz",
			digest: " \t\n",
		},
		"prefix without digest": {
			url:    "https://example.com/agent.tar.gz",
			digest: "sha256:",
		},
		"invalid digest": {
			url:    "https://example.com/agent.tar.gz",
			digest: "bad",
		},
		"non-hex digest": {
			url:    "https://example.com/agent.tar.gz",
			digest: strings.Repeat("g", 64),
		},
		"long digest": {
			url:    "https://example.com/agent.tar.gz",
			digest: strings.Repeat("a", 66),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := secureAgentInstallOptions(tt.url, tt.digest); err == nil {
				t.Fatal("secureAgentInstallOptions error = nil")
			}
		})
	}
}

func TestInstallAndSwitchAgentBinaryRejectsInvalidInputsWithoutSwitching(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.BluePath, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	err := installAndSwitchAgentBinary(
		t.Context(),
		slog.Default(),
		"ftp://example.com/agent.tar.gz",
		strings.Repeat("0", 64),
		paths,
	)
	if err == nil {
		t.Fatal("installAndSwitchAgentBinary error = nil")
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
}

func testAgentUpgradePaths(t *testing.T) agentUpgradePaths {
	t.Helper()
	dir := t.TempDir()
	return agentUpgradePaths{
		BinaryPath:   filepath.Join(dir, "bin", "aks-flex-node"),
		BluePath:     filepath.Join(dir, "lib", "aks-flex-node-blue"),
		GreenPath:    filepath.Join(dir, "lib", "aks-flex-node-green"),
		CurrentPath:  filepath.Join(dir, "lib", "aks-flex-node-current"),
		LastGoodPath: filepath.Join(dir, "lib", "aks-flex-node-last-good"),
		SignalPath:   filepath.Join(dir, "etc", "agent-upgrade-signal.json"),
	}
}

func assertResolvedPath(t *testing.T, path, want string) {
	t.Helper()
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	if got != want {
		t.Fatalf("resolved %s = %s, want %s", path, got, want)
	}
}

// TestFlattenAgentLayout: reset leaves the agent's binary a regular file, the binary the layout
// ran, and no layout, so the aks-flex-node command still works and the next start seeds a new
// layout from the binary that runs it.
func TestFlattenAgentLayout(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		layout       bool
		breakCurrent bool
		lockHeld     bool
		wantBinary   string
		wantLayout   bool
		wantErr      error
	}{
		"a layout becomes its binary": {layout: true, wantBinary: "agent"},
		// The last good binary, when current leads nowhere.
		"a broken current falls back to last-good": {layout: true, breakCurrent: true, wantBinary: "agent"},
		"no layout is left alone":                  {wantBinary: "direct agent"},
		// It never runs beside an activation's switch.
		"an activation in progress keeps the layout": {layout: true, lockHeld: true, wantLayout: true, wantErr: agentbinary.ErrActivationInProgress},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			lockPath := filepath.Join(t.TempDir(), "agent-upgrade.lock")
			if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.layout {
				writeExecutable(t, paths.BinaryPath, "agent")
				if err := ensureAgentUpgradeLayout(t.Context(), discardLogger(), paths); err != nil {
					t.Fatalf("ensureAgentUpgradeLayout: %v", err)
				}
				if tc.breakCurrent {
					if err := os.Remove(paths.CurrentPath); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), paths.CurrentPath); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				writeExecutable(t, paths.BinaryPath, "direct agent")
			}
			if tc.lockHeld {
				lock, err := agentbinary.AcquireHostActivationLock(lockPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}

			for range 2 {
				err := flattenAgentLayout(discardLogger(), paths, lockPath)
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("flattenAgentLayout() error = %v, want %v", err, tc.wantErr)
				}
			}
			_, err := os.Lstat(filepath.Dir(paths.CurrentPath))
			if layout := err == nil; layout != tc.wantLayout {
				t.Fatalf("layout kept = %v, want %v", layout, tc.wantLayout)
			}
			if tc.wantLayout {
				return
			}
			info, err := os.Lstat(paths.BinaryPath)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				t.Fatalf("%s is %v, %v; want a regular executable file", paths.BinaryPath, info, err)
			}
			if got, err := os.ReadFile(paths.BinaryPath); err != nil || string(got) != tc.wantBinary {
				t.Fatalf("%s holds %q, %v; want %q", paths.BinaryPath, got, err, tc.wantBinary)
			}
		})
	}
}
