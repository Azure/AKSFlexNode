package start

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestBootstrapOrdersHooksBeforeWorkerAndState(t *testing.T) {
	t.Parallel()

	gs := &goalstates.MachineGoalState{
		RootFS:    &goalstates.RootFS{MachineDir: t.TempDir()},
		NodeStart: &goalstates.NodeStart{},
	}
	log := slog.New(slog.DiscardHandler)
	cfg := &config.Config{}
	input := agentruntime.Inputs{
		Config: cfg,
		Node: agentruntime.Node{
			Slot:      "kube1",
			GoalState: gs,
		},
	}
	saveTask := hostnode.Func("save-daemon-state", func(context.Context) error { return nil })
	name := bootstrapTasks(cfg, log, agentruntime.NSpawn{}, input, false, saveTask).Name()

	hooks := strings.Index(name, "ensure-nspawn-lifecycle-units")
	archives := strings.Index(name, "download-container-image-archives")
	provision := strings.Index(name, "ensure-nspawn-workspace")
	npd := strings.Index(name, "start-npd")
	save := strings.Index(name, "save-daemon-state")
	if hooks < 0 || archives <= hooks || provision <= archives || npd <= provision || save <= npd {
		t.Fatalf("bootstrap task order = %s", name)
	}
}

type bootstrapTestRuntime struct {
	input  agentruntime.Inputs
	events []string
}

func (*bootstrapTestRuntime) Worker(*slog.Logger, agentruntime.Node) (agentruntime.WorkerContext, error) {
	return agentruntime.WorkerContext{}, nil
}

func (r *bootstrapTestRuntime) Start(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	r.input = input
	return hostnode.Func("provision", func(context.Context) error {
		r.events = append(r.events, "provision")
		return nil
	})
}

func (r *bootstrapTestRuntime) Restart(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	r.input = input
	return hostnode.Func("restart", func(context.Context) error {
		r.events = append(r.events, "restart")
		return nil
	})
}

func (r *bootstrapTestRuntime) Prepare(*slog.Logger, agentruntime.Inputs) phases.Task {
	return hostnode.Func("recover", func(context.Context) error {
		r.events = append(r.events, "recover")
		return nil
	})
}

func (*bootstrapTestRuntime) Retire(*slog.Logger, agentruntime.Inputs) phases.Task {
	return hostnode.Func("retire", func(context.Context) error { return nil })
}

func (*bootstrapTestRuntime) PlanReset(*slog.Logger) (agentruntime.ResetPlan, error) {
	return nil, nil
}

func TestNoneReapplyUsesInstalledConfigWithoutSavingOrProvisioning(t *testing.T) {
	t.Parallel()

	installed := &agentconfig.AgentConfig{
		Isolation: "none",
		Cluster:   agentconfig.AgentClusterConfig{Version: "1.34.0"},
	}
	cfg := &config.Config{
		Bootstrap:  config.BootstrapConfig{Isolation: isolation.ModeNone},
		Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
	}
	worker := &bootstrapTestRuntime{}
	input := agentruntime.Inputs{
		Config: cfg,
		Node: agentruntime.Node{
			Slot:   "kube1",
			Config: installed,
		},
	}
	saveTask := hostnode.Func("save-daemon-state", func(context.Context) error { return nil })
	name := bootstrapTasks(cfg, slog.New(slog.DiscardHandler), worker, input, true, saveTask).Name()
	if worker.input.Node.Config != installed || !strings.Contains(name, "recover") || !strings.Contains(name, "restart") {
		t.Fatalf("reapply did not select installed config: %s", name)
	}
	if strings.Contains(name, "provision") || strings.Contains(name, "save-daemon-state") || strings.Contains(name, "install-packages") {
		t.Fatalf("reapply attempted host setup, provisioning, or state save: %s", name)
	}
}

type bootstrapTestMachine struct {
	remote  *aksmachine.Machine
	gets    int
	creates int
}

func (m *bootstrapTestMachine) Get(context.Context) (*aksmachine.Machine, error) {
	m.gets++
	if m.remote == nil {
		return nil, &aksmachine.NotFoundError{}
	}
	return m.remote, nil
}

func (m *bootstrapTestMachine) Create(_ context.Context, goal aksmachine.GoalState) (*aksmachine.Machine, error) {
	m.creates++
	goal.SettingsVersion = "etag-created"
	return &aksmachine.Machine{Goal: goal}, nil
}

func (*bootstrapTestMachine) PatchStatus(context.Context, aksmachine.Status) error {
	return nil
}

type bootstrapTestStore struct {
	state *daemon.State
	saves int
}

func (s *bootstrapTestStore) Load(context.Context) (*daemon.State, error) {
	return s.state, nil
}

func (s *bootstrapTestStore) Save(_ context.Context, state *daemon.State) error {
	s.saves++
	copy := *state
	s.state = &copy
	return nil
}

func (s *bootstrapTestStore) Delete(context.Context) error {
	s.state = nil
	return nil
}

func bootstrapConfig(mode isolation.Mode) *config.Config {
	return &config.Config{
		Bootstrap:  config.BootstrapConfig{Isolation: mode},
		Components: config.ComponentsConfig{Kubernetes: "1.34.0"},
		Agent:      config.AgentConfig{NodeName: "node1"},
		Azure: config.AzureConfig{
			BootstrapToken: &config.BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
		Node: config.NodeConfig{
			Kubelet: config.KubeletConfig{
				ClusterFQDN: "https://cluster.example",
				CACertData:  "ca",
			},
		},
	}
}

func TestFreshBootstrapRegistersBeforeSavingAppliedState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		mode       isolation.Mode
		registered bool
	}{
		{
			name:       "nspawn existing",
			mode:       isolation.ModeNSpawn,
			registered: true,
		},
		{
			name: "nspawn created",
			mode: isolation.ModeNSpawn,
		},
		{
			name:       "none existing",
			mode:       isolation.ModeNone,
			registered: true,
		},
		{
			name: "none created",
			mode: isolation.ModeNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := bootstrapConfig(tc.mode)
			state, goal, reapply, err := selectBootstrap(cfg, nil)
			if err != nil || reapply {
				t.Fatalf("select bootstrap: reapply=%t, err=%v", reapply, err)
			}
			machines := &bootstrapTestMachine{}
			wantETag, wantCreates := "etag-created", 1
			if tc.registered {
				machines.remote = &aksmachine.Machine{
					Goal: aksmachine.GoalState{
						KubernetesVersion: "1.34.0",
						SettingsVersion:   "etag-registered",
					},
				}
				wantETag, wantCreates = "etag-registered", 0
			}
			log := slog.New(slog.DiscardHandler)
			if err := registerBootstrap(t.Context(), log, cfg, machines, &goal, state, reapply); err != nil {
				t.Fatal(err)
			}
			worker := &bootstrapTestRuntime{}
			store := &bootstrapTestStore{}
			input := agentruntime.Inputs{
				Config: cfg,
				Node: agentruntime.Node{
					Slot:   state.ActiveMachine,
					Config: config.ToAgentConfig(cfg, state.ActiveMachine),
				},
			}
			if tc.mode == isolation.ModeNone {
				state.NodeConfig = input.Node.Config.DeepCopy()
			}
			saveTask := hostnode.Func("save-daemon-state", func(ctx context.Context) error {
				return store.Save(ctx, state)
			})
			if err := phases.Serial(log, workerBootstrapTasks(log, worker, input, reapply, saveTask)...).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			if machines.gets != 1 || machines.creates != wantCreates || store.saves != 1 || store.state.AppliedSettingsVersion != wantETag || store.state.AppliedKubernetesVersion != "1.34.0" {
				t.Fatalf("registration/save mismatch: gets=%d creates=%d saves=%d", machines.gets, machines.creates, store.saves)
			}
			if !reflect.DeepEqual(worker.events, []string{"provision"}) {
				t.Fatalf("worker events = %v", worker.events)
			}
		})
	}
}

func TestInstalledReapplyDoesNotContactOrRewriteMachine(t *testing.T) {
	t.Parallel()

	cfg := bootstrapConfig(isolation.ModeNone)
	appliedConfig := config.ToAgentConfig(cfg, "kube1")
	appliedConfig.Cluster.Version = "1.35.0"
	applied := &daemon.State{
		Isolation:                isolation.ModeNone,
		ActiveMachine:            "kube1",
		AppliedKubernetesVersion: "1.35.0",
		AppliedSettingsVersion:   "etag-applied",
		NodeConfig:               appliedConfig,
	}
	state, goal, reapply, err := selectBootstrap(cfg, applied)
	if err != nil || !reapply || state != applied {
		t.Fatalf("select installed worker: reapply=%t, err=%v", reapply, err)
	}
	machines := &bootstrapTestMachine{
		remote: &aksmachine.Machine{
			Goal: aksmachine.GoalState{
				KubernetesVersion: "1.35.0",
				SettingsVersion:   "etag-newer",
			},
		},
	}
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
	saveTask := hostnode.Func("save-daemon-state", func(ctx context.Context) error {
		return store.Save(ctx, state)
	})
	if err := phases.Serial(log, workerBootstrapTasks(log, worker, input, reapply, saveTask)...).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if machines.gets != 0 || machines.creates != 0 || store.saves != 0 {
		t.Fatalf("reapply touched machine/state: gets=%d creates=%d saves=%d", machines.gets, machines.creates, store.saves)
	}
	if !reflect.DeepEqual(worker.events, []string{"recover", "restart"}) || worker.input.Node.Config != appliedConfig || state.AppliedSettingsVersion != "etag-applied" {
		t.Fatalf("reapply did not retain installed worker: events=%v", worker.events)
	}
}

func TestInstalledReapplyRejectsChangedPairedKubeconfig(t *testing.T) {
	t.Parallel()

	cfg := bootstrapConfig(isolation.ModeNone)
	cfg.Agent.KubeconfigData = "host config"
	cfg.Node.Kubelet.KubeconfigData = "installed worker config"
	applied := &daemon.State{
		Isolation:     isolation.ModeNone,
		ActiveMachine: "kube1",
		NodeConfig:    config.ToAgentConfig(cfg, "kube1"),
	}
	cfg.Node.Kubelet.KubeconfigData = "different worker config"
	if _, _, _, err := selectBootstrap(cfg, applied); err == nil {
		t.Fatal("reapply accepted a different kubelet credential")
	}
}

func TestInstalledReapplyRejectsChangedExecIdentity(t *testing.T) {
	t.Parallel()

	cfg := bootstrapConfig(isolation.ModeNone)
	cfg.Azure.BootstrapToken = nil
	cfg.Azure.ManagedIdentity = &config.ManagedIdentityConfig{ClientID: "identity-one"}
	applied := &daemon.State{
		Isolation:     isolation.ModeNone,
		ActiveMachine: "kube1",
		NodeConfig:    config.ToAgentConfig(cfg, "kube1"),
	}
	cfg.Azure.ManagedIdentity.ClientID = "identity-two"
	if _, _, _, err := selectBootstrap(cfg, applied); err == nil {
		t.Fatal("reapply accepted a different managed identity")
	}
}

func TestInstalledReapplyRejectsIncompatibleIdentityBeforeWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		change  func(*config.Config)
		wantErr bool
	}{
		{
			name: "node name",
			change: func(cfg *config.Config) {
				cfg.Agent.NodeName = "another-node"
			},
			wantErr: true,
		},
		{
			name: "endpoint",
			change: func(cfg *config.Config) {
				cfg.Node.Kubelet.ClusterFQDN = "https://other.example"
			},
			wantErr: true,
		},
		{
			name: "cluster CA",
			change: func(cfg *config.Config) {
				cfg.Node.Kubelet.CACertData = "different-ca"
			},
			wantErr: true,
		},
		{
			name: "authentication mode",
			change: func(cfg *config.Config) {
				cfg.Azure.BootstrapToken = nil
				cfg.Azure.ManagedIdentity = &config.ManagedIdentityConfig{ClientID: "other"}
			},
			wantErr: true,
		},
		{
			name: "rotated bootstrap token",
			change: func(cfg *config.Config) {
				cfg.Azure.BootstrapToken.Token = "aaaaaa.bbbbbbbbbbbbbbbb"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			installedCfg := bootstrapConfig(isolation.ModeNone)
			applied := &daemon.State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube1",
				NodeConfig:    config.ToAgentConfig(installedCfg, "kube1"),
			}
			candidate := installedCfg.DeepCopy()
			tc.change(candidate)
			state, _, reapply, err := selectBootstrap(candidate, applied)
			if (err != nil) != tc.wantErr {
				t.Fatalf("selection error = %v", err)
			}
			if err == nil && (!reapply || state != applied) {
				t.Fatal("compatible reapply did not retain installed state")
			}
		})
	}
}
