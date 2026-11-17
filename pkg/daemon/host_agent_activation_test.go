package daemon

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
)

func TestActivationRejectsNilLogger(t *testing.T) {
	t.Parallel()

	t.Run("preflight before candidate access", func(t *testing.T) {
		t.Parallel()

		candidate := filepath.Join(t.TempDir(), "missing")
		if _, err := PreflightHostAgentActivation(t.Context(), nil, candidate); err == nil || err.Error() != "logger is nil" {
			t.Fatalf("PreflightHostAgentActivation() error = %v, want logger is nil", err)
		}
	})

	t.Run("direct activation", func(t *testing.T) {
		t.Parallel()

		if _, err := ActivateHostAgent(t.Context(), nil, "/unused/candidate"); err == nil || err.Error() != "logger is nil" {
			t.Fatalf("ActivateHostAgent() error = %v, want logger is nil", err)
		}
	})

	t.Run("coordinator constructor", func(t *testing.T) {
		t.Parallel()

		service, paths, err := newFlexDaemonActivationService(nil, func() string {
			t.Fatal("resolved host root before rejecting nil logger")
			return ""
		}, "/unused/candidate")
		if err == nil || err.Error() != "logger is nil" {
			t.Fatalf("newFlexDaemonActivationService() error = %v, want logger is nil", err)
		}
		if service != nil || paths != (agentdeployment.UpgradePaths{}) {
			t.Fatalf("returned partial activation service: service = %v, paths = %+v", service, paths)
		}
	})
}

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
