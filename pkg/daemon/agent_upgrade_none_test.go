package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
			err := installAndSwitchAgentBinary(t.Context(), discardLogger(), server.URL, hex.EncodeToString(sum[:]), paths, mustNodeProvider(t, isolation.ModeNone))
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

func TestAgentUpgradeSignalIsolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode    isolation.Mode
		wantErr bool
	}{
		{mode: ""},
		{mode: isolation.ModeNSpawn},
		{mode: isolation.ModeNone},
		{mode: isolation.ModeContainer},
		{mode: "unknown", wantErr: true},
		{mode: "NONE", wantErr: true},
		{mode: " none ", wantErr: true},
	} {
		t.Run("isolation="+string(tc.mode), func(t *testing.T) {
			t.Parallel()
			store := agentUpgradeSignalStore{path: filepath.Join(t.TempDir(), "signal.json")}
			if err := store.write(agentUpgradeSignal{OperationName: "upgrade", Isolation: tc.mode}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				signal, err := store.read()
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), "AgentUpgrade signal isolation") || !strings.Contains(err.Error(), "unsupported isolation mode") {
						t.Fatalf("read error = %v, want invalid signal isolation", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if signal == nil || signal.Isolation != tc.mode {
					t.Fatalf("signal = %+v, want isolation %q unchanged", signal, tc.mode)
				}
			}
		})
	}
}

func TestNoneAgentUpgradeSignal(t *testing.T) {
	t.Parallel()
	paths := testAgentUpgradePaths(t)
	store := agentUpgradeSignalStore{path: paths.SignalPath}
	loader := &fakeNodeOperator{state: &State{Isolation: isolation.ModeNone}}
	if err := recordPendingAgentUpgrade(t.Context(), store, loader, "upgrade", "daemon", mustNodeProvider(t, "none")); err != nil {
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
	if err := validateStartedAgentUpgrade(paths, signal, mustNodeProvider(t, "none")); err != nil {
		t.Fatal(err)
	}
}
