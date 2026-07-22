package nodebootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateAgent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		format   string
		artifact func(*testing.T, []byte) []byte
	}{
		"raw binary": {
			format: "binary",
			artifact: func(_ *testing.T, binary []byte) []byte {
				return binary
			},
		},
		"tar gzip": {
			format: "tar.gz",
			artifact: func(t *testing.T, binary []byte) []byte {
				t.Helper()
				return makeAgentArchive(t, "aks-flex-node-linux-amd64", binary)
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			binary := []byte("new agent")
			artifact := test.artifact(t, binary)
			source := filepath.Join(dir, "agent-{{OS}}-{{ARCH}}")
			expandedSource := filepath.Join(dir, "agent-linux-amd64")
			if err := os.WriteFile(expandedSource, artifact, 0o600); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}
			digest := sha256.Sum256(artifact)
			destination := filepath.Join(dir, "bin", "aks-flex-node")
			downloader, err := NewDownloader(StorageAuthOptions{})
			if err != nil {
				t.Fatalf("NewDownloader() error = %v", err)
			}
			result, err := UpdateAgent(context.Background(), downloader, AgentUpdateOptions{
				Source:      source,
				SHA256:      hex.EncodeToString(digest[:]),
				Format:      test.format,
				Destination: destination,
				GOOS:        "linux",
				GOARCH:      "amd64",
			})
			if err != nil {
				t.Fatalf("UpdateAgent() error = %v", err)
			}
			if !result.Updated {
				t.Fatal("UpdateAgent() Updated = false, want true")
			}
			got, err := os.ReadFile(destination)
			if err != nil {
				t.Fatalf("os.ReadFile() error = %v", err)
			}
			if !bytes.Equal(got, binary) {
				t.Fatalf("installed binary = %q, want %q", got, binary)
			}
			info, err := os.Stat(destination)
			if err != nil {
				t.Fatalf("os.Stat() error = %v", err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Fatalf("installed mode = %o, want 755", info.Mode().Perm())
			}

			result, err = UpdateAgent(context.Background(), downloader, AgentUpdateOptions{
				Source: source, Format: test.format, Destination: destination, GOOS: "linux", GOARCH: "amd64",
			})
			if err != nil {
				t.Fatalf("second UpdateAgent() error = %v", err)
			}
			if result.Updated {
				t.Fatal("second UpdateAgent() Updated = true, want false")
			}
		})
	}
}

func TestUpdateAgentRejectsDigestMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "agent")
	if err := os.WriteFile(source, []byte("agent"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	downloader, err := NewDownloader(StorageAuthOptions{})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	_, err = UpdateAgent(context.Background(), downloader, AgentUpdateOptions{
		Source: source, SHA256: strings.Repeat("0", 64), Format: "binary", Destination: filepath.Join(dir, "out"),
	})
	if err == nil {
		t.Fatal("UpdateAgent() error = nil, want digest error")
	}
}

func makeAgentArchive(t *testing.T, name string, binary []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar Close() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}
	return buffer.Bytes()
}
