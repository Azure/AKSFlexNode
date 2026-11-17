package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	runtimeprovider "github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type completionFixture struct {
	op          *noneNodeOperator
	store       *noneTestStore
	params      noneops.Params
	active      map[string]bool
	calls       []string
	npdVersions []string
	cleanups    int
	prepared    bool
	fail        string
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

type noneCommitFailure struct{ noneNode }

func (noneCommitFailure) Commit() error { return errors.New("commit interrupted") }

func newCompletionFixture(t *testing.T) *completionFixture {
	t.Helper()
	f := &completionFixture{active: make(map[string]bool)}
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	if err := os.Mkdir(proc, 0o700); err != nil {
		t.Fatal(err)
	}
	f.params = noneops.Params{
		Overrides: noneops.Overrides{
			Layout: none.Layout{Base: filepath.Join(root, "node")}, SystemdDir: filepath.Join(root, "etc/systemd/system"), Proc: proc,
			ImageArchiveDir: filepath.Join(root, "images"), Download: func(context.Context) error { return nil },
			WaitKubelet: func(context.Context) error {
				if f.fail == "kubelet" {
					return errors.New("target kubelet unavailable")
				}
				return nil
			},
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
						if unit == none.UnitKubelet && !f.prepared {
							t.Error("SDK started kubelet before Flex installed host dependencies")
						}
						f.active[unit] = true
					case "stop":
						delete(f.active, unit)
					}
				}
				return "", nil
			},
		},
		Hooks: hostnode.Hooks{HostRoot: root, ConfigSnapshotDir: filepath.Join(root, config.ConfigDir, "none-upgrade-config"),
			SetupNetwork: func(context.Context) error { return nil }, CleanupNetwork: func(context.Context) error { f.cleanups++; return nil }},
	}
	var events []string
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}, Components: config.ComponentsConfig{Kubernetes: "1.34.1"}, Npd: config.NPDConfig{Version: "v1.36.0"}}
	ac := config.ToAgentConfig(cfg, noneops.Slot)
	f.store = &noneTestStore{events: &events, state: &runtimeprovider.State{Isolation: isolation.ModeNone, NodeConfig: ac, AppliedSettingsVersion: "41", AppliedKubernetesVersion: "1.34.1"}}
	factory := func(p noneops.Params) noneNode {
		p.Overrides, p.Hooks = f.params.Overrides, f.params.Hooks
		p.Resolve = func(string) (*goalstates.MachineGoalState, error) { return completionGoal(p.Config), nil }
		if p.Previous != nil {
			p.PreviousGoalState = completionGoal(p.Previous)
		}
		node := newNoneRuntime(p)
		if f.fail == "commit" {
			return noneCommitFailure{node}
		}
		return node
	}
	oldParams := f.params
	oldParams.Log, oldParams.Config, oldParams.GoalState = discardLogger(), ac, completionGoal(ac)
	old := newNoneRuntime(oldParams)
	if err := old.Provision(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.Configure(t.Context()); err != nil {
		t.Fatal(err)
	}
	selected, err := mustNodeProvider(t, cfg.Bootstrap.Isolation).NewOperator(t.Context(), cfg, f.store)
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	f.op, ok = selected.(*noneNodeOperator)
	if !ok {
		t.Fatalf("selected operator=%T", selected)
	}
	f.op.newNode = factory
	f.op.resolve = func(_ context.Context, _ *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
		ac := config.ToAgentConfig(cfg, slot)
		return ac, completionGoal(ac), nil, nil
	}
	f.op.stage = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error { return nil }
	f.op.dependencies = func(context.Context, *slog.Logger, *config.Config) error { f.prepared = true; return nil }
	f.op.installNPD = func(_ context.Context, _ *slog.Logger, cfg *config.Config, gs *goalstates.MachineGoalState) error {
		if f.fail == "artifacts" && cfg.Npd.Version == "v1.36.0" {
			return errors.New("NPD install interrupted")
		}
		path := gs.RootFS.Root().Path("opt/aks-flex-node/bin/node-problem-detector")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(cfg.Npd.Version), 0o600)
	}
	f.op.startNPD = func(_ context.Context, _ *slog.Logger, cfg *config.Config, _ *goalstates.NodeStart) error {
		f.npdVersions = append(f.npdVersions, cfg.Npd.Version)
		if f.fail == "npd" && cfg.Npd.Version == "v1.36.0" {
			return errors.New("NPD start interrupted")
		}
		return nil
	}
	return f
}

func (f *completionFixture) journalPath() string {
	return filepath.Join(filepath.Dir(f.params.SnapshotDir("none-upgrade-config")), "none-upgrade-journal.json")
}

func (f *completionFixture) assertCommitted(t *testing.T) {
	t.Helper()
	for _, path := range []string{f.journalPath(), f.params.SnapshotDir("none-upgrade-config")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("SDK recovery artifacts remain at %s: %v", path, err)
		}
	}
}

func (f *completionFixture) assertAppliedRuntime(t *testing.T, state *runtimeprovider.State) {
	t.Helper()
	p := f.params
	p.Log, p.Config, p.GoalState = discardLogger(), state.NodeConfig, completionGoal(state.NodeConfig)
	node := noneops.New(p)
	want, err := node.Version()
	if err != nil {
		t.Fatal(err)
	}
	current, err := p.Layout.Current()
	if err != nil || current != want {
		t.Fatalf("active runtime = %q, want %q: %v", current, want, err)
	}
	gs, err := node.GoalState()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(gs.RootFS.Root().Path("opt/aks-flex-node/bin/node-problem-detector"))
	if err != nil || string(binary) != f.op.cfg.Npd.Version {
		t.Fatalf("active NPD = %q, want %q: %v", binary, f.op.cfg.Npd.Version, err)
	}
	if len(f.npdVersions) == 0 || f.npdVersions[len(f.npdVersions)-1] != f.op.cfg.Npd.Version {
		t.Fatal("NPD restart did not select configured version")
	}
}

func TestNoneBaseUpgradeAndRecovery(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"1.34.1", "1.34.2"} {
		for _, failure := range []string{"", "kubelet", "artifacts", "npd", "state save", "commit"} {
			t.Run(version+"/"+failure, func(t *testing.T) {
				t.Parallel()
				f := newCompletionFixture(t)
				if err := f.op.recover(t.Context(), discardLogger()); err != nil {
					t.Fatal(err)
				}
				source, err := json.Marshal(f.store.state)
				if err != nil {
					t.Fatal(err)
				}
				f.fail, f.store.failSave = failure, failure == "state save"
				goal := aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: version}
				next, err := f.op.ApplyGoalState(t.Context(), discardLogger(), goal)
				wantErr := failure != "" && failure != "commit"
				if (err != nil) != wantErr {
					t.Fatalf("ApplyGoalState error = %v, wantErr %t", err, wantErr)
				}
				if failure == "kubelet" && !strings.Contains(err.Error(), "rolled back") {
					t.Fatalf("expected SDK rollback: %v", err)
				}
				if failure == "" || failure == "commit" {
					if next.AppliedSettingsVersion != "42" {
						t.Fatal("repave did not publish new settings")
					}
				} else {
					applied, err := json.Marshal(f.store.state)
					if err != nil || !bytes.Equal(applied, source) {
						t.Fatal("failed repave changed applied source configuration")
					}
				}
				if failure == "artifacts" || failure == "npd" || failure == "state save" || failure == "commit" {
					info, err := os.Stat(f.journalPath())
					if err != nil || info.Mode().Perm() != 0o600 {
						t.Fatalf("SDK journal missing or not private: %v", err)
					}
					data, err := os.ReadFile(f.journalPath())
					if err != nil || strings.Contains(string(data), "fixture-token") {
						t.Fatal("SDK journal exposed bootstrap credentials")
					}
				} else {
					f.assertCommitted(t)
				}
				// The SDK commits matching configs; changed configs roll back
				// unless the target was saved as applied.
				f.fail, f.store.failSave = "", false
				restarted := *f.op
				restarted.recovered = false
				f.op = &restarted
				cleanups := f.cleanups
				if handled, err := f.op.ResumeRepave(t.Context(), discardLogger(), nil); err != nil || handled {
					t.Fatalf("ResumeRepave = %t, %v", handled, err)
				}
				if failure == "" || failure == "commit" || failure == "kubelet" || version == "1.34.1" {
					if f.cleanups != cleanups {
						t.Fatal("recovery rolled back a matching configuration or repeated a completed rollback")
					}
				} else if f.cleanups != cleanups+1 {
					t.Fatal("uncommitted upgrade did not use base rollback")
				}
				f.assertCommitted(t)
				f.assertAppliedRuntime(t, f.store.state)
				calls, npdCalls := len(f.calls), len(f.npdVersions)
				if _, err := f.op.ResumeRepave(t.Context(), discardLogger(), nil); err != nil {
					t.Fatal(err)
				}
				if len(f.calls) != calls || len(f.npdVersions) != npdCalls {
					t.Fatal("completed recovery repeated host or NPD work")
				}
			})
		}
	}
}

func TestNoneBaseRecoveryRejectsCorruptJournal(t *testing.T) {
	t.Parallel()
	f := newCompletionFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.journalPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.journalPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := slices.Clone(f.calls)
	if _, err := f.op.ResumeRepave(t.Context(), discardLogger(), nil); err == nil {
		t.Fatal("unreadable SDK journal was accepted")
	}
	if !slices.Equal(before, f.calls) || len(f.npdVersions) != 0 {
		t.Fatal("corrupt journal changed services or selected NPD")
	}
}

// Reset must not depend on upgrade recovery or its health dependencies.
type resetCompletionOperator struct {
	*fakeNodeOperator
	resumed bool
}

func (o *resetCompletionOperator) ResumeRepave(context.Context, *slog.Logger, *corev1.Node) (bool, error) {
	o.resumed = true
	return true, errors.New("recovery unavailable")
}

func TestNoneCompletionRespectsDeletePrecedence(t *testing.T) {
	t.Parallel()
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("machine removed=%t", removed), func(t *testing.T) {
			t.Parallel()
			op := &resetCompletionOperator{fakeNodeOperator: &fakeNodeOperator{state: &runtimeprovider.State{AppliedSettingsVersion: "41"}}}
			machines := &fakeMachineClient{notFound: removed, machine: &aksmachine.Machine{Goal: aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.2"}}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{deletionTaint()}}}
			r := newTestRepaveReconciler(t, machines, fakeClient(node), op)
			if err := r.reconcileOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if op.resumed || op.reset != removed || op.stopped != removed {
				t.Fatal("recovery bypassed RP deletion/reset precedence")
			}
		})
	}
}
