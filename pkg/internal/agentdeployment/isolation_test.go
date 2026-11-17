package agentdeployment

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

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestCandidateIsolationProbe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		script  string
		wantErr bool
	}{
		{
			name:   "old nspawn release",
			mode:   isolation.ModeNSpawn,
			script: "exit 1",
		},
		{
			name:    "old none release",
			mode:    isolation.ModeNone,
			script:  "exit 1",
			wantErr: true,
		},
		{
			name:    "old OCI release",
			mode:    isolation.ModeContainer,
			script:  "exit 1",
			wantErr: true,
		},
		{
			name:   "OCI support",
			mode:   isolation.ModeContainer,
			script: `test "$1" = supports-isolation && test "$2" = container`,
		},
		{
			name:   "none support",
			mode:   isolation.ModeNone,
			script: `test "$1" = supports-isolation && test "$2" = none`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "candidate")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := CheckIsolation(t.Context(), path, tc.mode); (err != nil) != tc.wantErr {
				t.Fatalf("probe error = %v", err)
			}
		})
	}
}

func TestNoneUpgradeRejectsUnsupportedCandidateBeforeSwitch(t *testing.T) {
	t.Parallel()

	archive := agentUpgradeTestArchive(t, "#!/bin/sh\nif [ \"$1\" = version ]; then echo v1; exit 0; fi\nexit 1\n")
	digest := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(archive); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.BluePath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.CurrentPath, paths.LastGoodPath} {
		if err := os.Symlink(paths.BluePath, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := installAndSwitchBinary(t.Context(), slog.New(slog.DiscardHandler), server.URL, hex.EncodeToString(digest[:]), paths, isolation.ModeNone); err == nil {
		t.Fatal("unsupported candidate was accepted")
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
}

type snapshotActivationService struct {
	candidate string
	original  string
}

func (s snapshotActivationService) Preflight(ctx context.Context, _ string) (agentbinary.ServicePlan, error) {
	if err := CheckIsolation(ctx, s.candidate, isolation.ModeNone); err != nil {
		return agentbinary.ServicePlan{}, err
	}
	// Replacing the caller's candidate after its compatibility check must not
	// change the input that the SDK installs.
	if err := os.WriteFile(s.original, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		return agentbinary.ServicePlan{}, err
	}

	return agentbinary.ServicePlan{}, nil
}

func (snapshotActivationService) Prepare(context.Context, string) error     { return nil }
func (snapshotActivationService) Reload(context.Context) error              { return nil }
func (snapshotActivationService) Restart(context.Context) error             { return nil }
func (snapshotActivationService) WaitHealthy(context.Context, string) error { return nil }

func TestActivationUsesPrivateCompatibilityCandidate(t *testing.T) {
	t.Parallel()

	original := filepath.Join(t.TempDir(), "candidate")
	const content = "#!/bin/sh\ncase \"$1\" in version) echo v1;; supports-isolation) test \"$2\" = none;; *) exit 1;; esac\n"
	writeTestFile(t, original, content)
	candidate, cleanup, err := SnapshotCandidate(original)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	info, err := os.Stat(filepath.Dir(candidate))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot directory permissions: %v, %v", info, err)
	}

	paths := testAgentUpgradePaths(t)
	writeTestFile(t, paths.BinaryPath, "#!/bin/sh\necho old\n")
	if err := ensureUpgradeLayout(t.Context(), discardLogger(), paths); err != nil {
		t.Fatal(err)
	}
	opts := HostAgentActivationOptions(paths, candidate)
	opts.LockPath = filepath.Join(t.TempDir(), "activation.lock")
	result, err := agentbinary.ActivateHostDaemon(t.Context(), discardLogger(), opts, snapshotActivationService{
		candidate: candidate,
		original:  original,
	})
	if err != nil || !result.Changed || result.RolledBack {
		t.Fatalf("activation result = %+v, %v", result, err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
	assertAgentUpgradeFileContent(t, paths.CurrentPath, content)
	cleanup()
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("private snapshot survived cleanup: %v", err)
	}
}
