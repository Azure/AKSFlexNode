package start

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestOCIReapplyUsesPersistedSlotAndConfig(t *testing.T) {
	t.Parallel()

	for _, slot := range []string{"kube1", "kube2"} {
		t.Run(slot, func(t *testing.T) {
			t.Parallel()

			cfg := bootstrapConfig(isolation.ModeContainer)
			appliedConfig := config.ToAgentConfig(cfg, slot)
			appliedConfig.Cluster.Version = "1.35.0"
			applied := &daemon.State{
				Isolation:                isolation.ModeContainer,
				ActiveMachine:            slot,
				AppliedKubernetesVersion: "1.35.0",
				AppliedSettingsVersion:   "etag-applied",
				NodeConfig:               appliedConfig,
			}
			log := slog.New(slog.DiscardHandler)
			if err := daemon.ValidateWorkerBootstrap(cfg, applied, false); err != nil {
				t.Fatalf("validate installed worker: %v", err)
			}
			state, goal, reapply, err := selectBootstrap(cfg, applied)
			if err != nil || !reapply || state != applied {
				t.Fatalf("select installed worker: reapply=%t, err=%v", reapply, err)
			}

			machines := &bootstrapTestMachine{}
			if err := registerBootstrap(t.Context(), log, cfg, machines, &goal, state, reapply); err != nil {
				t.Fatal(err)
			}

			worker := &bootstrapTestRuntime{}
			store := &bootstrapTestStore{state: applied}
			input := agentruntime.Inputs{
				Config: cfg,
				Node: agentruntime.Node{
					Slot:   state.ActiveMachine,
					Config: state.NodeConfig,
				},
			}
			saveTask := hostnode.Func("save-daemon-state", func(ctx context.Context) error {
				return store.Save(ctx, state)
			})
			if err := phases.Serial(log, workerBootstrapTasks(log, worker, input, reapply, saveTask)...).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			if machines.gets != 0 || machines.creates != 0 || store.saves != 0 {
				t.Fatalf("reapply touched machine/state: gets=%d creates=%d saves=%d", machines.gets, machines.creates, store.saves)
			}
			if !reflect.DeepEqual(worker.events, []string{"recover", "restart"}) {
				t.Fatalf("reapply events = %v", worker.events)
			}
			if worker.input.Node.Slot != slot || worker.input.Node.Config != appliedConfig || state.AppliedSettingsVersion != "etag-applied" {
				t.Fatal("reapply did not retain the applied OCI slot and configuration")
			}
		})
	}
}

func TestOCIReapplyRejectsInvalidSelectionBeforeWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*config.Config, *daemon.State)
		want   string
	}{
		{
			name: "missing applied config",
			change: func(_ *config.Config, applied *daemon.State) {
				applied.NodeConfig = nil
			},
			want: "applied worker configuration is incomplete",
		},
		{
			name: "invalid slot",
			change: func(_ *config.Config, applied *daemon.State) {
				applied.ActiveMachine = "kube3"
			},
			want: "applied worker configuration is incomplete",
		},
		{
			name: "different node",
			change: func(cfg *config.Config, _ *daemon.State) {
				cfg.Agent.NodeName = "another-node"
			},
			want: "reapply node name differs",
		},
		{
			name: "different endpoint",
			change: func(cfg *config.Config, _ *daemon.State) {
				cfg.Node.Kubelet.ClusterFQDN = "https://other.example"
			},
			want: "reapply cluster endpoint or CA differs",
		},
		{
			name: "different authentication",
			change: func(cfg *config.Config, _ *daemon.State) {
				cfg.Azure.BootstrapToken = nil
				cfg.Azure.ManagedIdentity = &config.ManagedIdentityConfig{ClientID: "other"}
			},
			want: "reapply worker authentication differs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := bootstrapConfig(isolation.ModeContainer)
			applied := &daemon.State{
				Isolation:     isolation.ModeContainer,
				ActiveMachine: "kube2",
				NodeConfig:    config.ToAgentConfig(cfg, "kube2"),
			}
			tc.change(cfg, applied)

			if err := daemon.ValidateWorkerBootstrap(cfg, applied, false); err != nil {
				t.Fatalf("bootstrap guard: %v", err)
			}
			if _, _, _, err := selectBootstrap(cfg, applied); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("selection error = %v, want %q", err, tc.want)
			}
		})
	}
}
