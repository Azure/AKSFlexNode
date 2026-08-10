package daemon

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

func TestSecureAgentInstallOptions(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	opts, err := secureAgentInstallOptions("https://example.com/agent.tar.gz?sig=secret", digest)
	if err != nil {
		t.Fatalf("secureAgentInstallOptions: %v", err)
	}
	if opts.ExpectedMember != "aks-flex-node-linux-"+runtime.GOARCH {
		t.Fatalf("ExpectedMember = %q", opts.ExpectedMember)
	}
	if opts.MaxArchiveBytes != agentUpgradeMaxArchiveBytes || opts.MaxExtractedBytes != agentUpgradeMaxBinaryBytes {
		t.Fatalf("size limits = %d, %d", opts.MaxArchiveBytes, opts.MaxExtractedBytes)
	}
	if !opts.ExactMember {
		t.Fatal("ExactMember = false")
	}
	withoutDigest, err := secureAgentInstallOptions("https://example.com/agent.tar.gz", "")
	if err != nil {
		t.Fatalf("secureAgentInstallOptions without digest: %v", err)
	}
	if withoutDigest.ExpectedSHA256 != "" {
		t.Fatalf("ExpectedSHA256 = %q, want empty", withoutDigest.ExpectedSHA256)
	}
	redirect, err := http.NewRequest(http.MethodGet, "http://example.com/agent.tar.gz", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if err := opts.HTTPClient.CheckRedirect(redirect, nil); err == nil {
		t.Fatal("HTTP redirect was accepted")
	}
}

func TestSecureAgentInstallOptionsRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		url    string
		digest string
	}{
		"HTTP": {
			url:    "http://example.com/agent.tar.gz",
			digest: strings.Repeat("a", 64),
		},
		"invalid digest": {
			url:    "https://example.com/agent.tar.gz",
			digest: "bad",
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
		"http://example.com/agent.tar.gz",
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
