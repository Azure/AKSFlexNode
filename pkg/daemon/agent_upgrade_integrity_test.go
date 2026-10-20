package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestInstallAndSwitchAgentBinaryIntegrity(t *testing.T) {
	t.Parallel()

	// Detect execution even if it happens before a digest rejection. The older
	// version also exercises verified downgrades.
	const candidate = "#!/bin/sh\nprintf '%s\\n' \"$1\" > \"$0.executed\"\nprintf 'v0.1.0\\n'\n"
	archive := agentUpgradeTestArchive(t, candidate)
	digest := sha256.Sum256(archive)
	expected := hex.EncodeToString(digest[:])
	tests := map[string]struct {
		digest   string
		wantErr  string
		download bool
	}{
		"missing digest": {
			wantErr: "missing required parameter",
		},
		"blank digest": {
			digest:  " \t\n",
			wantErr: "missing required parameter",
		},
		"invalid digest": {
			digest:  "bad",
			wantErr: "64 hexadecimal",
		},
		"prefix without digest": {
			digest:  "sha256:",
			wantErr: "64 hexadecimal",
		},
		"mismatched digest": {
			digest:   strings.Repeat("0", 64),
			wantErr:  "SHA-256 does not match",
			download: true,
		},
		"verified downgrade": {
			digest:   expected,
			download: true,
		},
		"prefixed digest": {
			digest:   "sha256:" + expected,
			download: true,
		},
		"uppercase digest": {
			digest:   strings.ToUpper(expected),
			download: true,
		},
		"padded digest": {
			digest:   " \t" + expected + "\n",
			download: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			const active = "#!/bin/sh\nprintf 'v0.2.0\\n'\n"
			const inactive = "previous inactive binary"
			for path, content := range map[string]string{paths.BluePath: active, paths.GreenPath: inactive} {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			for link, target := range map[string]string{paths.CurrentPath: paths.BluePath, paths.LastGoodPath: paths.GreenPath} {
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
			}

			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				downloads.Add(1)
				if _, err := w.Write(archive); err != nil {
					t.Errorf("write archive: %v", err)
				}
			}))
			defer server.Close()
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))

			attempts := 1
			if tt.wantErr != "" {
				attempts = 2
			}
			for range attempts {
				err := installAndSwitchAgentBinary(t.Context(), log, server.URL+"/agent.tar.gz?sig=secret", tt.digest, paths)
				if tt.wantErr == "" {
					if err != nil {
						t.Fatalf("installAndSwitchAgentBinary: %v", err)
					}
					assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
					assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
					assertAgentUpgradeFileContent(t, paths.GreenPath, candidate)
					assertAgentUpgradeFileContent(t, paths.GreenPath+".executed", "version\n")
				} else {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
					}
					if strings.Contains(err.Error(), "secret") {
						t.Fatalf("error exposes URL credentials: %v", err)
					}
					assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
					assertResolvedPath(t, paths.LastGoodPath, paths.GreenPath)
					assertAgentUpgradeFileContent(t, paths.GreenPath, inactive)
					if _, err := os.Stat(paths.GreenPath + ".executed"); !os.IsNotExist(err) {
						t.Fatalf("candidate execution marker: %v, want not found", err)
					}
				}
				assertAgentUpgradeFileContent(t, paths.BluePath, active)
			}
			wantDownloads := 0
			if tt.download {
				wantDownloads = attempts
			}
			if got := downloads.Load(); int(got) != wantDownloads {
				t.Fatalf("downloads = %d, want %d", got, wantDownloads)
			}
			if strings.Contains(logs.String(), "secret") {
				t.Fatalf("logs expose URL credentials: %s", logs.String())
			}
		})
	}
}

func agentUpgradeTestArchive(t *testing.T, content string) []byte {
	t.Helper()

	member, err := config.AgentBinaryArchiveMember(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("AgentBinaryArchiveMember: %v", err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := io.WriteString(tw, content); err != nil {
		t.Fatalf("write member: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return archive.Bytes()
}

func assertAgentUpgradeFileContent(t *testing.T, path, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(content) != want {
		t.Fatalf("content of %s = %q, want %q", path, content, want)
	}
}
