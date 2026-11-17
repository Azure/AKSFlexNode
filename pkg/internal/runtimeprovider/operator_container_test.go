package runtimeprovider

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/bootstrapdata"
	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func containerTestState(slot, version string) *State {
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true}}
	return (&State{Isolation: isolation.ModeContainer, ActiveMachine: slot,
		AppliedSettingsVersion: version, NodeConfig: config.ToAgentConfig(cfg, slot)}).DeepCopy()
}

type fakeContainerNode struct {
	params  containerops.Params
	events  *[]string
	fail    string
	backend container.Backend
}

func (n *fakeContainerNode) call(event string) error {
	*n.events = append(*n.events, event)
	if n.fail == event {
		return errors.New("failed " + event)
	}
	return nil
}
func (n *fakeContainerNode) Backend() *container.Backend { return &n.backend }
func (n *fakeContainerNode) GoalState() (*goalstates.MachineGoalState, error) {
	if n.params.GoalState != nil {
		return n.params.GoalState, nil
	}
	return &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{}}, nil
}
func (n *fakeContainerNode) Start(_ context.Context, running, cri, kubelet bool) error {
	if running || cri || kubelet {
		return errors.New("resume rewrites or restarts configuration")
	}
	return n.call("resume-start")
}
func (n *fakeContainerNode) Restart(context.Context) error { return n.call("restart") }
func (n *fakeContainerNode) Destroy(context.Context) error { return n.call("destroy-" + n.params.Slot) }
func (n *fakeContainerNode) Upgrade(context.Context, containerNode) error {
	if err := n.call("upgrade"); err != nil {
		return err
	}
	*n.events = append(*n.events, "source-stop", "start")
	return nil
}

func containerOperatorFixture(t *testing.T, fail string, failSave bool) (*containerNodeOperator, *[]string, *noneTestStore) {
	t.Helper()
	events := new([]string)
	state := containerTestState("kube1", "old")
	store := &noneTestStore{events: events, state: state, failSave: failSave}
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true}}
	startup := ContainerStartupStore{Path: filepath.Join(t.TempDir(), "startup.json")}
	provider := containerTestProvider(t, startup)
	operator, err := provider.NewOperator(t.Context(), cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := operator.(*containerNodeOperator)
	if !ok {
		t.Fatalf("operator = %T", operator)
	}
	op.bootstrapDataRefresher = &fakeBootstrapDataRefresher{}
	op.newNode = func(p containerops.Params) (containerNode, error) {
		return &fakeContainerNode{params: p, events: events, fail: fail}, nil
	}
	op.syncPrepared = func(context.Context, ContainerRuntime) error {
		*events = append(*events, "sync-prepared")
		if fail == "sync-prepared" {
			return errors.New("prepared filesystem could not be persisted")
		}
		return nil
	}
	op.resolve = func(_ context.Context, _ *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
		return config.ToAgentConfig(cfg, slot), &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{}}, nil, nil
	}
	op.stage = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
		*events = append(*events, "stage")
		return nil
	}
	op.startNPD = func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error {
		*events = append(*events, "npd")
		if fail == "npd" {
			return errors.New("NPD not ready")
		}
		return nil
	}
	return op, events, store
}

func TestContainerRepaveCompletionAndCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fail, phase string
		failSave, wantErr bool
		want              []string
	}{
		{name: "healthy", phase: "finished", want: []string{"stage", "upgrade", "source-stop", "start", "sync-prepared", "npd", "sync-prepared", "persist", "destroy-kube1"}},
		{name: "SDK error fails closed", fail: "upgrade", phase: "preparing", wantErr: true, want: []string{"stage", "upgrade"}},
		{name: "NPD retryable", fail: "npd", phase: "starting", wantErr: true, want: []string{"stage", "upgrade", "source-stop", "start", "sync-prepared", "npd"}},
		{name: "post-SDK sync failure fails closed", fail: "sync-prepared", phase: "preparing", wantErr: true, want: []string{"stage", "upgrade", "source-stop", "start", "sync-prepared"}},
		{name: "state failure keeps source", failSave: true, phase: "starting", wantErr: true, want: []string{"stage", "upgrade", "source-stop", "start", "sync-prepared", "npd", "sync-prepared", "persist"}},
		{name: "source cleanup failure fenced", fail: "destroy-kube1", phase: "starting", wantErr: true, want: []string{"stage", "upgrade", "source-stop", "start", "sync-prepared", "npd", "sync-prepared", "persist", "destroy-kube1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, events, _ := containerOperatorFixture(t, tc.fail, tc.failSave)
			got, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.36.2"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ApplyGoalState = %v", err)
			}
			if !slices.Equal(*events, tc.want) {
				t.Fatalf("events = %v, want %v", *events, tc.want)
			}
			startup, err := op.startup.Load()
			if err != nil || startup == nil || startup.Phase != tc.phase {
				t.Fatalf("startup = %+v, %v", startup, err)
			}
			if !tc.wantErr && (got.ActiveMachine != "kube2" || got.AppliedSettingsVersion != "new" || got.PreviousSettingsVersion != "old" || got.NodeConfig.Cluster.Version != "1.36.2") {
				t.Fatalf("incorrect applied state: %+v", got)
			}
		})
	}
}

func TestContainerStartupResumeFrozen(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "", false)
	frozen := containerTestState("kube2", "frozen")
	frozen.NodeConfig.Cluster.Version = "1.36.2"
	if err := op.startup.Save(t.Context(), &ContainerStartup{Phase: "starting", Next: frozen, Previous: store.state}); err != nil {
		t.Fatal(err)
	}
	op.cfg.Components.Kubernetes = "1.99.0"
	op.resolve = func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
		t.Fatal("resume resolved a newer goal")
		return nil, nil, nil, nil
	}
	for attempt := range 2 {
		handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil)
		if err != nil || handled != (attempt == 0) {
			t.Fatalf("ResumeRepave = %t, %v", handled, err)
		}
	}
	if !slices.Equal(*events, []string{"resume-start", "npd", "sync-prepared", "persist", "destroy-kube1"}) {
		t.Fatalf("resume changed shared configuration: %v", *events)
	}
	if !reflect.DeepEqual(store.state, frozen) {
		t.Fatal("did not persist frozen state")
	}
	if startup, err := op.startup.Load(); err != nil || startup != nil {
		t.Fatalf("completed startup remains: %+v, %v", startup, err)
	}
}

func TestContainerInterruptedSDKUpgradeFailsClosed(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "", false)
	if err := op.startup.Save(t.Context(), &ContainerStartup{Phase: "preparing", Next: containerTestState("kube2", "next"), Previous: store.state}); err != nil {
		t.Fatal(err)
	}
	if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); !handled || err == nil {
		t.Fatalf("ResumeRepave = %t, %v", handled, err)
	}
	if err := op.RestartNode(t.Context(), discardLogger()); err == nil {
		t.Fatal("soft reboot bypassed pending preparation")
	}
	if len(*events) != 0 {
		t.Fatalf("unsafe preparation recovery: %v", *events)
	}
}

func TestContainerRepaveRotatesSlots(t *testing.T) {
	t.Parallel()
	for _, settings := range []string{"old", "new"} {
		t.Run("settings="+settings, func(t *testing.T) {
			t.Parallel()
			op, _, _ := containerOperatorFixture(t, "", false)
			goal := aksmachine.GoalState{SettingsVersion: settings, KubernetesVersion: "1.36.2"}
			for _, slot := range []string{"kube2", "kube1"} {
				state, err := op.ApplyGoalState(t.Context(), discardLogger(), goal)
				if err != nil {
					t.Fatal(err)
				}
				if state.ActiveMachine != slot || state.AppliedSettingsVersion != settings {
					t.Fatalf("repave did not rotate slot: slot=%s settings=%s", state.ActiveMachine, state.AppliedSettingsVersion)
				}
				if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); err != nil || !handled {
					t.Fatalf("complete startup: handled=%t, err=%v", handled, err)
				}
				if pending, err := op.startup.Load(); err != nil || pending != nil {
					t.Fatalf("completed startup remains: %+v, %v", pending, err)
				}
			}
		})
	}
}

func TestContainerRepaveRefreshesARMInputs(t *testing.T) {
	t.Parallel()
	op, _, _ := containerOperatorFixture(t, "", false)
	refresher := &fakeBootstrapDataRefresher{data: &bootstrapdata.Data{BootstrapToken: "fresh-bootstrap-token", CACertData: "fresh-ca"}}
	op.bootstrapDataRefresher = refresher
	op.cfg.Azure.BootstrapToken = &config.BootstrapTokenConfig{Token: "expired-bootstrap-token"}
	state, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.36.2"})
	if err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || state.NodeConfig.Kubelet.Auth.BootstrapToken != "fresh-bootstrap-token" || state.NodeConfig.Cluster.CaCertBase64 != "fresh-ca" {
		t.Fatal("container repave did not refresh ARM bootstrap credentials")
	}
	if op.cfg.Azure.BootstrapToken.Token != "expired-bootstrap-token" {
		t.Fatal("repave mutated startup configuration")
	}
}
