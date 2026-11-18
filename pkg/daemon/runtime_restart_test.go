package daemon

import (
	"context"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRestartUsesAppliedMachineVersion(t *testing.T) {
	t.Parallel()

	worker := &fakeWorkerRuntime{
		state: &State{
			Isolation:                config.DefaultIsolationMode,
			ActiveMachine:            goalstates.NSpawnMachineKube2,
			AppliedKubernetesVersion: "1.34.0",
		},
	}
	cfg := &config.Config{
		Bootstrap:  config.BootstrapConfig{Isolation: config.DefaultIsolationMode},
		Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
	}
	var resolvedMachine, resolvedVersion string
	h := &machineOperationHandlers{
		log:     slog.New(slog.DiscardHandler),
		runtime: worker,
		state:   worker,
		cfg:     cfg,
		resolve: func(_ context.Context, _ *slog.Logger, resolved *config.Config, machine string) (config.ResolvedMachineGoal, error) {
			resolvedMachine = machine
			resolvedVersion = resolved.Components.Kubernetes
			return config.ResolvedMachineGoal{GoalState: &goalstates.MachineGoalState{}}, nil
		},
	}

	if err := h.restartNode(t.Context()); err != nil {
		t.Fatalf("restartNode: %v", err)
	}
	if resolvedMachine != goalstates.NSpawnMachineKube2 || resolvedVersion != "1.34.0" || !worker.restarted {
		t.Fatalf("resolved machine %q version %q restarted %v", resolvedMachine, resolvedVersion, worker.restarted)
	}
	if cfg.Components.Kubernetes != "1.35.0" {
		t.Fatal("restart mutated the daemon's startup config")
	}
}
