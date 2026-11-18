package agentdeployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestHostUpgradeWorkerSynchronization(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			const previous = "#!/bin/sh\necho previous\n"
			writeTestFile(t, paths.BinaryPath, previous)
			marker := filepath.Join(t.TempDir(), "executed")
			candidate := agentUpgradeProbeScript(paths, marker)
			candidate += "if [ \"$1\" = version ]; then exit 0; fi\n"
			if mode == isolation.ModeNone {
				candidate += "test \"$1\" = supports-isolation && test \"$2\" = none\n"
			} else {
				candidate += "exit 1\n"
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
			wantExecutions := fmt.Sprintf("%s|version||%s\n", paths.GreenPath, paths.BluePath)
			if mode == isolation.ModeNone {
				wantExecutions += fmt.Sprintf("%s|supports-isolation|none|%s\n", paths.GreenPath, paths.BluePath)
			}
			assertAgentUpgradeFileContent(t, marker, wantExecutions)
			if mode == isolation.ModeNSpawn {
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
				if mode == isolation.ModeNSpawn {
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

func TestNoneUpgradeRefusesCandidateBeforeSwitch(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		badDigest bool
		wantErr   string
	}{
		"unsupported mode": {
			wantErr: `cannot operate isolation mode "none"`,
		},
		"checksum mismatch": {
			badDigest: true,
			wantErr:   "SHA-256 does not match",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			writeTestFile(t, paths.BinaryPath, "previous")
			marker := filepath.Join(t.TempDir(), "executed")
			candidate := agentUpgradeProbeScript(paths, marker)
			candidate += "if [ \"$1\" = version ]; then exit 0; fi\necho candidate-secret\necho candidate-secret >&2\nexit 1\n"
			archive := agentUpgradeTestArchive(t, candidate)
			digest := sha256.Sum256(archive)
			expectedDigest := hex.EncodeToString(digest[:])
			if tc.badDigest {
				expectedDigest = strings.Repeat("0", 64)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(archive); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			helper := filepath.Join(t.TempDir(), "helper")
			writeExecutable(t, helper, "unchanged")
			executor := &HostUpgrade{
				log:     discardLogger(),
				paths:   paths,
				mode:    isolation.ModeNone,
				signals: NewUpgradeSignalStore(paths.SignalPath),
				helperPaths: map[string]string{
					"kube1": helper,
				},
			}
			if err := executor.signals.RecordPending("upgrade", "kube1", "initiator"); err != nil {
				t.Fatal(err)
			}
			request := UpgradeRequest{
				DownloadURL: server.URL + "?sig=url-secret",
				SHA256:      expectedDigest,
			}

			stageErr := executor.Stage(t.Context(), request)
			if stageErr == nil || !strings.Contains(stageErr.Error(), tc.wantErr) {
				t.Fatalf("Stage error = %v, want %q", stageErr, tc.wantErr)
			}
			if strings.Contains(stageErr.Error(), "secret") {
				t.Fatalf("Stage error exposes secret: %v", stageErr)
			}
			assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
			assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
			assertAgentUpgradeFileContent(t, helper, "unchanged")
			signal, err := executor.signals.Read()
			if err != nil || signal == nil || signal.CandidatePath != paths.GreenPath || signal.SwitchCommitted {
				t.Fatalf("signal after refusal = %#v, %v", signal, err)
			}
			if tc.badDigest {
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("unverified candidate executed: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(stageErr, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("Stage error = %v, want candidate exit status 1", stageErr)
				}
				want := fmt.Sprintf("%s|version||%s\n%s|supports-isolation|none|%s\n", paths.GreenPath, paths.BluePath, paths.GreenPath, paths.BluePath)
				assertAgentUpgradeFileContent(t, marker, want)
			}
			if err := executor.Abort(t.Context()); err != nil {
				t.Fatal(err)
			}
			if signal, err := executor.signals.Read(); err != nil || signal != nil {
				t.Fatalf("signal after abort = %#v, %v", signal, err)
			}
			assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
			assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
			assertAgentUpgradeFileContent(t, helper, "unchanged")
		})
	}
}

func agentUpgradeProbeScript(paths UpgradePaths, marker string) string {
	return fmt.Sprintf(`#!/bin/sh
printf '%%s|%%s|%%s|%%s\n' "$0" "$1" "$2" "$(readlink %q)" >> %q
`, paths.CurrentPath, marker)
}

func TestCheckIsolationSupportPropagatesCancellation(t *testing.T) {
	t.Parallel()

	t.Run("before launch", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := checkIsolationSupport(ctx, "/unused", isolation.ModeNone); !errors.Is(err, context.Canceled) {
			t.Fatalf("checkIsolationSupport error = %v, want context.Canceled", err)
		}
	})

	t.Run("during probe", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		readyPath := filepath.Join(dir, "ready")
		if err := syscall.Mkfifo(readyPath, 0o600); err != nil {
			t.Fatal(err)
		}
		ready, err := os.OpenFile(readyPath, os.O_RDWR, 0) //nolint:gosec // test-only FIFO
		if err != nil {
			t.Fatal(err)
		}
		defer ready.Close()
		candidate := filepath.Join(dir, "candidate")
		writeExecutable(t, candidate, fmt.Sprintf("#!/bin/sh\nprintf ready > %q\nexec sleep 30\n", readyPath))

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			result <- checkIsolationSupport(ctx, candidate, isolation.ModeNone)
		}()
		readyResult := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(ready, make([]byte, len("ready")))
			readyResult <- err
		}()
		select {
		case err := <-result:
			t.Fatalf("probe exited before signaling readiness: %v", err)
		case err := <-readyResult:
			if err != nil {
				t.Fatal(err)
			}
		}

		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("checkIsolationSupport error = %v, want context.Canceled", err)
		}
	})
}
