package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	basenone "github.com/Azure/unbounded/pkg/agent/noneops"
	noneops "github.com/Azure/unbounded/pkg/agent/noneops/transaction"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type completionFixture struct {
	op       *noneNodeOperator
	store    *noneTestStore
	params   noneops.Params
	active   map[string]bool
	calls    []string
	reaps    int
	cleanups int
	resolves int
	npdCalls int
	failNPD  bool
}

func completionGoal(cfg *agentconfig.AgentConfig) *goalstates.MachineGoalState {
	return &goalstates.MachineGoalState{
		RootFS: &goalstates.RootFS{KubernetesVersion: cfg.Cluster.Version, HostArch: "amd64", ContainerdVersion: "2.1.8", RunCVersion: "1.5.0", CNIPluginVersion: "1.5.1"},
		NodeStart: &goalstates.NodeStart{
			NodeName: "node1", MachineName: noneops.Slot,
			Containerd: goalstates.Containerd{SandboxImage: "pause", CNIBinDir: "/opt/cni/bin", CNIConfDir: "/etc/cni/net.d"},
			Gantry:     goalstates.Gantry{Disabled: true},
			Kubelet: goalstates.Kubelet{APIServer: "https://api", ClusterDNS: "10.0.0.10", CACertData: []byte("ca"), NodeLabels: cfg.Kubelet.Labels,
				KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "fixture-token"}},
		},
	}
}

func newCompletionFixture(t *testing.T) *completionFixture {
	t.Helper()
	f := &completionFixture{active: make(map[string]bool)}
	root := t.TempDir()
	f.params = noneops.Params{
		StateDir: config.ConfigDir,
		ReapPods: func() error { f.reaps++; return nil },
		Overrides: basenone.Overrides{
			Layout: none.Layout{Base: filepath.Join(root, "node")}, SystemdDir: filepath.Join(root, "etc/systemd/system"), Proc: filepath.Join(root, "proc"),
			ImageArchiveDir: filepath.Join(root, "images"), Download: func(context.Context) error { return nil },
			WaitKubelet: func(context.Context) error { return nil },
			Run: func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
				f.calls = append(f.calls, name+" "+strings.Join(args, " "))
				if name == "systemctl" && len(args) > 1 {
					unit := args[len(args)-1]
					switch args[0] {
					case "is-active":
						if !f.active[unit] {
							return "inactive", errors.New("inactive")
						}
						return "active", nil
					case "enable", "restart":
						f.active[unit] = true
					case "stop":
						delete(f.active, unit)
					}
				}
				return "", nil
			},
		},
		Hooks: hostnode.Hooks{HostRoot: root,
			SetupNetwork: func(context.Context) error { return nil }, CleanupNetwork: func(context.Context) error { f.cleanups++; return nil }},
	}
	var events []string
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}, Components: config.ComponentsConfig{Kubernetes: "1.34.1"}}
	ac := config.ToAgentConfig(cfg, noneops.Slot)
	f.store = &noneTestStore{events: &events, state: &State{Isolation: isolation.ModeNone, NodeConfig: ac, AppliedSettingsVersion: "41", AppliedKubernetesVersion: "1.34.1"}}
	factory := func(p noneops.Params) noneNode {
		p.Overrides, p.Hooks, p.ReapPods = f.params.Overrides, f.params.Hooks, f.params.ReapPods
		p.Resolve = func(string) (*goalstates.MachineGoalState, error) { return completionGoal(p.Config), nil }
		if p.Previous != nil {
			p.PreviousGoalState = completionGoal(p.Previous)
		}
		if p.GoalState != nil && p.GoalState.NodeStart.Kubelet.NodeLabels[noneGenerationLabel] != p.Config.Kubelet.Labels[noneGenerationLabel] {
			t.Fatal("resolved goal lost the stamped registration label")
		}
		p.Install = nil // Artifact downloads are outside this reconciliation fixture.
		return newNoneRuntime(p)
	}
	oldParams := f.params
	oldParams.Log, oldParams.Config, oldParams.GoalState = discardLogger(), ac, completionGoal(ac)
	if err := newNoneRuntime(oldParams).Provision(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.op = &noneNodeOperator{
		nspawnNodeOperator: &nspawnNodeOperator{cfg: cfg, state: f.store, bootstrapDataRefresher: &fakeBootstrapDataRefresher{}}, newNode: factory,
		resolve: func(_ context.Context, _ *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
			f.resolves++
			ac := config.ToAgentConfig(cfg, slot)
			return ac, completionGoal(ac), nil, nil
		},
		stage: func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error { return nil },
		startNPD: func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error {
			f.npdCalls++
			if f.failNPD {
				return errors.New("NPD start interrupted")
			}
			return nil
		},
	}
	return f
}

func (f *completionFixture) assertNondestructive(t *testing.T, calls, reaps, cleanups int) {
	t.Helper()
	if f.reaps != reaps || f.cleanups != cleanups {
		t.Fatal("completion repeated pod/network cleanup")
	}
	for _, call := range f.calls[calls:] {
		if strings.Contains(call, "systemctl stop") || strings.Contains(call, "systemctl restart") {
			t.Fatalf("completion disturbed running services: %s", call)
		}
	}
}

func completionNode(state *State, generation string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", UID: types.UID(generation), Labels: state.NodeConfig.Kubelet.Labels}}
}

func TestNoneRegistrationFenceChangesOnSourceRestart(t *testing.T) {
	t.Parallel()
	state := &State{AppliedSettingsVersion: "41", NodeConfig: &agentconfig.AgentConfig{Isolation: "none"}}
	first, second := noneRegistrationState(state), noneRegistrationState(state)
	if first.NodeConfig.Kubelet.Labels[noneGenerationLabel] == second.NodeConfig.Kubelet.Labels[noneGenerationLabel] {
		t.Fatal("re-exposing the same source generation reused its old registration fence")
	}
	if state.NodeConfig.Kubelet.Labels[noneGenerationLabel] != "" {
		t.Fatal("registration preparation mutated the applied state")
	}
}

func TestNoneCompletionAfterRecreatedNode(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"npd", "state save"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			f := newCompletionFixture(t)
			f.failNPD, f.store.failSave = failure == "npd", failure == "state save"
			goal := aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}
			if _, err := f.op.ApplyGoalState(t.Context(), discardLogger(), goal); err == nil {
				t.Fatal("expected post-start failure")
			}
			node := f.op.newNode(noneops.Params{Log: discardLogger(), Config: f.store.state.NodeConfig})
			s, err := node.PendingStartup()
			if err != nil || s == nil {
				t.Fatalf("missing startup intent: %v", err)
			}
			var frozen State
			if err := json.Unmarshal(s.Completion, &frozen); err != nil {
				t.Fatal(err)
			}
			args, err := os.ReadFile(filepath.Join(f.params.SystemdDir, none.UnitKubelet+".d", "20-node-config.conf"))
			if err != nil || !strings.Contains(string(args), noneGenerationLabel+"="+frozen.NodeConfig.Kubelet.Labels[noneGenerationLabel]) {
				t.Fatalf("frozen registration fence did not reach rendered kubelet arguments: %v", err)
			}
			info, err := os.Stat(filepath.Join(f.params.HostRoot, "etc/aks-flex-node/none-startup.json"))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("startup permissions: %v", err)
			}
			kube := fakeClient(completionNode(&frozen, "recreated"))
			machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: goal}}
			r := newTestRepaveReconciler(t, machines, kube, f.op)
			beforeCalls, beforeReaps, beforeCleanups := len(f.calls), f.reaps, f.cleanups
			f.failNPD, f.store.failSave = false, false
			// Simulate daemon replacement using the on-disk frozen record, not
			// the configuration that a newer process happens to desire.
			restarted := *f.op
			r.operator = &restarted
			for range 2 {
				if err := r.reconcileOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			f.assertNondestructive(t, beforeCalls, beforeReaps, beforeCleanups)
			if f.resolves != 1 || f.store.state.AppliedSettingsVersion != "42" || f.store.state.AppliedKubernetesVersion != "1.34.2" {
				t.Fatal("completion reapplied or lost the frozen goal")
			}
			if s, err := node.PendingStartup(); err != nil || s != nil {
				t.Fatalf("completed startup not cleared: %v", err)
			}
		})
	}
}

func TestNoneCompletionFailsClosedOnChangedIntent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*noneops.Startup)
	}{
		{name: "record version", change: func(s *noneops.Startup) { s.RecordVersion = 2 }},
		{name: "active version mismatch", change: func(s *noneops.Startup) { s.Version = "not-active" }},
		{name: "frozen config mismatch", change: func(s *noneops.Startup) { s.Config.NodeName = "another-worker" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCompletionFixture(t)
			f.failNPD = true
			if _, err := f.op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}); err == nil {
				t.Fatal("expected incomplete startup")
			}
			path := filepath.Join(f.params.HostRoot, "etc/aks-flex-node/none-startup.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var s noneops.Startup
			if err := json.Unmarshal(data, &s); err != nil {
				t.Fatal(err)
			}
			tc.change(&s)
			data, err = json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			calls, reaps, cleanups := len(f.calls), f.reaps, f.cleanups
			if _, err := f.op.ResumeRepave(t.Context(), discardLogger(), nil); err == nil {
				t.Fatal("changed startup intent was accepted")
			}
			f.assertNondestructive(t, calls, reaps, cleanups)
			if len(f.calls) != calls || f.store.state.AppliedSettingsVersion != "41" {
				t.Fatal("invalid intent caused service operations or state publication")
			}
		})
	}
}

func TestNoneCompletionRequiresFreshGenerationBeforeNewerGoal(t *testing.T) {
	t.Parallel()
	f := newCompletionFixture(t)
	f.failNPD = true
	oldGoal := aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}
	if _, err := f.op.ApplyGoalState(t.Context(), discardLogger(), oldGoal); err == nil {
		t.Fatal("expected interrupted completion")
	}
	f.failNPD = false
	newGoal := aksmachine.GoalState{SettingsVersion: "43", KubernetesVersion: "1.34.3"}
	machines := &fakeMachineClient{machine: &aksmachine.Machine{Goal: newGoal}}
	staleState := noneRegistrationState(f.store.state)
	stale := completionNode(staleState, "stale-old-node")
	kube := fakeClient(stale)
	r := newTestRepaveReconciler(t, machines, kube, f.op)
	calls, reaps, cleanups := len(f.calls), f.reaps, f.cleanups
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	// The cached old Node must not release the barrier, even if a subsequent
	// read reports the original absence while a newer goal is already pending.
	for range 2 {
		if err := r.reconcileOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f.assertNondestructive(t, calls, reaps, cleanups)
	if f.resolves != 1 || f.store.state.AppliedSettingsVersion != "42" {
		t.Fatal("newer goal consumed the previous deletion signal")
	}
	fresh := completionNode(f.store.state, "fresh-node")
	if err := kube.Create(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.resolves != 1 {
		t.Fatal("newer goal repaved an existing fresh Node")
	}
	if err := kube.Delete(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.resolves != 2 || f.store.state.AppliedSettingsVersion != "43" {
		t.Fatal("fresh RP deletion did not authorize the newer goal")
	}
}

func TestNonePreExposureRecoveryWaitsForRPDeletion(t *testing.T) {
	t.Parallel()
	f := newCompletionFixture(t)
	run := f.params.Run
	f.params.Run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) > 1 && args[0] == "enable" && args[len(args)-1] == none.UnitContainerd {
			return "", errors.New("both runtime starts are unavailable")
		}
		return run(ctx, log, name, args...)
	}
	goal := aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}
	if _, err := f.op.ApplyGoalState(t.Context(), discardLogger(), goal); err == nil {
		t.Fatal("expected pre-exposure failure")
	}
	old := completionNode(f.store.state, "already-recreated")
	kube := fakeClient(old)
	r := newTestRepaveReconciler(t, &fakeMachineClient{machine: &aksmachine.Machine{Goal: goal}}, kube, f.op)
	calls, reaps, cleanups := len(f.calls), f.reaps, f.cleanups
	if err := r.reconcileOnce(t.Context()); err == nil {
		t.Fatal("destructive recovery was allowed while a Node exists")
	}
	f.assertNondestructive(t, calls, reaps, cleanups)
	f.params.Run = run
	if err := kube.Delete(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.store.state.AppliedSettingsVersion != "41" {
		t.Fatal("source restoration was reported as the requested goal")
	}
	source := completionNode(f.store.state, "restored-source")
	if err := kube.Create(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.resolves != 1 {
		t.Fatal("restored source was automatically repaved")
	}
}

// Reset must not be coupled to a failed startup or its health dependencies.
type resetCompletionOperator struct {
	*fakeNodeOperator
	resumed bool
}

func (o *resetCompletionOperator) ResumeRepave(context.Context, *slog.Logger, *corev1.Node) (bool, error) {
	o.resumed = true
	return true, errors.New("startup unavailable")
}

func TestNoneCompletionRespectsDeletePrecedence(t *testing.T) {
	t.Parallel()
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("machine removed=%t", removed), func(t *testing.T) {
			t.Parallel()
			op := &resetCompletionOperator{fakeNodeOperator: &fakeNodeOperator{state: &State{AppliedSettingsVersion: "41"}}}
			machines := &fakeMachineClient{notFound: removed, machine: &aksmachine.Machine{Goal: aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}
			r := newTestRepaveReconciler(t, machines, fakeClient(node), op)
			if err := r.reconcileOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if op.resumed || op.reset != removed || op.stopped != removed {
				t.Fatal("startup completion bypassed RP deletion/reset precedence")
			}
		})
	}
}
