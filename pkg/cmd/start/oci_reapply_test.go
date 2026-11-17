package start

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestOCIReapplyUsesPersistedAlternateSlotAndConfig(t *testing.T) {
	t.Parallel()

	cfg := bootstrapConfig(isolation.ModeContainer)
	appliedConfig := config.ToAgentConfig(cfg, "kube2")
	appliedConfig.Cluster.Version = "1.35.0"
	applied := &daemon.State{
		Isolation:                isolation.ModeContainer,
		ActiveMachine:            "kube2",
		AppliedKubernetesVersion: "1.35.0",
		AppliedSettingsVersion:   "etag-applied",
		NodeConfig:               appliedConfig,
	}
	state, goal, reapply, err := selectBootstrap(cfg, applied)
	if err != nil || !reapply || state != applied {
		t.Fatalf("select installed worker: reapply=%t, err=%v", reapply, err)
	}

	machines := &bootstrapTestMachine{}
	log := slog.New(slog.DiscardHandler)
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
	if err := phases.Serial(log, workerBootstrapTasks(log, worker, input, reapply, daemon.SaveState(store, state))...).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if machines.gets != 0 || machines.creates != 0 || store.saves != 0 {
		t.Fatalf("reapply touched machine/state: gets=%d creates=%d saves=%d", machines.gets, machines.creates, store.saves)
	}
	if !reflect.DeepEqual(worker.events, []string{"recover", "restart"}) {
		t.Fatalf("reapply events = %v", worker.events)
	}
	if worker.input.Node.Slot != "kube2" || worker.input.Node.Config != appliedConfig || state.AppliedSettingsVersion != "etag-applied" {
		t.Fatal("reapply did not retain the applied OCI slot and configuration")
	}
}
