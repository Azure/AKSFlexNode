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
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	stateFileMode = 0o600
	stateFileName = "daemon-state.json"

	noneStateRecordVersion = 1
)

// noneStateRecord keeps the applied config and its integrity check in one
// durable publication, so upgrade recovery never reads a half-committed pair.
type noneStateRecord struct {
	Version  int             `json:"version"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

// State records the last safely applied AKS machine goal and the previous
// known-good goal needed for rollback-oriented reconciliation.
type State struct {
	AppliedSettingsVersion    string                   `json:"appliedSettingsVersion,omitempty"`
	AppliedKubernetesVersion  string                   `json:"appliedKubernetesVersion,omitempty"`
	PreviousSettingsVersion   string                   `json:"previousSettingsVersion,omitempty"`
	PreviousKubernetesVersion string                   `json:"previousKubernetesVersion,omitempty"`
	ActiveMachine             string                   `json:"activeMachine,omitempty"`
	Isolation                 isolation.Mode           `json:"isolation,omitempty"`
	NodeConfig                *agentconfig.AgentConfig `json:"nodeConfig,omitempty"`
	// NPDVersion identifies none's application binaries. Empty denotes the
	// legacy directory identity or a node with NPD disabled.
	NPDVersion string `json:"npdVersion,omitempty"`
}

func (s *State) isolationMode() isolation.Mode {
	if s.Isolation == "" {
		return isolation.ModeNSpawn
	}
	return s.Isolation
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

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err == nil {
		for _, key := range []string{"version", "payload", "checksum"} {
			if _, ok := fields[key]; ok {
				return s.loadNoneRecord(data)
			}
		}
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

func (s *fileStateStore) loadNoneRecord(data []byte) (*State, error) {
	var record noneStateRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode daemon state record %s: %w", s.path, err)
	}
	if record.Version != noneStateRecordVersion {
		return nil, fmt.Errorf("unsupported daemon state record version %d in %s", record.Version, s.path)
	}
	if len(record.Payload) == 0 {
		return nil, fmt.Errorf("missing daemon state record payload in %s", s.path)
	}
	if record.Checksum != checksum(record.Payload) {
		return nil, fmt.Errorf("daemon state checksum mismatch for %s", s.path)
	}
	var state State
	if err := json.Unmarshal(record.Payload, &state); err != nil {
		return nil, fmt.Errorf("decode daemon state payload %s: %w", s.path, err)
	}
	if state.isolationMode() != isolation.ModeNone && state.isolationMode() != isolation.ModeContainer {
		return nil, fmt.Errorf("daemon state record %s requires none or container isolation", s.path)
	}
	return &state, nil
}

func (s *fileStateStore) saveNoneRecord(state *State) error {
	payload, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal daemon state: %w", err)
	}
	data, err := json.Marshal(noneStateRecord{
		Version: noneStateRecordVersion, Payload: payload, Checksum: checksum(payload),
	})
	if err != nil {
		return fmt.Errorf("marshal daemon state record: %w", err)
	}
	// The durable writer preserves existing permissions. Tighten them before
	// publishing credentials into a file an operator may have made readable.
	if err := os.Chmod(s.path, stateFileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("set daemon state permissions %s: %w", s.path, err)
	}
	if err := utilio.WriteFileDurable(s.path, append(data, '\n'), stateFileMode); err != nil {
		return fmt.Errorf("write daemon state record %s: %w", s.path, err)
	}
	return nil
}

func (s *fileStateStore) Save(_ context.Context, state *State) error {
	if state == nil {
		return fmt.Errorf("daemon state is nil")
	}
	if state.isolationMode() == isolation.ModeNone || state.isolationMode() == isolation.ModeContainer {
		return s.saveNoneRecord(state)
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
