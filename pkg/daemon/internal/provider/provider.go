// Package provider owns worker runtime operations, independently of AKS state.
package provider

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Node identifies one applied or desired worker configuration.
type Node struct {
	Slot      string
	Config    *agentconfig.AgentConfig
	GoalState *goalstates.MachineGoalState
}

// StartParams contains resolved worker inputs and Flex-owned artifact settings.
type StartParams struct {
	Node     Node
	Config   *config.Config
	Archives *goalstates.ContainerImageArchiveStaging
}

// UpgradeParams describes both sides of a repave. The daemon persists Node
// only after Upgrade succeeds and before Retire runs.
type UpgradeParams struct {
	StartParams
	Previous Node
}

// Layout describes worker placement and the services and paths consumers use.
type Layout struct {
	Mode    isolation.Mode
	BaseDir string
	// FixedSlot is set only for a runtime without blue/green slots.
	FixedSlot   string
	KubeletUnit string
	// Empty HelperPath means credentials invoke the host agent directly.
	HelperPath string
	Targets    []string
}

// Directory returns the agent-visible filesystem containing worker-local state.
func (l Layout) Directory(slot string) string {
	if l.FixedSlot != "" {
		return l.BaseDir
	}
	return filepath.Join(l.BaseDir, slot)
}

// NextSlot selects runtime placement for a repave.
func (l Layout) NextSlot(current string) string {
	if l.FixedSlot != "" {
		return l.FixedSlot
	}
	return goalstates.AlternateMachine(current)
}

// CredentialHelperPath returns the worker's copy, or empty for direct host execution.
func (l Layout) CredentialHelperPath(slot string) string {
	if l.HelperPath == "" {
		return ""
	}
	return filepath.Join(l.Directory(slot), l.HelperPath)
}

// Provider performs worker operations without owning AKS state or status.
type Provider interface {
	// Start provisions and configures a worker. It returns whether to persist the
	// applied config; an existing none worker retains its installed version.
	Start(ctx context.Context, log *slog.Logger, params StartParams) (persist bool, err error)
	// Restart restarts the worker without resetting its credentials.
	Restart(ctx context.Context, log *slog.Logger, params StartParams) error
	// Upgrade applies Node using the SDK's existing rollback behavior.
	Upgrade(ctx context.Context, log *slog.Logger, params UpgradeParams) error
	// Recover finishes SDK recovery against the persisted applied configuration.
	Recover(ctx context.Context, log *slog.Logger, params StartParams) error
	// Retire commits the SDK upgrade and removes the superseded runtime after save.
	Retire(ctx context.Context, log *slog.Logger, params UpgradeParams) error
	// RewriteUnits updates worker units after the host root moves, without restart.
	RewriteUnits(ctx context.Context, log *slog.Logger, node Node) error
	// Reset returns the runtime teardown; the daemon supplies host-global cleanup.
	Reset(log *slog.Logger) phases.Task
	// GoalState supplies the SDK filesystem and execution view of a worker.
	GoalState(log *slog.Logger, node Node) (*goalstates.MachineGoalState, error)
	// Layout supplies runtime placement, credential-helper paths and service dependencies.
	Layout() Layout
}

// New requires an already-normalized and validated mode.
func New(mode isolation.Mode) (Provider, error) {
	switch mode {
	case isolation.ModeNSpawn:
		return nspawnProvider{}, nil
	case isolation.ModeNone:
		return newNoneProvider(), nil
	case isolation.ModeContainer:
		return containerProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported worker isolation %q", mode)
	}
}
