package daemon

import (
	"bytes"
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
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

const (
	stateFileMode = 0o600
	stateFileName = "daemon-state.json"
)

// State records the last safely applied AKS machine goal and the previous
// known-good goal needed for rollback-oriented reconciliation.
type State struct {
	Isolation  isolation.Mode           `json:"isolation,omitempty"`
	NodeConfig *agentconfig.AgentConfig `json:"nodeConfig,omitempty"`

	AppliedSettingsVersion    string `json:"appliedSettingsVersion,omitempty"`
	AppliedKubernetesVersion  string `json:"appliedKubernetesVersion,omitempty"`
	PreviousSettingsVersion   string `json:"previousSettingsVersion,omitempty"`
	PreviousKubernetesVersion string `json:"previousKubernetesVersion,omitempty"`
	ActiveMachine             string `json:"activeMachine,omitempty"`
}

type activeMachine struct {
	Name  string
	State *State
}

type saveStateTask struct {
	store stateStore
	state *State
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
		AppliedSettingsVersion:   goal.SettingsVersion,
		AppliedKubernetesVersion: goal.KubernetesVersion,
		ActiveMachine:            goalstates.NSpawnMachineKube1,
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
	if state.Isolation == "" {
		state.Isolation = config.DefaultIsolationMode
	}
	if err := config.ValidateIsolation(state.Isolation); err != nil {
		return nil, fmt.Errorf("invalid applied worker isolation: %w", err)
	}
	if state.Isolation == isolation.ModeNone {
		if state.ActiveMachine != goalstates.NSpawnMachineKube1 {
			return nil, fmt.Errorf("applied none worker has invalid slot %q", state.ActiveMachine)
		}
		if state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
			return nil, fmt.Errorf("applied none worker has no matching runtime configuration")
		}
	}
	if state.Isolation == isolation.ModeContainer {
		if !validActiveMachine(state.ActiveMachine) || state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeContainer) {
			return nil, fmt.Errorf("applied OCI worker has invalid slot or runtime configuration")
		}
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
	if err := utilio.InstallFile(s.path, bytes.NewReader(data), stateFileMode); err != nil {
		return fmt.Errorf("write daemon state %s: %w", s.path, err)
	}
	if err := utilio.InstallFile(s.checksumPath(), strings.NewReader(checksum(data)+"\n"), stateFileMode); err != nil {
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

func nextAppliedState(current *State, goal aksmachine.GoalState, active *activeMachine) *State {
	next := &State{
		AppliedSettingsVersion:    goal.SettingsVersion,
		AppliedKubernetesVersion:  goal.KubernetesVersion,
		PreviousSettingsVersion:   "",
		PreviousKubernetesVersion: "",
	}
	if current != nil {
		next.PreviousSettingsVersion = current.AppliedSettingsVersion
		next.PreviousKubernetesVersion = current.AppliedKubernetesVersion
	}
	if active != nil {
		next.ActiveMachine = active.Name
	}
	return next
}
