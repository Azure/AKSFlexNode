package agentdeployment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestHostUpgradeWorkerSynchronization(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			const previous = "#!/bin/sh\necho previous\n"
			writeTestFile(t, paths.BinaryPath, previous)
			// An older nspawn candidate need not implement supports-isolation.
			candidate := "#!/bin/sh\ntest \"$1\" = version\n"
			if mode != isolation.ModeNSpawn {
				candidate = fmt.Sprintf("#!/bin/sh\ncase \"$1\" in version) echo v1;; supports-isolation) test \"$2\" = %s;; *) exit 1;; esac\n", mode)
			}
			archive := agentUpgradeTestArchive(t, candidate)
			digest := sha256.Sum256(archive)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(archive); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			slot := "kube2"
			if mode == isolation.ModeNone {
				slot = "kube1"
			}
			helper := filepath.Join(t.TempDir(), "worker", "agent")
			executor := &HostUpgrade{
				log:         discardLogger(),
				paths:       paths,
				mode:        mode,
				signals:     NewUpgradeSignalStore(paths.SignalPath),
				helperPaths: map[string]string{slot: helper},
			}
			if err := executor.signals.RecordPending("upgrade", slot, "initiator"); err != nil {
				t.Fatal(err)
			}
			request := UpgradeRequest{DownloadURL: server.URL, SHA256: hex.EncodeToString(digest[:])}
			if err := executor.Validate(request); err != nil {
				t.Fatal(err)
			}
			if err := executor.Stage(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			signal, err := executor.signals.Read()
			if err != nil || signal == nil || !signal.SwitchCommitted {
				t.Fatalf("staged signal = %#v, %v", signal, err)
			}
			if err := executor.ValidateStarted(signal); err != nil {
				t.Fatalf("validate candidate: %v", err)
			}
			assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
			assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
			if mode != isolation.ModeNone {
				assertAgentUpgradeFileContent(t, helper, candidate)
				writeTestFile(t, helper, "stale helper")
				if err := executor.ValidateStarted(signal); err == nil || !strings.Contains(err.Error(), "do not match") {
					t.Fatalf("stale helper validation = %v", err)
				}
			}

			for range 2 {
				if err := executor.RollbackSwitch(signal); err != nil {
					t.Fatalf("rollback: %v", err)
				}
				assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
				if mode != isolation.ModeNone {
					assertAgentUpgradeFileContent(t, helper, previous)
				} else if _, err := os.Stat(filepath.Dir(helper)); !os.IsNotExist(err) {
					t.Fatalf("none upgrade touched guest helper destination: %v", err)
				}
			}
			if err := executor.Abort(t.Context()); err != nil {
				t.Fatal(err)
			}
			if signal, err := executor.signals.Read(); err != nil || signal != nil {
				t.Fatalf("signal after abort = %#v, %v", signal, err)
			}
		})
	}
}

func TestNoneInstallerVerifiesChecksumBeforeExecutingCandidate(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "executed")
	archive := agentUpgradeTestArchive(t, fmt.Sprintf("#!/bin/sh\necho executed > %q\nexit 0\n", marker))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(archive); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	paths := testAgentUpgradePaths(t)
	writeTestFile(t, paths.BinaryPath, "previous")
	if err := ensureUpgradeLayout(t.Context(), discardLogger(), paths); err != nil {
		t.Fatal(err)
	}
	if err := installAndSwitchBinary(t.Context(), discardLogger(), server.URL, strings.Repeat("0", 64), paths, isolation.ModeNone); err == nil || !strings.Contains(err.Error(), "SHA-256 does not match") {
		t.Fatalf("checksum error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unverified candidate executed: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
}
