package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

func TestNewDaemonInstanceID(t *testing.T) {
	t.Parallel()

	first, err := newDaemonInstanceID()
	if err != nil {
		t.Fatalf("newDaemonInstanceID: %v", err)
	}
	second, err := newDaemonInstanceID()
	if err != nil {
		t.Fatalf("newDaemonInstanceID: %v", err)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("instance IDs = %q, %q", first, second)
	}
}

func TestParseAgentUpgradeRequest(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		parameters map[string]string
		wantErr    string
	}{
		"valid": {
			parameters: map[string]string{
				agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz?sig=secret",
				agentUpgradeSHA256Parameter:      strings.Repeat("a", 64),
			},
		},
		"missing URL": {
			parameters: map[string]string{agentUpgradeSHA256Parameter: strings.Repeat("a", 64)},
			wantErr:    agentUpgradeDownloadURLParameter,
		},
		"HTTP URL": {
			parameters: map[string]string{
				agentUpgradeDownloadURLParameter: "http://example.com/agent.tar.gz",
				agentUpgradeSHA256Parameter:      strings.Repeat("a", 64),
			},
			wantErr: "HTTPS",
		},
		"missing digest": {
			parameters: map[string]string{agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz"},
			wantErr:    agentUpgradeSHA256Parameter,
		},
		"invalid digest": {
			parameters: map[string]string{
				agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
				agentUpgradeSHA256Parameter:      "bad",
			},
			wantErr: "64 hexadecimal",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseAgentUpgradeRequest(tt.parameters)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseAgentUpgradeRequest: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestHostAgentUpgradeExecutorRecordPendingIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent-upgrade.json")
	executor := &hostAgentUpgradeExecutor{
		state:   &fakeNodeOperator{state: &State{ActiveMachine: "kube1"}},
		signals: agentUpgradeSignalStore{path: path},
	}
	if err := executor.RecordPending(t.Context(), "operation-1"); err != nil {
		t.Fatalf("first RecordPending: %v", err)
	}
	if err := executor.RecordPending(t.Context(), "operation-1"); !errors.Is(err, errAgentUpgradeAlreadyPending) {
		t.Fatalf("second RecordPending error = %v, want errAgentUpgradeAlreadyPending", err)
	}
	if err := executor.RecordPending(t.Context(), "operation-2"); err == nil || !strings.Contains(err.Error(), "operation-1") {
		t.Fatalf("competing RecordPending error = %v", err)
	}
}

func TestAgentUpgradeSignalStoreLifecycle(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "signals", "agent-upgrade.json")
	store := agentUpgradeSignalStore{path: path}
	if err := store.recordPending("operation-1", "kube1", "instance-1"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	if err := store.recordCandidate("/slots/green"); err != nil {
		t.Fatalf("recordCandidate: %v", err)
	}
	if err := store.recordFailure("rolled back"); err != nil {
		t.Fatalf("recordFailure: %v", err)
	}
	signal, err := store.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if signal == nil || signal.OperationName != "operation-1" || signal.ActiveMachine != "kube1" || signal.CandidatePath != "/slots/green" || signal.InitiatingDaemonInstance != "instance-1" || signal.Failure != "rolled back" || !signal.RecoveryRequired {
		t.Fatalf("signal = %#v", signal)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("signal mode = %o, want 600", info.Mode().Perm())
	}
	if err := store.clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("signal still exists: %v", err)
	}
}

func TestAgentUpgradeSignalStoreRejectsInvalidMachine(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "signal.json")
	if err := os.WriteFile(path, []byte(`{"operationName":"operation-1","activeMachine":"../../etc"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := (agentUpgradeSignalStore{path: path}).read()
	if err == nil || !strings.Contains(err.Error(), "invalid active machine") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostAgentUpgradeExecutorSchedulesRestartOutsideService(t *testing.T) {
	t.Parallel()

	executor := &hostAgentUpgradeExecutor{
		runSystemdRun: func(_ context.Context, args ...string) error {
			joined := strings.Join(args, " ")
			for _, expected := range []string{
				"--collect",
				"--on-active=1s",
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
}

func TestHostAgentUpgradeExecutorRestartRejectsPreCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	executor := &hostAgentUpgradeExecutor{runSystemdRun: func(context.Context, ...string) error {
		called = true
		return nil
	}}
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
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := signals.recordPending("operation-1", "", "instance-1"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	executor := &hostAgentUpgradeExecutor{paths: paths, signals: signals, instanceID: "instance-1"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := executor.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	if signal, err := signals.read(); err != nil || signal != nil {
		t.Fatalf("signal after Abort = %#v, %v", signal, err)
	}
}

func TestHostAgentUpgradeExecutorAbortPreservesSignalOnRollbackFailure(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := signals.recordPending("operation-1", "", "instance-1"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	executor := &hostAgentUpgradeExecutor{paths: paths, signals: signals, instanceID: "instance-1"}
	if err := executor.Abort(t.Context()); err == nil {
		t.Fatal("Abort error = nil")
	}
	if signal, err := signals.read(); err != nil || signal == nil {
		t.Fatalf("signal after failed Abort = %#v, %v", signal, err)
	}
}

func TestPublishAgentUpgradeSignalPreservesSignalWhenRollbackFails(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.BluePath, []byte("candidate"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := signals.write(agentUpgradeSignal{
		OperationName:            "operation-1",
		ActiveMachine:            "kube1",
		CandidatePath:            paths.BluePath,
		InitiatingDaemonInstance: "other-instance",
	}); err != nil {
		t.Fatalf("write signal: %v", err)
	}
	executor := &hostAgentUpgradeExecutor{paths: paths, signals: signals, instanceID: "current-instance"}
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), nil, executor); err == nil {
		t.Fatal("publishAndClearAgentUpgradeSignal error = nil")
	}
	if signal, err := signals.read(); err != nil || signal == nil {
		t.Fatalf("signal after failed rollback = %#v, %v", signal, err)
	}
}

func TestPublishAgentUpgradeFailureRestartsIntoLastGoodBeforeClearingSignal(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for path, content := range map[string]string{paths.BluePath: "candidate", paths.GreenPath: "last-good"} {
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("Symlink current: %v", err)
	}
	if err := os.Symlink(paths.GreenPath, paths.LastGoodPath); err != nil {
		t.Fatalf("Symlink last-good: %v", err)
	}
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := signals.write(agentUpgradeSignal{
		OperationName:            "operation-1",
		CandidatePath:            paths.BluePath,
		InitiatingDaemonInstance: "initiator",
		RecoveryRequired:         true,
		Failure:                  "candidate failed",
	}); err != nil {
		t.Fatalf("write signal: %v", err)
	}
	restarts := 0
	finished := 0
	executor := &hostAgentUpgradeExecutor{
		paths:      paths,
		signals:    signals,
		instanceID: "candidate-instance",
		runSystemdRun: func(context.Context, ...string) error {
			restarts++
			return nil
		},
		finishMachineOperation: func(_ context.Context, _ client.Client, _ agentdaemon.MachineOperation, result agentdaemon.MachineOperationResult[int64]) error {
			finished++
			if result.Phase != machinav1alpha3.OperationPhaseFailed {
				t.Fatalf("phase = %s, want Failed", result.Phase)
			}
			return nil
		},
		runningExecutable: func() (string, error) { return paths.BluePath, nil },
	}
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), nil, executor); err != nil {
		t.Fatalf("publish candidate recovery: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
	if signal, err := signals.read(); err != nil || signal == nil {
		t.Fatalf("signal cleared before last-good startup: %#v, %v", signal, err)
	}
	if restarts != 1 || finished != 1 {
		t.Fatalf("restarts = %d, finished = %d", restarts, finished)
	}

	executor.instanceID = "last-good-instance"
	executor.runningExecutable = func() (string, error) { return paths.GreenPath, nil }
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), nil, executor); err != nil {
		t.Fatalf("publish last-good recovery: %v", err)
	}
	if signal, err := signals.read(); err != nil || signal != nil {
		t.Fatalf("signal after last-good startup = %#v, %v", signal, err)
	}
	if restarts != 1 || finished != 2 {
		t.Fatalf("restarts = %d, finished = %d", restarts, finished)
	}
}

func TestPublishAgentUpgradeSignalIgnoresInitiatingProcess(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := signals.recordPending("operation-1", "kube1", "current-instance"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	executor := &hostAgentUpgradeExecutor{paths: paths, signals: signals, instanceID: "current-instance"}
	if err := publishAndClearAgentUpgradeSignal(t.Context(), slog.Default(), nil, executor); err != nil {
		t.Fatalf("publishAndClearAgentUpgradeSignal: %v", err)
	}
	if signal, err := signals.read(); err != nil || signal == nil {
		t.Fatalf("initiating process consumed signal: %#v, %v", signal, err)
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
