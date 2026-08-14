package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	stateFileMode = 0o600
	stateFileName = "daemon-state.json"
)

// State records the current and previous safely applied AKS machine goals and
// the active nspawn machine.
type State struct {
	AppliedGoal         *aksmachine.GoalState `json:"appliedGoal,omitempty"`
	PreviousAppliedGoal *aksmachine.GoalState `json:"previousAppliedGoal,omitempty"`

	// Deprecated: these projections keep state readable by older agent binaries.
	// AppliedGoal and PreviousAppliedGoal remain authoritative.
	AppliedSettingsVersion    string `json:"appliedSettingsVersion,omitempty"`
	AppliedKubernetesVersion  string `json:"appliedKubernetesVersion,omitempty"`
	PreviousSettingsVersion   string `json:"previousSettingsVersion,omitempty"`
	PreviousKubernetesVersion string `json:"previousKubernetesVersion,omitempty"`

	ActiveMachine string `json:"activeMachine,omitempty"`
}

func (s *State) validate() error {
	if s == nil {
		return fmt.Errorf("daemon state is nil")
	}
	// SettingsVersion can be empty when best-effort Machine registration fails,
	// but the applied goal itself is still required to restart safely.
	if s.AppliedGoal == nil {
		return fmt.Errorf("daemon state applied goal is missing")
	}
	return nil
}

type saveStateTask struct {
	store stateStore
	state *State
}

func saveState(store stateStore, state *State) phases.Task {
	return &saveStateTask{store: store, state: state}
}

func (t *saveStateTask) Name() string { return "save-daemon-state" }

func (t *saveStateTask) Do(ctx context.Context) error {
	if t.state == nil {
		return fmt.Errorf("daemon state is nil")
	}
	if t.store == nil {
		return fmt.Errorf("state store is nil")
	}
	if err := t.store.Save(ctx, t.state); err != nil {
		return fmt.Errorf("save daemon state: %w", err)
	}
	return nil
}

func SeededState(goal aksmachine.GoalState) *State {
	return &State{
		AppliedGoal:   cloneGoalState(goal),
		ActiveMachine: goalstates.NSpawnMachineKube1,
	}
}

func cloneGoalState(goal aksmachine.GoalState) *aksmachine.GoalState {
	cloned := goal
	cloned.NodeLabels = maps.Clone(goal.NodeLabels)
	cloned.NodeTaints = slices.Clone(goal.NodeTaints)
	return &cloned
}

func (s *State) migrateLegacyGoals() {
	if s.AppliedGoal == nil && (s.AppliedSettingsVersion != "" || s.AppliedKubernetesVersion != "") {
		s.AppliedGoal = &aksmachine.GoalState{
			SettingsVersion:   s.AppliedSettingsVersion,
			KubernetesVersion: s.AppliedKubernetesVersion,
		}
	}
	if s.PreviousAppliedGoal == nil && (s.PreviousSettingsVersion != "" || s.PreviousKubernetesVersion != "") {
		s.PreviousAppliedGoal = &aksmachine.GoalState{
			SettingsVersion:   s.PreviousSettingsVersion,
			KubernetesVersion: s.PreviousKubernetesVersion,
		}
	}
}

func (s *State) populateLegacyFields() {
	s.AppliedSettingsVersion = ""
	s.AppliedKubernetesVersion = ""
	if s.AppliedGoal != nil {
		s.AppliedSettingsVersion = s.AppliedGoal.SettingsVersion
		s.AppliedKubernetesVersion = s.AppliedGoal.KubernetesVersion
	}

	s.PreviousSettingsVersion = ""
	s.PreviousKubernetesVersion = ""
	if s.PreviousAppliedGoal != nil {
		s.PreviousSettingsVersion = s.PreviousAppliedGoal.SettingsVersion
		s.PreviousKubernetesVersion = s.PreviousAppliedGoal.KubernetesVersion
	}
}

func validActiveMachine(machine string) bool {
	return machine == goalstates.NSpawnMachineKube1 || machine == goalstates.NSpawnMachineKube2
}

func activeMachineFromStore(ctx context.Context, store stateStore) (*activeMachine, error) {
	state, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, fmt.Errorf("daemon state is missing active machine")
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	if !validActiveMachine(state.ActiveMachine) {
		return nil, fmt.Errorf("daemon state active machine %q is invalid", state.ActiveMachine)
	}
	return &activeMachine{Name: state.ActiveMachine, State: state}, nil
}

type stateStore interface {
	Load(ctx context.Context) (*State, error)
	Save(ctx context.Context, state *State) error
	Delete(ctx context.Context) error
}

func NewFileStateStore() (stateStore, error) {
	return newFileStateStore("")
}

type fileStateStore struct {
	path string
}

func newFileStateStore(path string) (*fileStateStore, error) {
	if path == "" {
		path = filepath.Join(config.ConfigDir, stateFileName)
	}
	return &fileStateStore{path: path}, nil
}

func (s *fileStateStore) Load(context.Context) (*State, error) {
	data, err := os.ReadFile(filepath.Clean(s.path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read daemon state %s: %w", s.path, err)
	}

	checksumData, err := os.ReadFile(filepath.Clean(s.checksumPath()))
	if err != nil {
		return nil, fmt.Errorf("read daemon state checksum %s: %w", s.checksumPath(), err)
	}
	if got, want := strings.TrimSpace(string(checksumData)), checksum(data); got != want {
		return nil, fmt.Errorf("daemon state checksum mismatch for %s", s.path)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode daemon state %s: %w", s.path, err)
	}
	state.migrateLegacyGoals()
	if err := state.validate(); err != nil {
		return nil, fmt.Errorf("validate daemon state %s: %w", s.path, err)
	}
	state.populateLegacyFields()
	return &state, nil
}

func (s *fileStateStore) Save(_ context.Context, state *State) error {
	if err := state.validate(); err != nil {
		return err
	}
	stateForPersistence := *state
	stateForPersistence.populateLegacyFields()
	data, err := json.MarshalIndent(&stateForPersistence, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal daemon state: %w", err)
	}
	data = append(data, '\n')
	if err := utilio.WriteFile(s.path, data, stateFileMode); err != nil {
		return fmt.Errorf("write daemon state %s: %w", s.path, err)
	}
	if err := utilio.WriteFile(s.checksumPath(), []byte(checksum(data)+"\n"), stateFileMode); err != nil {
		return fmt.Errorf("write daemon state checksum %s: %w", s.checksumPath(), err)
	}
	return nil
}

func (s *fileStateStore) Delete(context.Context) error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon state %s: %w", s.path, err)
	}
	if err := os.Remove(s.checksumPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon state checksum %s: %w", s.checksumPath(), err)
	}
	return nil
}

func (s *fileStateStore) checksumPath() string {
	return s.path + ".sha256"
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
