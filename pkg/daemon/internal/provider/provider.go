package provider

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/preflight"
	corev1 "k8s.io/api/core/v1"
)

// State records the applied AKS goal and runtime configuration used for recovery.
type State struct {
	AppliedSettingsVersion    string                   `json:"appliedSettingsVersion,omitempty"`
	AppliedKubernetesVersion  string                   `json:"appliedKubernetesVersion,omitempty"`
	PreviousSettingsVersion   string                   `json:"previousSettingsVersion,omitempty"`
	PreviousKubernetesVersion string                   `json:"previousKubernetesVersion,omitempty"`
	ActiveMachine             string                   `json:"activeMachine,omitempty"`
	Isolation                 isolation.Mode           `json:"isolation,omitempty"`
	NodeConfig                *agentconfig.AgentConfig `json:"nodeConfig,omitempty"`
}

// DeepCopy copies the state and its runtime configuration.
func (s *State) DeepCopy() *State {
	next := *s
	next.NodeConfig = s.NodeConfig.DeepCopy()
	return &next
}

// IsolationMode interprets records written before isolation was configurable.
func (s *State) IsolationMode() isolation.Mode {
	if s.Isolation == "" {
		return isolation.ModeNSpawn
	}
	return s.Isolation
}

// StateStore persists the applied goal independently of runtime-owned files.
type StateStore interface {
	// Load returns the last applied state, or nil when no state exists.
	Load(ctx context.Context) (*State, error)
	// Save durably publishes an applied runtime and its configuration.
	Save(ctx context.Context, state *State) error
	// Delete removes the applied state after runtime cleanup succeeds.
	Delete(ctx context.Context) error
}

// StateLoader reads applied state from a runtime operator.
type StateLoader interface {
	// LoadState returns the operator's applied runtime state.
	LoadState(ctx context.Context) (*State, error)
}

// Operator applies AKS machine actions to an installed runtime.
type Operator interface {
	StateLoader
	// ApplyGoalState repaves the worker and persists the resulting applied goal.
	ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error)
	// RestartNode restarts the applied worker without accepting a new goal.
	RestartNode(ctx context.Context, log *slog.Logger) error
	// ResumeRepave recovers durable work; true means this reconciliation handled recovery.
	ResumeRepave(ctx context.Context, log *slog.Logger, node *corev1.Node) (bool, error)
	// ResetNode cleans runtime and state without stopping the reporting daemon.
	ResetNode(ctx context.Context, log *slog.Logger) error
	// StopDaemon removes the daemon after lifecycle completion has been reported.
	StopDaemon(ctx context.Context, log *slog.Logger) error
}

// StartParams supplies the resolved goal and applied-state destination for bootstrap.
type StartParams struct {
	Config                 *config.Config
	Log                    *slog.Logger
	MachineName            string
	GoalState              *goalstates.MachineGoalState
	ContainerImageArchives *goalstates.ContainerImageArchiveStaging
	Store                  StateStore
	State                  *State
}

// NodeProvider owns node-runtime actions.
type NodeProvider interface {
	BootstrapActions
	AgentActions
	HostActions
	// Mode identifies the runtime implemented by this provider.
	Mode() isolation.Mode
	// NewOperator binds runtime actions to the daemon configuration and state store.
	NewOperator(ctx context.Context, cfg *config.Config, store StateStore) (Operator, error)
}

// BootstrapActions prepares and starts a worker from a resolved AKS configuration.
type BootstrapActions interface {
	// ValidateEnvironment rejects unsupported host-installed or containerized-agent execution.
	ValidateEnvironment(containerized bool) error
	// BootstrapPresent detects runtime-owned files left by bootstrap or reset.
	BootstrapPresent(log *slog.Logger) bool
	// CheckBootstrap rejects incompatible installations and checks runtime prerequisites.
	CheckBootstrap(ctx context.Context, log *slog.Logger, applied *State) error
	// ResolveGoalState adds runtime filesystem and execution details to the AKS goal.
	ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
	// SetupHost prepares host packages, networking, and services for this runtime.
	SetupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task
	// StartNode starts the resolved worker and saves its applied state.
	StartNode(params StartParams) phases.Task
	// RuntimeChecks returns additional checks for the selected runtime.
	RuntimeChecks(cfg *config.Config, log *slog.Logger, containerized bool) []preflight.Checker
}

// AgentActions coordinates host-agent binaries with the worker runtime.
type AgentActions interface {
	// ValidateAgentActivation rejects activation while runtime completion is pending.
	ValidateAgentActivation(ctx context.Context) error
	// ValidateActiveSlot verifies that a persisted slot can identify this runtime.
	ValidateActiveSlot(slot string) error
	// CheckAgentCandidate checks that a staged binary supports this runtime.
	CheckAgentCandidate(ctx context.Context, candidate string) error
	// SynchronizeAgent installs the host binary into the active worker when needed.
	SynchronizeAgent(source string, slot string) error
	// ValidateAgent checks that the worker helper matches the activated host binary.
	ValidateAgent(source string, slot string) error
	// RollbackAgent restores the worker helper after a host-agent rollback.
	RollbackAgent(source string, slot string) error
}

// HostActions manages runtime-owned host services and files.
type HostActions interface {
	// WantedTargets returns dependencies pulled in by the host agent service.
	WantedTargets() []string
	// SupportsDisconnectTolerance reports whether the runtime can host YurtHub.
	SupportsDisconnectTolerance() bool
	// ResetNode builds the runtime cleanup task, preserving managed agent binaries.
	ResetNode(log *slog.Logger) phases.Task
	// ValidateResetState verifies persisted runtime ownership before cleanup.
	ValidateResetState(state *State) error
	// RewriteNodeUnits updates runtime units after the host-agent root moves.
	RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, store StateStore) error
	// NSpawnRuntime returns nspawn-only agent hooks, or nil for another runtime.
	NSpawnRuntime() NSpawnRuntime
}

// NSpawnRuntime converges the existing containerized agent's nspawn installation.
type NSpawnRuntime interface {
	// ConvergeContainerAgent returns the unit rewrite needed before releasing old assets.
	ConvergeContainerAgent(ctx context.Context, log *slog.Logger, state StateLoader) (func(ctx context.Context) error, error)
}
