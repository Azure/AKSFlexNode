package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

func TestFileStateStoreSaveLoad(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	want := &State{
		AppliedSettingsVersion:    "42",
		AppliedKubernetesVersion:  "1.34.0",
		PreviousSettingsVersion:   "41",
		PreviousKubernetesVersion: "1.33.0",
		ActiveMachine:             "kube2",
	}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AppliedSettingsVersion != want.AppliedSettingsVersion || got.ActiveMachine != want.ActiveMachine {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestFileStateStoreLoadMissing(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Fatalf("state = %#v, want nil", got)
	}
}

func TestFileStateStoreChecksumMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &State{AppliedSettingsVersion: "42"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"appliedSettingsVersion":"43"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = store.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Load error = %v, want checksum mismatch", err)
	}
}

func TestFileStateStoreCorruptJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	data := []byte(`{"appliedSettingsVersion":`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(path+".sha256", []byte(checksum(data)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile checksum: %v", err)
	}
	_, err = store.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode daemon state") {
		t.Fatalf("Load error = %v, want decode error", err)
	}
}

func TestFileStateStoreDelete(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &State{AppliedSettingsVersion: "42"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file exists after Delete: %v", err)
	}
	if _, err := os.Stat(path + ".sha256"); !os.IsNotExist(err) {
		t.Fatalf("checksum file exists after Delete: %v", err)
	}
}

func TestSeededState(t *testing.T) {
	t.Parallel()

	state := SeededState(aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}, "kube1")
	if state.AppliedSettingsVersion != "42" {
		t.Fatalf("AppliedSettingsVersion = %q, want 42", state.AppliedSettingsVersion)
	}
	if state.AppliedKubernetesVersion != "1.34.0" {
		t.Fatalf("AppliedKubernetesVersion = %q, want 1.34.0", state.AppliedKubernetesVersion)
	}
	if state.ActiveMachine != "kube1" {
		t.Fatalf("ActiveMachine = %q, want kube1", state.ActiveMachine)
	}
	if state.PreviousSettingsVersion != "" || state.PreviousKubernetesVersion != "" {
		t.Fatalf("previous state = %#v, want empty", state)
	}
}

func TestSeedStateTaskValidation(t *testing.T) {
	t.Parallel()

	if err := NewSeedStateTask(nil, "kube1").Do(t.Context()); err == nil {
		t.Fatalf("seedStateTask nil client error = nil")
	}
	if err := NewSeedStateTask(&fakeMachineClient{}, "kube3").Do(t.Context()); err == nil {
		t.Fatalf("seedStateTask invalid active machine error = nil")
	}
	if task := NewSeedStateTask(&fakeMachineClient{}, "kube1"); task.Name() != "seed-daemon-state" {
		t.Fatalf("Name = %q, want seed-daemon-state", task.Name())
	}
}

func TestActiveMachineFromStore(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state   *State
		want    string
		wantErr bool
	}{
		"kube1": {
			state: &State{ActiveMachine: "kube1"},
			want:  "kube1",
		},
		"kube2": {
			state: &State{ActiveMachine: "kube2"},
			want:  "kube2",
		},
		"missing state": {
			wantErr: true,
		},
		"invalid active machine": {
			state:   &State{ActiveMachine: "kube3"},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := activeMachineFromStore(t.Context(), &testStateStore{state: tt.state})
			if tt.wantErr {
				if err == nil {
					t.Fatal("activeMachineFromStore error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("activeMachineFromStore: %v", err)
			}
			if got.Name != tt.want {
				t.Fatalf("machine = %q, want %q", got.Name, tt.want)
			}
		})
	}
}
