package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStateStoreSaveLoad(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	want := &State{
		AppliedGoal:         testGoalState("1.34.0", "42").DeepCopy(),
		PreviousAppliedGoal: testGoalState("1.33.0", "41").DeepCopy(),
		ActiveMachine:       "kube2",
	}
	want.AppliedGoal.NodeLabels = map[string]string{"workload": "flex"}
	want.AppliedGoal.NodeTaints = []string{"dedicated=flex:NoSchedule"}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ActiveMachine != want.ActiveMachine || got.AppliedGoal == nil || got.AppliedGoal.NodeLabels["workload"] != "flex" ||
		got.PreviousAppliedGoal == nil || got.PreviousAppliedGoal.SettingsVersion != "41" ||
		got.AppliedSettingsVersion != "42" || got.AppliedKubernetesVersion != "1.34.0" {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	persistedData, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var persisted State
	if err := json.Unmarshal(persistedData, &persisted); err != nil {
		t.Fatalf("Unmarshal persisted state: %v", err)
	}
	if persisted.AppliedSettingsVersion != "42" || persisted.PreviousSettingsVersion != "41" {
		t.Fatalf("legacy state projections = %#v", persisted)
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
		data  string
		check func(*testing.T, *State)
	}{
		"legacy state remains partial": {
			data: `{
				"appliedSettingsVersion":"42",
				"appliedKubernetesVersion":"1.34.0",
				"previousSettingsVersion":"41",
				"previousKubernetesVersion":"1.33.0",
				"activeMachine":"kube1"
			}`,
			check: func(t *testing.T, state *State) {
				t.Helper()
				if state.AppliedGoal != nil || state.PreviousAppliedGoal != nil {
					t.Fatalf("legacy goals were fabricated: %#v", state)
				}
				if state.AppliedSettingsVersion != "42" || state.AppliedKubernetesVersion != "1.34.0" {
					t.Fatalf("legacy projections = %#v", state)
				}
			},
		},
		"complete goals override stale projections": {
			data: `{
				"appliedGoal":{"kubernetesVersion":"1.34.0","settingsVersion":"42","maxPods":110,"kubeletConfig":{"imageGCHighThreshold":85,"imageGCLowThreshold":80}},
				"previousAppliedGoal":{"kubernetesVersion":"1.33.0","settingsVersion":"41","maxPods":110,"kubeletConfig":{"imageGCHighThreshold":85,"imageGCLowThreshold":80}},
				"appliedSettingsVersion":"stale",
				"appliedKubernetesVersion":"1.99.0",
				"previousSettingsVersion":"stale",
				"previousKubernetesVersion":"1.98.0",
				"activeMachine":"kube2"
			}`,
			check: func(t *testing.T, state *State) {
				t.Helper()
				if state.AppliedGoal == nil || state.AppliedGoal.SettingsVersion != "42" || state.PreviousAppliedGoal == nil {
					t.Fatalf("complete goals = %#v", state)
				}
				if state.AppliedSettingsVersion != "42" || state.AppliedKubernetesVersion != "1.34.0" ||
					state.PreviousSettingsVersion != "41" || state.PreviousKubernetesVersion != "1.33.0" {
					t.Fatalf("legacy projections were not corrected: %#v", state)
				}
			},
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

			state, err := store.Load(t.Context())
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, state)
		})
	}
}

func TestFileStateStoreChecksumMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.34.0"}); err != nil {
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
	if err := store.Save(context.Background(), &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.34.0"}); err != nil {
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

	goal := testGoalState("1.34.0", "42")
	goal.NodeLabels = map[string]string{"workload": "flex"}
	state := SeededState(goal)
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
	if state.AppliedGoal == nil || state.AppliedGoal.NodeLabels["workload"] != "flex" {
		t.Fatalf("AppliedGoal = %#v", state.AppliedGoal)
	}
	goal.NodeLabels["workload"] = "changed"
	if state.AppliedGoal.NodeLabels["workload"] != "flex" {
		t.Fatal("SeededState retained caller-owned label map")
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
			state: &State{AppliedKubernetesVersion: "1.34.0", ActiveMachine: "kube1"},
			want:  "kube1",
		},
		"kube2": {
			state: &State{AppliedKubernetesVersion: "1.34.0", ActiveMachine: "kube2"},
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
