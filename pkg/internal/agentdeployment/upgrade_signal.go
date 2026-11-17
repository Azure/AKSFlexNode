package agentdeployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// ValidNspawnMachine reports whether machine is one of the two managed slots.
func ValidNspawnMachine(machine string) bool {
	return machine == goalstates.NSpawnMachineKube1 || machine == goalstates.NSpawnMachineKube2
}

// UpgradeSignal records the durable handoff between the initiating and restarted agents.
type UpgradeSignal struct {
	OperationName            string `json:"operationName"`
	ActiveMachine            string `json:"activeMachine,omitempty"`
	CandidatePath            string `json:"candidatePath,omitempty"`
	InitiatingDaemonInstance string `json:"initiatingDaemonInstance,omitempty"`
	SwitchCommitted          bool   `json:"switchCommitted,omitempty"`
	RecoveryRequired         bool   `json:"recoveryRequired,omitempty"`
	Failure                  string `json:"failure,omitempty"`
	// In the agent's container, the image an upgrade switches to and the one it switches from.
	CandidateImage string `json:"candidateImage,omitempty"`
	PreviousImage  string `json:"previousImage,omitempty"`
}

// UpgradeSignalStore persists an agent upgrade's recovery signal.
type UpgradeSignalStore struct {
	path string
}

// NewUpgradeSignalStore uses path for the existing JSON signal format.
func NewUpgradeSignalStore(path string) UpgradeSignalStore {
	return UpgradeSignalStore{path: path}
}

func (s UpgradeSignalStore) RecordPending(operationName, activeMachine, daemonInstance string) error {
	return s.Write(UpgradeSignal{
		OperationName:            operationName,
		ActiveMachine:            activeMachine,
		InitiatingDaemonInstance: daemonInstance,
	})
}

func (s UpgradeSignalStore) RecordCandidate(candidatePath string) error {
	signal, err := s.Read()
	if err != nil {
		return err
	}
	if signal == nil {
		return fmt.Errorf("no pending AgentUpgrade signal")
	}
	signal.CandidatePath = candidatePath
	return s.Write(*signal)
}

func (s UpgradeSignalStore) RecordCandidateImage(candidate, previous string) error {
	signal, err := s.Read()
	if err != nil {
		return err
	}
	if signal == nil {
		return fmt.Errorf("no pending AgentUpgrade signal")
	}
	signal.CandidateImage = candidate
	signal.PreviousImage = previous
	return s.Write(*signal)
}

func (s UpgradeSignalStore) RecordSwitchCommitted() error {
	signal, err := s.Read()
	if err != nil {
		return err
	}
	if signal == nil {
		return fmt.Errorf("no pending AgentUpgrade signal")
	}
	signal.SwitchCommitted = true
	return s.Write(*signal)
}

func (s UpgradeSignalStore) RecordFailure(message string) error {
	signal, err := s.Read()
	if err != nil {
		return err
	}
	if signal == nil {
		return nil
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upgraded daemon failed to start; restored last-good binary"
	}
	signal.Failure = message
	signal.RecoveryRequired = true
	return s.Write(*signal)
}

func (s UpgradeSignalStore) Read() (*UpgradeSignal, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read AgentUpgrade signal: %w", err)
	}
	var signal UpgradeSignal
	if err := json.Unmarshal(data, &signal); err != nil {
		return nil, fmt.Errorf("decode AgentUpgrade signal: %w", err)
	}
	signal.OperationName = strings.TrimSpace(signal.OperationName)
	signal.ActiveMachine = strings.TrimSpace(signal.ActiveMachine)
	signal.CandidatePath = strings.TrimSpace(signal.CandidatePath)
	signal.Failure = strings.TrimSpace(signal.Failure)
	if signal.OperationName == "" {
		return nil, fmt.Errorf("AgentUpgrade signal has no operation name")
	}
	if signal.ActiveMachine != "" && !ValidNspawnMachine(signal.ActiveMachine) {
		return nil, fmt.Errorf("AgentUpgrade signal has invalid active machine %q", signal.ActiveMachine)
	}
	return &signal, nil
}

func (s UpgradeSignalStore) Write(signal UpgradeSignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return fmt.Errorf("encode AgentUpgrade signal: %w", err)
	}
	if err := utilio.WriteFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write AgentUpgrade signal: %w", err)
	}
	return nil
}

func (s UpgradeSignalStore) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove AgentUpgrade signal: %w", err)
	}
	return nil
}
