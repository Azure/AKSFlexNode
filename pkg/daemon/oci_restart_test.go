package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIRestartUsesAppliedSlotAndConfiguration(t *testing.T) {
	t.Parallel()

	applied := &agentconfig.AgentConfig{
		Isolation: string(isolation.ModeContainer),
		Cluster: agentconfig.AgentClusterConfig{
			Version: "1.34.0",
		},
	}
	worker := &fakeWorkerRuntime{}
	h := &machineOperationHandlers{
		log:     slog.New(slog.DiscardHandler),
		runtime: worker,
		state: &testStateStore{
			state: &State{
				Isolation:     isolation.ModeContainer,
				ActiveMachine: "kube2",
				NodeConfig:    applied,
			},
		},
		cfg: &config.Config{
			Components: config.ComponentsConfig{
				Kubernetes: "1.35.0",
			},
		},
		resolve: func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
			t.Fatal("resolved fresh configuration for OCI restart")
			return nil, nil, nil, nil
		},
	}
	if err := h.restartNode(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !worker.restarted || worker.restartInput.Node.Config != applied || worker.restartInput.Node.Slot != "kube2" {
		t.Fatal("restart did not use the installed OCI slot and configuration")
	}
}
