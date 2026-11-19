package daemon

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
)

// TestDirectActivationIsRefusedInAContainer: in the agent's container the host has no binary
// layout to switch, and an AgentUpgrade switches images instead. It sets the environment, so it
// doesn't run in parallel.
func TestDirectActivationIsRefusedInAContainer(t *testing.T) {
	t.Setenv(agentdeployment.ContainerizedEnv, "true")
	log := slog.New(slog.DiscardHandler)

	if _, err := PreflightHostAgentActivation(t.Context(), log, "/nonexistent/aks-flex-node"); !errors.Is(err, agentdeployment.ErrDirectActivationInContainer) {
		t.Fatalf("PreflightHostAgentActivation() error = %v, want %v", err, agentdeployment.ErrDirectActivationInContainer)
	}
	if _, err := ActivateHostAgent(t.Context(), log, "/nonexistent/aks-flex-node"); !errors.Is(err, agentdeployment.ErrDirectActivationInContainer) {
		t.Fatalf("ActivateHostAgent() error = %v, want %v", err, agentdeployment.ErrDirectActivationInContainer)
	}
}
