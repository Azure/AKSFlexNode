package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestParseAgentUpgradeSHA256(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("ab", sha256.Size)
	for _, value := range []string{digest, "sha256:" + digest} {
		if _, err := parseAgentUpgradeSHA256(value); err != nil {
			t.Fatalf("parseAgentUpgradeSHA256(%q): %v", value, err)
		}
	}
	for _, value := range []string{"", "abc", strings.Repeat("z", 64)} {
		if _, err := parseAgentUpgradeSHA256(value); err == nil {
			t.Fatalf("parseAgentUpgradeSHA256(%q) error = nil", value)
		}
	}
}

func TestValidateAgentUpgradeURL(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value   string
		wantErr bool
	}{
		"HTTPS":        {value: "https://example.com/agent.tar.gz?sig=secret"},
		"HTTP":         {value: "http://example.com/agent.tar.gz", wantErr: true},
		"file":         {value: "file:///tmp/agent.tar.gz", wantErr: true},
		"userinfo":     {value: "https://user:secret@example.com/agent.tar.gz", wantErr: true},
		"missing host": {value: "https:///agent.tar.gz", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := validateAgentUpgradeURL(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateAgentUpgradeURL() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRedactedAgentUpgradeURLRemovesQuery(t *testing.T) {
	t.Parallel()

	parsed, err := url.Parse("https://example.com/agent.tar.gz?sig=secret")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := redactedAgentUpgradeURL(parsed); got != "https://example.com/agent.tar.gz" {
		t.Fatalf("redacted URL = %q", got)
	}
}

func TestDownloadAgentUpgradeArchiveVerifiesDigestAndRedactsErrors(t *testing.T) {
	t.Parallel()

	payload := []byte("archive")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL + "/agent.tar.gz?sig=secret")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	expected := sha256.Sum256(payload)
	path, err := downloadAgentUpgradeArchive(t.Context(), server.Client(), parsed, expected)
	if err != nil {
		t.Fatalf("downloadAgentUpgradeArchive: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	wrong := sha256.Sum256([]byte("wrong"))
	_, err = downloadAgentUpgradeArchive(t.Context(), server.Client(), parsed, wrong)
	if err == nil {
		t.Fatal("downloadAgentUpgradeArchive error = nil")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked URL query: %v", err)
	}
}

func TestExtractAgentUpgradeBinary(t *testing.T) {
	t.Parallel()

	member, err := expectedAgentArchiveMember()
	if err != nil {
		t.Skipf("unsupported test architecture: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "agent.tar.gz")
	binary := []byte("#!/bin/sh\nexit 0\n")
	writeTestAgentArchive(t, archivePath, []testTarMember{{name: member, mode: 0o755, body: binary}})
	targetPath := filepath.Join(t.TempDir(), "agent")
	if err := extractAgentUpgradeBinary(archivePath, member, targetPath); err != nil {
		t.Fatalf("extractAgentUpgradeBinary: %v", err)
	}
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("binary = %q, want %q", got, binary)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != agentUpgradeBinaryMode {
		t.Fatalf("mode = %o, want %o", info.Mode().Perm(), agentUpgradeBinaryMode)
	}
}

func TestExtractAgentUpgradeBinaryRejectsUnsafeAndUnexpectedArchives(t *testing.T) {
	t.Parallel()

	member := "aks-flex-node-linux-" + runtime.GOARCH
	tests := map[string][]testTarMember{
		"missing member": {{name: "other", body: []byte("binary")}},
		"traversal":      {{name: "../" + member, body: []byte("binary")}},
		"duplicate": {
			{name: member, body: []byte("one")},
			{name: member, body: []byte("two")},
		},
	}
	for name, members := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			archivePath := filepath.Join(t.TempDir(), "agent.tar.gz")
			writeTestAgentArchive(t, archivePath, members)
			err := extractAgentUpgradeBinary(archivePath, member, filepath.Join(t.TempDir(), "agent"))
			if err == nil {
				t.Fatal("extractAgentUpgradeBinary error = nil")
			}
		})
	}
}

func TestInstallAndSwitchAgentBinarySwitchesAndProtectsLastGood(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	oldBinary := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(paths.BluePath, oldBinary, 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink current: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.LastGoodPath); err != nil {
		t.Fatalf("Symlink last-good: %v", err)
	}
	member, err := expectedAgentArchiveMember()
	if err != nil {
		t.Skipf("unsupported test architecture: %v", err)
	}
	goodArchive := filepath.Join(t.TempDir(), "good.tar.gz")
	goodBinary := []byte("#!/bin/sh\nexit 0\n")
	writeTestAgentArchive(t, goodArchive, []testTarMember{{name: member, body: goodBinary}})
	goodPayload, err := os.ReadFile(goodArchive)
	if err != nil {
		t.Fatalf("ReadFile good archive: %v", err)
	}
	badArchive := filepath.Join(t.TempDir(), "bad.tar.gz")
	badBinary := []byte("#!/bin/sh\nexit 42\n")
	writeTestAgentArchive(t, badArchive, []testTarMember{{name: member, body: badBinary}})
	badPayload, err := os.ReadFile(badArchive)
	if err != nil {
		t.Fatalf("ReadFile bad archive: %v", err)
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/bad.tar.gz" {
			_, _ = w.Write(badPayload)
			return
		}
		_, _ = w.Write(goodPayload)
	}))
	t.Cleanup(server.Close)
	goodDigest := sha256.Sum256(goodPayload)
	if err := installAndSwitchAgentBinaryWithClient(t.Context(), slog.Default(), server.Client(), server.URL+"/good.tar.gz", fmt.Sprintf("%x", goodDigest), paths); err != nil {
		t.Fatalf("install good candidate: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)

	badDigest := sha256.Sum256(badPayload)
	if err := installAndSwitchAgentBinaryWithClient(t.Context(), slog.Default(), server.Client(), server.URL+"/bad.tar.gz", fmt.Sprintf("%x", badDigest), paths); err == nil {
		t.Fatal("install bad candidate error = nil")
	}
	assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
	// The failed candidate overwrote the inactive blue slot, so last-good must
	// have moved to the still-running verified green slot first.
	assertResolvedPath(t, paths.LastGoodPath, paths.GreenPath)
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

	err := installAndSwitchAgentBinary(context.Background(), slog.Default(), "http://example.com/agent.tar.gz", strings.Repeat("0", 64), paths)
	if err == nil {
		t.Fatal("installAndSwitchAgentBinary error = nil")
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
}

type testTarMember struct {
	name string
	mode int64
	body []byte
}

func writeTestAgentArchive(t *testing.T, path string, members []testTarMember) {
	t.Helper()
	file, err := os.Create(path) //nolint:gosec // test-owned temporary path
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	for _, member := range members {
		mode := member.mode
		if mode == 0 {
			mode = 0o755
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: mode, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := io.Copy(tarWriter, bytes.NewReader(member.body)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
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

func Example_redactedAgentUpgradeURL() {
	parsed, _ := url.Parse("https://example.com/agent.tar.gz?sig=secret")
	fmt.Println(redactedAgentUpgradeURL(parsed))
	// Output: https://example.com/agent.tar.gz
}
