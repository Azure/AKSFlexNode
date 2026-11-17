package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestAgentCandidateIsolationGuard(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		mode                 isolation.Mode
		supported, wantError bool
	}{
		{name: "nspawn accepts earlier release", mode: isolation.ModeNSpawn},
		{name: "none refuses earlier release", mode: isolation.ModeNone, wantError: true},
		{name: "none accepts supporting release", mode: isolation.ModeNone, supported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			script := "#!/bin/sh\ncase \"$1\" in version) echo v0.1.0; exit 0;; esac\nexit 1\n"
			if tc.supported {
				script = "#!/bin/sh\ncase \"$1\" in version) echo v0.1.0; exit 0;; supports-isolation) test \"$2\" = none; exit $?;; esac\nexit 1\n"
			}
			archive := agentUpgradeTestArchive(t, script)
			digest := sha256.Sum256(archive)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := w.Write(archive); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			paths := testAgentUpgradePaths(t)
			if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.BluePath, []byte("active release"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{paths.CurrentPath, paths.LastGoodPath} {
				if err := os.Symlink(paths.BluePath, path); err != nil {
					t.Fatal(err)
				}
			}
			err := installAndSwitchAgentBinary(t.Context(), slog.New(slog.DiscardHandler), server.URL, hex.EncodeToString(digest[:]), paths, tc.mode)
			if (err != nil) != tc.wantError {
				t.Fatalf("candidate error=%v, want error=%t", err, tc.wantError)
			}
			want := paths.GreenPath
			if tc.wantError {
				want = paths.BluePath
			}
			assertResolvedPath(t, paths.CurrentPath, want)
			assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
		})
	}
}

func TestDirectNoneActivationRejectsUnsupportedCandidate(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := &flexDaemonActivationService{log: slog.New(slog.DiscardHandler), mode: isolation.ModeNone, candidatePath: path, paths: testAgentUpgradePaths(t), systemdDir: t.TempDir(), inspectService: func(context.Context, *slog.Logger, string) (bool, error) { return false, nil }}
	if _, err := service.Preflight(t.Context(), "/unused/current"); err == nil {
		t.Fatal("unsupported candidate accepted")
	}
}
