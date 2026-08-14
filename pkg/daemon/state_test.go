package daemon

import (
	"context"
	"encoding/json"
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
		AppliedGoal: &aksmachine.GoalState{
			KubernetesVersion: "1.34.0",
			SettingsVersion:   "42",
			NodeLabels:        map[string]string{"workload": "flex"},
			NodeTaints:        []string{"dedicated=flex:NoSchedule"},
		},
		PreviousAppliedGoal: &aksmachine.GoalState{
			KubernetesVersion: "1.33.0",
			SettingsVersion:   "41",
		},
		AppliedSettingsVersion:    "stale-applied",
		AppliedKubernetesVersion:  "stale-applied",
		PreviousSettingsVersion:   "stale-previous",
		PreviousKubernetesVersion: "stale-previous",
		ActiveMachine:             "kube2",
	}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	persistedData, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var persisted State
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatalf("Unmarshal persisted state: %v", err)
	}
	if persisted.AppliedSettingsVersion != "42" || persisted.AppliedKubernetesVersion != "1.34.0" ||
		persisted.PreviousSettingsVersion != "41" || persisted.PreviousKubernetesVersion != "1.33.0" {
		t.Fatalf("legacy projections = %#v", persisted)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ActiveMachine != want.ActiveMachine ||
		got.AppliedGoal == nil || got.AppliedGoal.NodeLabels["workload"] != "flex" || len(got.AppliedGoal.NodeTaints) != 1 ||
		got.PreviousAppliedGoal == nil || got.PreviousAppliedGoal.SettingsVersion != "41" ||
		got.AppliedSettingsVersion != "42" || got.PreviousSettingsVersion != "41" {
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

func TestFileStateStoreLoadCompatibility(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		data    string
		wantErr string
		check   func(*testing.T, *State)
	}{
		"legacy state is migrated": {
			data: `{
				"appliedSettingsVersion":"42",
				"appliedKubernetesVersion":"1.34.0",
				"previousSettingsVersion":"41",
				"previousKubernetesVersion":"1.33.0",
				"activeMachine":"kube1"
			}`,
			check: func(t *testing.T, state *State) {
				t.Helper()
				if state.AppliedGoal == nil || state.AppliedGoal.SettingsVersion != "42" || state.AppliedGoal.KubernetesVersion != "1.34.0" {
					t.Fatalf("AppliedGoal = %#v, want migrated legacy goal", state.AppliedGoal)
				}
				if state.PreviousAppliedGoal == nil || state.PreviousAppliedGoal.SettingsVersion != "41" || state.PreviousAppliedGoal.KubernetesVersion != "1.33.0" {
					t.Fatalf("PreviousAppliedGoal = %#v, want migrated legacy goal", state.PreviousAppliedGoal)
				}
			},
		},
		"full goals override stale legacy projections": {
			data: `{
				"appliedGoal":{"kubernetesVersion":"1.34.0","settingsVersion":"42","nodeLabels":{"workload":"flex"}},
				"previousAppliedGoal":{"kubernetesVersion":"1.33.0","settingsVersion":"41"},
				"appliedSettingsVersion":"99",
				"appliedKubernetesVersion":"1.99.0",
				"previousSettingsVersion":"98",
				"previousKubernetesVersion":"1.98.0",
				"activeMachine":"kube2"
			}`,
			check: func(t *testing.T, state *State) {
				t.Helper()
				if state.AppliedGoal == nil || state.AppliedGoal.SettingsVersion != "42" || state.AppliedGoal.NodeLabels["workload"] != "flex" {
					t.Fatalf("AppliedGoal = %#v, want complete goal", state.AppliedGoal)
				}
				if state.AppliedSettingsVersion != "42" || state.AppliedKubernetesVersion != "1.34.0" ||
					state.PreviousSettingsVersion != "41" || state.PreviousKubernetesVersion != "1.33.0" {
					t.Fatalf("legacy projections = %#v, want values derived from full goals", state)
				}
			},
		},
		"state without either format is rejected": {
			data:    `{"activeMachine":"kube1"}`,
			wantErr: "daemon state applied goal is missing",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "state.json")
			store, err := newFileStateStore(path)
			if err != nil {
				t.Fatalf("newFileStateStore: %v", err)
			}
			data := []byte(tt.data)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if err := os.WriteFile(path+".sha256", []byte(checksum(data)+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile checksum: %v", err)
			}

			got, err := store.Load(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestFileStateStoreAllowsAppliedGoalWithoutSettingsVersion(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	want := &State{
		AppliedGoal:   &aksmachine.GoalState{KubernetesVersion: "1.34.0"},
		ActiveMachine: "kube1",
	}
	if err := store.Save(t.Context(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(t.Context())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AppliedGoal == nil || got.AppliedGoal.KubernetesVersion != "1.34.0" || got.AppliedGoal.SettingsVersion != "" {
		t.Fatalf("AppliedGoal = %#v, want goal with empty settings version", got.AppliedGoal)
	}
}

func TestFileStateStoreRejectsSaveWithoutAppliedGoal(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	err = store.Save(t.Context(), &State{ActiveMachine: "kube1"})
	if err == nil || !strings.Contains(err.Error(), "daemon state applied goal is missing") {
		t.Fatalf("Save error = %v, want missing applied goal", err)
	}
}

func TestFileStateStoreChecksumMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"appliedGoal":{"settingsVersion":"43"}}`), 0o600); err != nil {
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
	data := []byte(`{"appliedGoal":`)
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
	if err := store.Save(context.Background(), &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}}); err != nil {
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

	goal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		SettingsVersion:   "42",
		NodeLabels:        map[string]string{"workload": "flex"},
		NodeTaints:        []string{"dedicated=flex:NoSchedule"},
	}
	state := SeededState(goal)
	if state.AppliedGoal == nil || state.AppliedGoal.SettingsVersion != "42" || state.AppliedGoal.KubernetesVersion != "1.34.0" {
		t.Fatalf("AppliedGoal = %#v", state.AppliedGoal)
	}
	if state.ActiveMachine != "kube1" {
		t.Fatalf("ActiveMachine = %q, want kube1", state.ActiveMachine)
	}
	if state.PreviousAppliedGoal != nil {
		t.Fatalf("PreviousAppliedGoal = %#v, want nil", state.PreviousAppliedGoal)
	}
	if state.AppliedGoal == nil || state.AppliedGoal.NodeLabels["workload"] != "flex" || len(state.AppliedGoal.NodeTaints) != 1 {
		t.Fatalf("AppliedGoal = %#v, want complete goal", state.AppliedGoal)
	}
}

func TestSaveStateValidation(t *testing.T) {
	t.Parallel()

	if err := saveState(nil, &State{}).Do(t.Context()); err == nil {
		t.Fatalf("SaveState nil store error = nil")
	}
	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := saveState(store, nil).Do(t.Context()); err == nil {
		t.Fatalf("SaveState nil state error = nil")
	}
	if task := saveState(store, &State{}); task.Name() != "save-daemon-state" {
		t.Fatalf("Name = %q, want save-daemon-state", task.Name())
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
			state: &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}, ActiveMachine: "kube1"},
			want:  "kube1",
		},
		"kube2": {
			state: &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}, ActiveMachine: "kube2"},
			want:  "kube2",
		},
		"missing state": {
			wantErr: true,
		},
		"missing applied goal": {
			state:   &State{ActiveMachine: "kube1"},
			wantErr: true,
		},
		"invalid active machine": {
			state:   &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}, ActiveMachine: "kube3"},
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
