package agentdeployment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentUpgradeSignalStoreLifecycle(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "signals", "agent-upgrade.json")
	store := NewUpgradeSignalStore(path)
	if err := store.RecordPending("operation-1", "kube1", "instance-1"); err != nil {
		t.Fatalf("recordPending: %v", err)
	}
	if err := store.recordCandidate("/slots/green"); err != nil {
		t.Fatalf("recordCandidate: %v", err)
	}
	if err := store.recordSwitchCommitted(); err != nil {
		t.Fatalf("recordSwitchCommitted: %v", err)
	}
	if err := store.RecordFailure("rolled back"); err != nil {
		t.Fatalf("recordFailure: %v", err)
	}
	signal, err := store.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := UpgradeSignal{
		OperationName:            "operation-1",
		ActiveMachine:            "kube1",
		CandidatePath:            "/slots/green",
		InitiatingDaemonInstance: "instance-1",
		SwitchCommitted:          true,
		RecoveryRequired:         true,
		Failure:                  "rolled back",
	}
	if signal == nil || *signal != want {
		t.Fatalf("signal = %#v, want %#v", signal, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read signal file: %v", err)
	}
	wantJSON := "{\"operationName\":\"operation-1\",\"activeMachine\":\"kube1\",\"candidatePath\":\"/slots/green\",\"initiatingDaemonInstance\":\"instance-1\",\"switchCommitted\":true,\"recoveryRequired\":true,\"failure\":\"rolled back\"}\n"
	if string(data) != wantJSON {
		t.Fatalf("signal file = %q, want %q", data, wantJSON)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("signal mode = %o, want 600", info.Mode().Perm())
	}
	if err := store.Clear(); err != nil {
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
	_, err := NewUpgradeSignalStore(path).Read()
	if err == nil || !strings.Contains(err.Error(), "invalid active machine") {
		t.Fatalf("error = %v", err)
	}
}
