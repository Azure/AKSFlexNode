package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestDirectNoneActivationRejectsUnsupportedCandidate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := &flexDaemonActivationService{
		hostActivationService: &agentdeployment.HostActivationService{},
		log:                   slog.New(slog.DiscardHandler),
		candidatePath:         path,
		state: &testStateStore{
			state: &State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube1",
				NodeConfig:    &agentconfig.AgentConfig{Isolation: "none"},
			},
		},
	}
	if _, err := service.Preflight(t.Context(), "/unused/current"); err == nil || !strings.Contains(err.Error(), "candidate cannot operate none workers") {
		t.Fatalf("unsupported candidate error = %v", err)
	}
}
