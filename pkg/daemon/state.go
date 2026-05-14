package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	stateFileMode = 0o600
	stateFileName = "daemon-state.json"
)

// State records the last safely applied AKS machine goal and the previous
// known-good goal needed for rollback-oriented reconciliation.
type State struct {
	AppliedSettingsVersion    string `json:"appliedSettingsVersion,omitempty"`
	AppliedKubernetesVersion  string `json:"appliedKubernetesVersion,omitempty"`
	PreviousSettingsVersion   string `json:"previousSettingsVersion,omitempty"`
	PreviousKubernetesVersion string `json:"previousKubernetesVersion,omitempty"`
	ActiveMachine             string `json:"activeMachine,omitempty"`
}

type seedStateTask struct {
	machines      aksmachine.MachineClient
	activeMachine string
}

func NewSeedStateTask(machines aksmachine.MachineClient, activeMachine string) *seedStateTask {
	return &seedStateTask{machines: machines, activeMachine: activeMachine}
}

func (t *seedStateTask) Name() string { return "seed-daemon-state" }

// Do records the bootstrap-applied goal so the daemon can reconcile from
// persisted state instead of live machinectl discovery after systemd starts it.
func (t *seedStateTask) Do(ctx context.Context) error {
	if t.machines == nil {
		return fmt.Errorf("machine client is nil")
	}
	if !validActiveMachine(t.activeMachine) {
		return fmt.Errorf("active machine %q is invalid", t.activeMachine)
	}
	machine, err := t.machines.Get(ctx)
	if err != nil {
		return fmt.Errorf("get AKS machine for daemon state seed: %w", err)
	}
	store, err := newFileStateStore("")
	if err != nil {
		return err
	}
	return store.Save(ctx, SeededState(machine.Goal, t.activeMachine))
}

func SeededState(goal aksmachine.GoalState, activeMachine string) *State {
	return &State{
		AppliedSettingsVersion:   goal.SettingsVersion,
		AppliedKubernetesVersion: goal.KubernetesVersion,
		ActiveMachine:            activeMachine,
	}
}

func validActiveMachine(machine string) bool {
	return machine == goalstates.NSpawnMachineKube1 || machine == goalstates.NSpawnMachineKube2
}

func (s State) AppliedGoal() aksmachine.GoalState {
	return aksmachine.GoalState{
		KubernetesVersion: s.AppliedKubernetesVersion,
		SettingsVersion:   s.AppliedSettingsVersion,
	}
}

type stateStore interface {
	Load(ctx context.Context) (*State, error)
	Save(ctx context.Context, state *State) error
	Delete(ctx context.Context) error
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
	return &state, nil
}

func (s *fileStateStore) Save(_ context.Context, state *State) error {
	if state == nil {
		return fmt.Errorf("daemon state is nil")
	}
	data, err := json.MarshalIndent(state, "", "  ")
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
