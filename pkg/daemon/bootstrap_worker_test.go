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

func TestContainerizedAgentModeValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		mode          isolation.Mode
		containerized bool
		wantErr       bool
	}{
		{
			name:          "containerized nspawn",
			mode:          isolation.ModeNSpawn,
			containerized: true,
		},
		{
			name:          "containerized none",
			mode:          isolation.ModeNone,
			containerized: true,
			wantErr:       true,
		},
		{
			name: "host none",
			mode: isolation.ModeNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := ValidateAgentDeployment(tc.mode, tc.containerized); (err != nil) != tc.wantErr {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestBootstrapStateSelectsInstalledWorker(t *testing.T) {
	t.Parallel()

	seed := &State{ActiveMachine: "kube1"}
	cfg := &config.Config{
		Agent: config.AgentConfig{NodeName: "node1"},
		Azure: config.AzureConfig{
			BootstrapToken: &config.BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
	}
	installed := &State{ActiveMachine: "kube1", NodeConfig: config.ToAgentConfig(cfg, "kube1")}
	alternate := &State{ActiveMachine: "kube2", NodeConfig: config.ToAgentConfig(cfg, "kube2")}
	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		applied *State
		want    *State
		reapply bool
		wantErr bool
	}{
		{
			name: "first install",
			mode: isolation.ModeNone,
			want: seed,
		},
		{
			name:    "none reapply",
			mode:    isolation.ModeNone,
			applied: installed,
			want:    installed,
			reapply: true,
		},
		{
			name:    "future alternate slot",
			mode:    isolation.ModeContainer,
			applied: alternate,
			want:    alternate,
			reapply: true,
		},
		{
			name:    "nspawn keeps bootstrap behavior",
			mode:    isolation.ModeNSpawn,
			applied: installed,
			want:    seed,
		},
		{
			name:    "missing applied config",
			mode:    isolation.ModeNone,
			applied: &State{ActiveMachine: "kube1"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			candidate := cfg.DeepCopy()
			candidate.Bootstrap.Isolation = tc.mode
			got, reapply, err := BootstrapState(tc.applied, seed, candidate)
			if (err != nil) != tc.wantErr || got != tc.want || reapply != tc.reapply {
				t.Fatalf("selection = %p, %t, %v; want %p, %t", got, reapply, err, tc.want, tc.reapply)
			}
		})
	}
}

func TestNoneRestartUsesAppliedConfigurationWithoutResolvingDrift(t *testing.T) {
	t.Parallel()

	applied := &agentconfig.AgentConfig{
		Isolation: string(isolation.ModeNone),
		Cluster:   agentconfig.AgentClusterConfig{Version: "1.34.0"},
	}
	worker := &fakeWorkerRuntime{}
	h := &machineOperationHandlers{
		log:     slog.New(slog.DiscardHandler),
		runtime: worker,
		state: &testStateStore{
			state: &State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube1",
				NodeConfig:    applied,
			},
		},
		cfg: &config.Config{
			Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
		},
		resolve: func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
			t.Fatal("resolved caller configuration for none restart")
			return nil, nil, nil, nil
		},
	}
	if err := h.restartNode(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !worker.restarted || worker.restartInput.Node.Config != applied {
		t.Fatal("restart did not use the installed SDK configuration")
	}
}
