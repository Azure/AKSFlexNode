package bootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallAgentFromLocalArchive(t *testing.T) {
	t.Parallel()
	binary := []byte("test agent binary")
	archive := makeAgentArchive(t, "aks-flex-node-linux-"+runtime.GOARCH, binary)
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "agent.tar.gz")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	result, err := InstallAgent(context.Background(), Options{
		AgentURL:    "file://" + archivePath,
		AgentSHA256: hex.EncodeToString(digest[:]),
		InstallDir:  filepath.Join(dir, "bin"),
	})
	if err != nil {
		t.Fatalf("InstallAgent() error = %v", err)
	}
	if !result.Reexecute {
		t.Fatal("Reexecute = false, want true")
	}
	got, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, binary) {
		t.Fatalf("binary = %q, want %q", got, binary)
	}
	info, err := os.Stat(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want 755", info.Mode().Perm())
	}
}

func TestInstallAgentChecksumMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.tar.gz")
	if err := os.WriteFile(path, makeAgentArchive(t, "aks-flex-node-linux-"+runtime.GOARCH, []byte("agent")), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := InstallAgent(context.Background(), Options{AgentURL: path, AgentSHA256: string(bytes.Repeat([]byte{'0'}, 64)), InstallDir: filepath.Join(dir, "bin")})
	if err == nil {
		t.Fatal("InstallAgent() error = nil, want checksum error")
	}
}

func makeAgentArchive(t *testing.T, name string, binary []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
