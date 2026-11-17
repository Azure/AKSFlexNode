package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestDirectActivationRejectsUnsupportedCandidate(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			testDirectActivationUnsupported(t, mode)
		})
	}
}

func testDirectActivationUnsupported(t *testing.T, mode isolation.Mode) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	service := &flexDaemonActivationService{
		HostActivationService: &agentdeployment.HostActivationService{},
		log:                   slog.New(slog.DiscardHandler),
		candidatePath:         path,
		state: &testStateStore{
			state: &State{
				Isolation:     mode,
				ActiveMachine: "kube1",
				NodeConfig: &agentconfig.AgentConfig{
					Isolation: string(mode),
				},
			},
		},
	}
	if _, err := service.Preflight(t.Context(), "/unused/current"); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("candidate cannot operate %s workers", mode)) {
		t.Fatalf("unsupported candidate error = %v", err)
	}
}
