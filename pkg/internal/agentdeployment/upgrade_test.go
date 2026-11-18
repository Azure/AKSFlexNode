package agentdeployment

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// TestHostAgentUpgradeExecutorStageGuardsTheLegacySeedFirst: a host that would
// hand a release before the host root a binary under /usr/local is refused
// before anything is staged, so the operation fails with nothing to roll back.
func TestHostAgentUpgradeExecutorStageGuardsTheLegacySeedFirst(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	guardErr := errors.New("/usr/local/bin/aks-flex-node leads to an executable")
	executor := &HostUpgrade{
		log:             slog.New(slog.DiscardHandler),
		paths:           paths,
		mode:            isolation.ModeNSpawn,
		signals:         NewUpgradeSignalStore(paths.SignalPath),
		guardLegacySeed: func() error { return guardErr },
	}

	err := executor.Stage(t.Context(), UpgradeRequest{DownloadURL: "https://example.test/agent.tar.gz"})
	if !errors.Is(err, guardErr) {
		t.Fatalf("Stage() error = %v, want %v", err, guardErr)
	}
	for _, path := range []string{paths.CurrentPath, paths.LastGoodPath, paths.BluePath, paths.GreenPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stage() changed %s before the guard passed: %v", path, err)
		}
	}
}

func TestHostAgentUpgradeExecutorSchedulesRestartOutsideService(t *testing.T) {
	t.Parallel()

	var calls []string
	executor := &HostUpgrade{
		systemctl: func(_ context.Context, args ...string) error {
			calls = append(calls, "systemctl "+strings.Join(args, " "))
			return nil
		},
		runSystemdRun: func(_ context.Context, args ...string) error {
			joined := strings.Join(args, " ")
			calls = append(calls, "systemd-run "+joined)
			for _, expected := range []string{
				"--collect",
				"--on-active=1000ms",
				// Without it, systemd fires the timer up to a minute late.
				"--timer-property=AccuracySec=100ms",
				"--unit=aks-flex-node-agent-upgrade-restart-",
				"/usr/bin/systemctl restart " + ServiceUnitName,
			} {
				if !strings.Contains(joined, expected) {
					t.Fatalf("systemd-run args %q do not contain %q", joined, expected)
				}
			}
			return nil
		},
	}
	if err := executor.Restart(t.Context()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// The restart doesn't count toward the unit's start limit: its count is reset first.
	if len(calls) != 2 || calls[0] != "systemctl reset-failed "+ServiceUnitName ||
		!strings.HasPrefix(calls[1], "systemd-run ") {
		t.Fatalf("calls = %q, want reset-failed of %s, then the scheduled restart", calls, ServiceUnitName)
	}
}

// A restart is still worth trying when the start limit can't be reset.
func TestHostAgentUpgradeExecutorRestartsThoughItCantResetTheStartLimit(t *testing.T) {
	t.Parallel()

	scheduled := false
	executor := &HostUpgrade{
		log:       discardLogger(),
		systemctl: func(context.Context, ...string) error { return errors.New("access denied") },
		runSystemdRun: func(context.Context, ...string) error {
			scheduled = true
			return nil
		},
	}
	if err := executor.Restart(t.Context()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if !scheduled {
		t.Fatal("the restart wasn't scheduled after reset-failed failed")
	}
}

func TestHostAgentUpgradeExecutorRestartReportsAFailedRestart(t *testing.T) {
	t.Parallel()

	scheduleErr := errors.New("systemd-run failed")
	executor := &HostUpgrade{
		systemctl:     func(context.Context, ...string) error { return nil },
		runSystemdRun: func(context.Context, ...string) error { return scheduleErr },
	}
	if err := executor.Restart(t.Context()); !errors.Is(err, scheduleErr) {
		t.Fatalf("Restart() = %v, want %v", err, scheduleErr)
	}
}

func TestHostAgentUpgradeExecutorRestartRejectsPreCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	executor := &HostUpgrade{
		systemctl: func(context.Context, ...string) error {
			called = true
			return nil
		},
		runSystemdRun: func(context.Context, ...string) error {
			called = true
			return nil
		},
	}
	if err := executor.Restart(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Restart error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("systemctl called with an already canceled context")
	}
}

func TestHostAgentUpgradeExecutorAbortUsesCleanupContext(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for path, content := range map[string]string{paths.BluePath: "blue", paths.GreenPath: "green"} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	if err := os.Symlink(paths.GreenPath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink current: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.LastGoodPath); err != nil {
		t.Fatalf("Symlink last-good: %v", err)
	}
	signals := NewUpgradeSignalStore(paths.SignalPath)
	if err := signals.RecordPending("operation-1", "", "instance-1"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	if err := signals.recordCandidate(paths.GreenPath); err != nil {
		t.Fatalf("recordCandidate: %v", err)
	}
	executor := &HostUpgrade{paths: paths, signals: signals, mode: isolation.ModeNSpawn}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := executor.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	if signal, err := signals.Read(); err != nil || signal != nil {
		t.Fatalf("signal after Abort = %#v, %v", signal, err)
	}
}

func TestHostAgentUpgradeExecutorAbortPreservesSignalOnRollbackFailure(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	signals := NewUpgradeSignalStore(paths.SignalPath)
	if err := signals.RecordPending("operation-1", "", "instance-1"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	if err := signals.recordCandidate(paths.BluePath); err != nil {
		t.Fatalf("recordCandidate: %v", err)
	}
	executor := &HostUpgrade{paths: paths, signals: signals, mode: isolation.ModeNSpawn}
	if err := executor.Abort(t.Context()); err == nil {
		t.Fatal("Abort error = nil")
	}
	if signal, err := signals.Read(); err != nil || signal == nil {
		t.Fatalf("signal after failed Abort = %#v, %v", signal, err)
	}
}

func TestRollbackAgentUpgradeFilesDoesNotDowngradeBeforeSwitch(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for path, content := range map[string]string{paths.BluePath: "last-good", paths.GreenPath: "active"} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	if err := os.Symlink(paths.GreenPath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink current: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.LastGoodPath); err != nil {
		t.Fatalf("Symlink last-good: %v", err)
	}

	if err := rollbackAgentUpgradeFiles(paths, &UpgradeSignal{CandidatePath: paths.BluePath}, isolation.ModeNSpawn, nil); err != nil {
		t.Fatalf("rollbackAgentUpgradeFiles: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
}

func TestRollbackAgentUpgradeFilesRetriesAfterHostLinkWasRestored(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, path := range []string{paths.BluePath, paths.GreenPath} {
		if err := os.WriteFile(path, []byte(path), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	if err := os.Symlink(paths.GreenPath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink current: %v", err)
	}

	err := rollbackAgentUpgradeFiles(paths, &UpgradeSignal{
		CandidatePath:    paths.BluePath,
		RecoveryRequired: true,
	}, isolation.ModeNSpawn, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve last-good") {
		t.Fatalf("rollbackAgentUpgradeFiles error = %v, want retry to resolve last-good", err)
	}
}

func TestFilesHaveEqualSHA256(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("same"), 0o600); err != nil {
		t.Fatalf("WriteFile first: %v", err)
	}
	if err := os.WriteFile(second, []byte("same"), 0o600); err != nil {
		t.Fatalf("WriteFile second: %v", err)
	}
	equal, err := filesHaveEqualSHA256(first, second)
	if err != nil || !equal {
		t.Fatalf("filesHaveEqualSHA256 = %v, %v", equal, err)
	}
	if err := os.WriteFile(second, []byte("different"), 0o600); err != nil {
		t.Fatalf("WriteFile second: %v", err)
	}
	equal, err = filesHaveEqualSHA256(first, second)
	if err != nil || equal {
		t.Fatalf("filesHaveEqualSHA256 = %v, %v", equal, err)
	}
}

// TestAgentUpgradePathsUnder covers the host layout used by agent upgrade.
//
// Under the legacy root it must reproduce the absolute paths released versions
// used. A migrated host resolves the host root to the legacy root, and the
// links and units those versions wrote name these paths.
func TestAgentUpgradePathsUnder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		root         string
		wantBinary   string
		wantBlue     string
		wantCurrent  string
		wantLastGood string
	}{
		{
			name:         "host root",
			root:         "/opt/unbounded/agent",
			wantBinary:   "/opt/unbounded/agent/bin/aks-flex-node",
			wantBlue:     "/opt/unbounded/agent/lib/aks-flex-node/aks-flex-node-blue",
			wantCurrent:  "/opt/unbounded/agent/lib/aks-flex-node/aks-flex-node-current",
			wantLastGood: "/opt/unbounded/agent/lib/aks-flex-node/aks-flex-node-last-good",
		},
		{
			name:         "legacy root keeps the released layout",
			root:         "/usr/local",
			wantBinary:   "/usr/local/bin/aks-flex-node",
			wantBlue:     "/usr/local/lib/aks-flex-node/aks-flex-node-blue",
			wantCurrent:  "/usr/local/lib/aks-flex-node/aks-flex-node-current",
			wantLastGood: "/usr/local/lib/aks-flex-node/aks-flex-node-last-good",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			paths := UpgradePathsUnder(tt.root)
			if paths.BinaryPath != tt.wantBinary {
				t.Errorf("BinaryPath = %q, want %q", paths.BinaryPath, tt.wantBinary)
			}
			if paths.BluePath != tt.wantBlue {
				t.Errorf("BluePath = %q, want %q", paths.BluePath, tt.wantBlue)
			}
			if paths.CurrentPath != tt.wantCurrent {
				t.Errorf("CurrentPath = %q, want %q", paths.CurrentPath, tt.wantCurrent)
			}
			if paths.LastGoodPath != tt.wantLastGood {
				t.Errorf("LastGoodPath = %q, want %q", paths.LastGoodPath, tt.wantLastGood)
			}

			// The signal must stay on a filesystem that is writable even when
			// /usr is read-only, and survive a rollback to a binary that
			// predates the host root, so it does not move with the root.
			if paths.SignalPath != "/etc/aks-flex-node/agent-upgrade-signal.json" {
				t.Errorf("SignalPath = %q, want it to stay under /etc", paths.SignalPath)
			}
		})
	}
}
