package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/bootstrapdata"
	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func containerTestState(slot, version string) *State {
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true}}
	return noneRegistrationState(&State{Isolation: isolation.ModeContainer, ActiveMachine: slot,
		AppliedSettingsVersion: version, NodeConfig: config.ToAgentConfig(cfg, slot), NPDVersion: "v1.35.1"})
}

type fakeContainerNode struct {
	params  containerops.Params
	events  *[]string
	fail    string
	backend container.Backend
	journal containerStartupStore
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
func (n *fakeContainerNode) CommitUpgrade() error          { return n.call("commit") }
func (n *fakeContainerNode) UpgradeWithStartup(ctx context.Context, _ containerNode, publish func(context.Context) error) error {
	for _, event := range []string{"target-cleanup", "prepare", "source-stop", "configure"} {
		if err := n.call(event); err != nil {
			return err
		}
	}
	if err := publish(ctx); err != nil {
		return err
	}
	startup, err := n.journal.Load()
	if err != nil || startup == nil || startup.Phase != "starting" {
		return errors.New("kubelet exposed without durable startup")
	}
	if n.params.Config.Kubelet.Labels[noneGenerationLabel] != startup.Next.NodeConfig.Kubelet.Labels[noneGenerationLabel] {
		return errors.New("published generation differs from kubelet configuration")
	}
	return n.call("start")
}

func containerOperatorFixture(t *testing.T, fail string, failSave bool) (*containerNodeOperator, *[]string, *noneTestStore) {
	t.Helper()
	events := new([]string)
	state := containerTestState("kube1", "old")
	store := &noneTestStore{events: events, state: state, failSave: failSave}
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true}}
	op, err := newContainerNodeOperator(&nspawnNodeOperator{cfg: cfg, state: store, bootstrapDataRefresher: &fakeBootstrapDataRefresher{}}, state)
	if err != nil {
		t.Fatal(err)
	}
	op.startup = containerStartupStore{path: filepath.Join(t.TempDir(), "startup.json")}
	op.newNode = func(p containerops.Params) containerNode {
		return &fakeContainerNode{params: p, events: events, fail: fail, journal: op.startup}
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

func TestContainerRepavePublicationAndCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fail, phase string
		failSave, wantErr bool
		want              []string
	}{
		{name: "healthy", phase: "finished", want: []string{"stage", "target-cleanup", "prepare", "source-stop", "configure", "start", "npd", "persist", "destroy-kube1", "commit"}},
		{name: "target cleanup fails closed", fail: "target-cleanup", phase: "preparing", wantErr: true, want: []string{"stage", "target-cleanup"}},
		{name: "prepare retains source", fail: "prepare", phase: "preparing", wantErr: true, want: []string{"stage", "target-cleanup", "prepare"}},
		{name: "source stop fails closed", fail: "source-stop", phase: "preparing", wantErr: true, want: []string{"stage", "target-cleanup", "prepare", "source-stop"}},
		{name: "start retryable", fail: "start", phase: "starting", wantErr: true, want: []string{"stage", "target-cleanup", "prepare", "source-stop", "configure", "start"}},
		{name: "NPD retryable", fail: "npd", phase: "starting", wantErr: true, want: []string{"stage", "target-cleanup", "prepare", "source-stop", "configure", "start", "npd"}},
		{name: "state failure keeps source", failSave: true, phase: "starting", wantErr: true, want: []string{"stage", "target-cleanup", "prepare", "source-stop", "configure", "start", "npd", "persist"}},
		{name: "source cleanup failure fenced", fail: "destroy-kube1", phase: "starting", wantErr: true, want: []string{"stage", "target-cleanup", "prepare", "source-stop", "configure", "start", "npd", "persist", "destroy-kube1"}},
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

func TestContainerStartupResumeFrozenAndFenced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, observed string
		clear          bool
	}{
		{name: "Node absent"}, {name: "unlabeled Node", observed: "missing"},
		{name: "old registration", observed: "stale"}, {name: "exact registration", observed: "match", clear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			op, events, store := containerOperatorFixture(t, "", false)
			frozen := containerTestState("kube2", "frozen")
			frozen.NodeConfig.Cluster.Version = "1.36.2"
			if err := op.startup.Save(t.Context(), &containerStartup{Phase: "starting", Next: frozen, Previous: store.state}); err != nil {
				t.Fatal(err)
			}
			op.cfg.Components.Kubernetes = "1.99.0"
			op.resolve = func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
				t.Fatal("resume resolved a newer goal")
				return nil, nil, nil, nil
			}
			var observed *corev1.Node
			if tc.observed != "" {
				observed = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}
				switch tc.observed {
				case "match":
					observed.Labels = frozen.NodeConfig.Kubelet.Labels
				case "stale":
					observed.Labels[noneGenerationLabel] = "old-registration"
				}
			}
			for range 2 {
				handled, err := op.ResumeRepave(t.Context(), discardLogger(), observed)
				if err != nil {
					t.Fatal(err)
				}
				if !handled && !tc.clear {
					t.Fatal("startup fence cleared early")
				}
			}
			if !slices.Equal(*events, []string{"resume-start", "npd", "persist", "destroy-kube1", "commit"}) {
				t.Fatalf("resume changed shared configuration: %v", *events)
			}
			if !reflect.DeepEqual(store.state, frozen) {
				t.Fatal("did not persist frozen state")
			}
			startup, err := op.startup.Load()
			if err != nil || (startup == nil) != tc.clear {
				t.Fatalf("startup fence: %+v, %v", startup, err)
			}
			if !tc.clear {
				if _, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "newer"}); err == nil {
					t.Fatal("pending startup authorized another repave")
				}
			}
		})
	}
}

func TestContainerPreExposureRecoveryFailsClosed(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "", false)
	if err := op.startup.Save(t.Context(), &containerStartup{Phase: "preparing", Next: containerTestState("kube2", "next"), Previous: store.state}); err != nil {
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

func TestContainerSelectionAndEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, configured              string
		applied                       isolation.Mode
		optIn, containerized, wantErr bool
	}{
		{name: "container opted in", configured: "container", applied: isolation.ModeContainer, optIn: true},
		{name: "container requires opt in", configured: "container", applied: isolation.ModeContainer, wantErr: true},
		{name: "containerized agent refused", configured: "container", applied: isolation.ModeContainer, optIn: true, containerized: true, wantErr: true},
		{name: "container cannot migrate nspawn", configured: "container", optIn: true, wantErr: true},
		{name: "container cannot migrate none", configured: "container", applied: isolation.ModeNone, optIn: true, wantErr: true},
		{name: "nspawn cannot adopt container", applied: isolation.ModeContainer, wantErr: true},
		{name: "none cannot adopt container", configured: "none", applied: isolation.ModeContainer, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: tc.configured, ExperimentalContainerIsolation: tc.optIn}}
			state := containerTestState("kube1", "old")
			state.Isolation = tc.applied
			err := validateIsolationEnvironment(cfg, tc.containerized)
			if err == nil {
				_, err = newNodeOperator(t.Context(), cfg, &testStateStore{state: state})
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("selection = %v", err)
			}
		})
	}
}

func TestContainerHostRecipeAndHelper(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true}}
	recipe := setupHost(cfg, discardLogger(), false).Name()
	for _, required := range []string{"disable-containerd", "disable-kubelet", "remove-apt-hardening", "load-kernel-modules"} {
		if !strings.Contains(recipe, required) {
			t.Errorf("missing %s: %s", required, recipe)
		}
	}
	if strings.Contains(recipe, "harden-apt") || strings.Contains(recipe, "install-systemd-container") {
		t.Fatalf("container recipe includes nspawn host preparation: %s", recipe)
	}
	node := newContainerNode(containerops.Params{Log: discardLogger(), Slot: "kube1"})
	if node.Backend().HelperPath != defaultAgentUpgradePaths().BinaryPath {
		t.Fatal("container helper is not the stable Flex executable")
	}
	unit, err := renderAgentServiceUnit("/agent", agentServiceOptionsFromConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "machines.target") || !strings.Contains(string(unit), "# Node isolation: container") {
		t.Fatalf("incorrect container agent service: %s", unit)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ServiceUnitName), unit, 0o600); err != nil {
		t.Fatal(err)
	}
	installed, err := installedAgentServiceOptions(dir)
	if err != nil || !installed.ContainerIsolation || installed.NoneIsolation {
		t.Fatalf("installed service lost its mode: %+v, %v", installed, err)
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

func TestContainerHubIntentDoesNotOperateNSpawn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		enabled bool
		reason  string
	}{
		{name: "disabled", reason: hubReasonDisabled}, {name: "unsupported", enabled: true, reason: "Unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			adapter, err := newHubMachineAdapter(func(string) (hubTransitionOperations, error) {
				t.Fatal("container DT invoked nspawn operations")
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var condition corev1.NodeCondition
			adapter.setCondition = func(_ context.Context, c corev1.NodeCondition) error { condition = c; return nil }
			for range 2 {
				if err := adapter.ReconcileHub(t.Context(), aksmachine.GoalState{DisconnectToleranceEnabled: &tc.enabled}, containerTestState("kube1", "old"), nil); err != nil {
					t.Fatal(err)
				}
			}
			if condition.Reason != tc.reason {
				t.Fatalf("condition = %s, want %s", condition.Reason, tc.reason)
			}
		})
	}
}
