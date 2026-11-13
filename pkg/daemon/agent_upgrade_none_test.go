package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestNoneUpgradeRejectsUnsupportedCandidateBeforeSwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, script string
		wantErr      bool
	}{
		{name: "older agent", script: "#!/bin/sh\n[ \"$1\" = version ]\n", wantErr: true},
		{name: "none capable agent", script: "#!/bin/sh\ncase \"$1\" in version) exit 0;; supports-isolation) [ \"$2\" = none ];; *) exit 1;; esac\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			paths := testAgentUpgradePaths(t)
			for _, path := range []string{paths.BluePath, paths.GreenPath} {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(paths.BluePath, paths.LastGoodPath); err != nil {
				t.Fatal(err)
			}
			archive := agentUpgradeTestArchive(t, tc.script)
			sum := sha256.Sum256(archive)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
			defer server.Close()
			err := installAndSwitchAgentBinaryForIsolation(t.Context(), discardLogger(), server.URL, hex.EncodeToString(sum[:]), paths, isolation.ModeNone)
			if (err != nil) != tc.wantErr {
				t.Fatalf("install = %v", err)
			}
			want := paths.GreenPath
			if tc.wantErr {
				want = paths.BluePath
			}
			assertResolvedPath(t, paths.CurrentPath, want)
		})
	}
}

func TestNoneAgentUpgradeSignal(t *testing.T) {
	t.Parallel()
	paths := testAgentUpgradePaths(t)
	store := agentUpgradeSignalStore{path: paths.SignalPath}
	loader := &fakeNodeOperator{state: &State{Isolation: isolation.ModeNone}}
	if err := recordPendingAgentUpgrade(t.Context(), store, loader, "upgrade", "daemon"); err != nil {
		t.Fatal(err)
	}
	signal, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	if signal.Isolation != isolation.ModeNone || signal.ActiveMachine != "" {
		t.Fatal("none upgrade recorded an nspawn machine")
	}
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.BluePath, []byte("#!/bin/sh\nexit 0\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	signal.CandidatePath = paths.BluePath
	if err := validateStartedAgentUpgrade(paths, signal, func(string) string { t.Fatal("none upgraded agent must not read an nspawn binary"); return "" }); err != nil {
		t.Fatal(err)
	}
}
