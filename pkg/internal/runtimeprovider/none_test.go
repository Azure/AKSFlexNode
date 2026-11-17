package runtimeprovider

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

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestNoneEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		containerized bool
		wantErr       bool
	}{
		{name: "host-installed agent"},
		{name: "containerized agent", containerized: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := (None{}).ValidateEnvironment(tc.containerized); (err != nil) != tc.wantErr {
				t.Fatalf("ValidateEnvironment = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestNoneRuntimePolicy(t *testing.T) {
	t.Parallel()
	p := None{}
	if p.Mode() != isolation.ModeNone || p.NSpawnRuntime() != nil {
		t.Fatal("none selected nspawn runtime behavior")
	}
	want := []string{"network-online.target", "systemd-udev-settle.service"}
	if !slices.Equal(p.WantedTargets(), want) {
		t.Fatalf("WantedTargets = %v, want %v", p.WantedTargets(), want)
	}
	if checks := p.RuntimeChecks(&config.Config{}, slog.New(slog.DiscardHandler), false); len(checks) != 0 {
		t.Fatalf("unexpected runtime checks: %v", checks)
	}
}

func TestNoneAgentActionsDoNotTouchWorkerSlots(t *testing.T) {
	t.Parallel()
	p := None{Host: HostServices{
		CopyExecutable: func(source string, destination string) error {
			t.Error("none attempted to copy a worker helper")
			return errors.New("unexpected helper copy")
		},
		FilesHaveEqualSHA256: func(first string, second string) (bool, error) {
			t.Error("none attempted to inspect a worker helper")
			return false, errors.New("unexpected helper validation")
		},
	}}
	for _, slot := range []string{"", "kube1", "not-an-nspawn-slot"} {
		t.Run("slot="+slot, func(t *testing.T) {
			t.Parallel()
			source := filepath.Join(t.TempDir(), "missing-agent")
			for range 2 {
				for _, err := range []error{
					p.ValidateAgentActivation(t.Context()), p.ValidateActiveSlot(slot),
					p.SynchronizeAgent(source, slot), p.ValidateAgent(source, slot), p.RollbackAgent(source, slot),
				} {
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("agent action created a worker helper: %v", err)
			}
		})
	}
}

func TestNoneCheckAgentCandidate(t *testing.T) {
	t.Parallel()
	fixture, err := filepath.Abs("testdata/agent_isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, executable string
		missing, cancel  bool
		wantErr          bool
	}{
		{name: "supports none", executable: "none"},
		{name: "only supports nspawn", executable: "nspawn", wantErr: true},
		{name: "only supports container", executable: "container", wantErr: true},
		{name: "missing executable", executable: "missing", missing: true, wantErr: true},
		{name: "canceled", executable: "none", cancel: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			candidate := filepath.Join(t.TempDir(), tc.executable)
			if !tc.missing {
				if err := os.Symlink(fixture, candidate); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			for range 2 {
				err := (None{}).CheckAgentCandidate(ctx, candidate)
				if (err != nil) != tc.wantErr {
					t.Fatalf("CheckAgentCandidate = %v, wantErr %t", err, tc.wantErr)
				}
				if tc.cancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation was not propagated: %v", err)
				}
			}
		})
	}
}

func TestNoneValidateResetState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     *agentconfig.AgentConfig
		wantErr bool
	}{
		{name: "none", cfg: &agentconfig.AgentConfig{Isolation: "none"}},
		{name: "missing configuration", wantErr: true},
		{name: "undeclared mode", cfg: &agentconfig.AgentConfig{}, wantErr: true},
		{name: "nspawn configuration", cfg: &agentconfig.AgentConfig{Isolation: "nspawn"}, wantErr: true},
		{name: "container configuration", cfg: &agentconfig.AgentConfig{Isolation: "container"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := (None{}).ValidateResetState(&State{Isolation: isolation.ModeNone, NodeConfig: tc.cfg})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateResetState = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestNoneCheckBootstrapRejectsAppliedState(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{isolation.ModeNone, isolation.ModeNSpawn, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			for range 2 {
				err := (None{}).CheckBootstrap(t.Context(), slog.New(slog.DiscardHandler), &State{Isolation: mode})
				if err == nil || !strings.Contains(err.Error(), "reset it before bootstrapping") {
					t.Fatalf("installed node accepted for bootstrap: %v", err)
				}
			}
		})
	}
}

func TestNoneRequiredServices(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		extra  []string
		routes []config.StaticRoute
		want   []string
	}{
		{name: "route overlap", want: []string{"check-route-overlap.service"}},
		{name: "additional services", extra: []string{"custom.service"}, want: []string{"check-route-overlap.service", "custom.service"}},
		{name: "static routes", routes: []config.StaticRoute{{Destination: "10.0.0.0/24"}}, want: []string{"check-route-overlap.service", "static-routes.service"}},
		{name: "additional services and routes", extra: []string{"first.service", "second.service"}, routes: []config.StaticRoute{{Destination: "10.0.0.0/24"}}, want: []string{"check-route-overlap.service", "first.service", "second.service", "static-routes.service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Bootstrap:   config.BootstrapConfig{AdditionalRequiredServices: slices.Clone(tc.extra)},
				HostRouting: config.HostRoutingConfig{StaticRoutes: config.StaticRoutesConfig{Enabled: len(tc.routes) > 0, Routes: tc.routes}},
			}
			for range 2 {
				if got := NoneRequiredServices(cfg); !slices.Equal(got, tc.want) {
					t.Fatalf("NoneRequiredServices = %v, want %v", got, tc.want)
				}
			}
			if !slices.Equal(cfg.Bootstrap.AdditionalRequiredServices, tc.extra) {
				t.Fatal("service selection mutated the configuration")
			}
		})
	}
}

func TestNoneResetNodeRecipe(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	builds := 0
	p := None{Host: HostServices{RemoveHostHelpers: func(got *slog.Logger) phases.Task {
		builds++
		if got != log {
			t.Error("reset lost its logger")
		}
		return hostnode.Func("remove-host-helpers", func(ctx context.Context) error {
			t.Fatal("reset task executed while inspecting its recipe")
			return nil
		})
	}}}
	for range 2 {
		name := p.ResetNode(log).Name()
		last := -1
		for _, step := range []string{"remove-node-problem-detector.service", "remove-none-cni-state", "remove-host-helpers", "remove-static-routes.service", "remove-check-route-overlap.service", "remove-host-npd-config"} {
			index := strings.Index(name, step)
			if index < 0 || index <= last {
				t.Fatalf("reset recipe lost step ordering at %s: %s", step, name)
			}
			last = index
		}
	}
	if builds != 2 {
		t.Fatalf("host helper cleanup built %d times, want 2", builds)
	}
}

func TestNoneSetupHostRecipe(t *testing.T) {
	t.Parallel()
	name := (None{}).SetupHost(&config.Config{}, slog.New(slog.DiscardHandler), false).Name()
	for _, required := range []string{"install-packages", "load-kernel-modules", "configure-os", "configure-nftables", "disable-docker", "disable-swap", "remove-apt-hardening"} {
		if !strings.Contains(name, required) {
			t.Errorf("host recipe missing %s: %s", required, name)
		}
	}
	packages := strings.Index(name, "install-packages")
	modules := strings.Index(name, "load-kernel-modules")
	configure := strings.Index(name, "configure-os")
	if packages >= modules || modules >= configure {
		t.Fatalf("host preparation order changed: %s", name)
	}
	if strings.Contains(name, "harden-apt") || strings.Contains(name, "install-systemd-container") {
		t.Fatalf("none selected nspawn host preparation: %s", name)
	}
}

func TestSeedNoneState(t *testing.T) {
	t.Parallel()
	cfg := &agentconfig.AgentConfig{Isolation: "none"}
	state := &State{Isolation: isolation.ModeNSpawn, ActiveMachine: "kube2", AppliedSettingsVersion: "42", PreviousSettingsVersion: "41"}
	want := &State{Isolation: isolation.ModeNone, NodeConfig: cfg, AppliedSettingsVersion: "42", PreviousSettingsVersion: "41"}
	for range 2 {
		SeedNoneState(state, cfg)
		if !reflect.DeepEqual(state, want) || state.NodeConfig != cfg {
			t.Fatalf("SeedNoneState = %+v, want %+v", state, want)
		}
	}
}

type noneTestStateStore struct {
	applied *State
	err     error
}

func (s *noneTestStateStore) Load(ctx context.Context) (*State, error) {
	return s.applied, s.err
}

func (s *noneTestStateStore) Save(ctx context.Context, state *State) error {
	s.applied = state
	return nil
}

func (s *noneTestStateStore) Delete(ctx context.Context) error {
	s.applied = nil
	return nil
}

type noneTestOperator struct{ Operator }

func TestNoneNewOperator(t *testing.T) {
	t.Parallel()
	failure := errors.New("operator construction failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"}, {name: "failure", err: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeNone}}
			store, want := &noneTestStateStore{}, &noneTestOperator{}
			calls := 0
			p := None{OperatorFactory: func(ctx context.Context, gotCfg *config.Config, gotStore StateStore, worker NodeProvider) (Operator, error) {
				calls++
				if ctx != t.Context() || gotCfg != cfg || gotStore != store || worker.Mode() != isolation.ModeNone {
					t.Fatal("operator construction lost its inputs")
				}
				return want, tc.err
			}}
			for range 2 {
				got, err := p.NewOperator(t.Context(), cfg, store)
				if got != want || !errors.Is(err, tc.err) {
					t.Fatalf("NewOperator = %v, %v; want %v, %v", got, err, want, tc.err)
				}
			}
			if calls != 2 {
				t.Fatalf("operator factory called %d times, want 2", calls)
			}
		})
	}
}

func TestNoneRewriteNodeUnitsRejectsUnavailableState(t *testing.T) {
	t.Parallel()
	failure := errors.New("state unavailable")
	for _, tc := range []struct {
		name    string
		applied *State
		err     error
	}{
		{name: "load failure", err: failure},
		{name: "missing state"},
		{name: "missing runtime configuration", applied: &State{Isolation: isolation.ModeNone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := None{Host: HostServices{SyncDirs: func(dirs ...string) error {
				t.Fatal("unit rewrite continued without applied state")
				return nil
			}}}
			err := p.RewriteNodeUnits(t.Context(), slog.New(slog.DiscardHandler), &config.Config{}, &noneTestStateStore{applied: tc.applied, err: tc.err})
			if err == nil || tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("RewriteNodeUnits = %v", err)
			}
		})
	}
}

func TestNewNoneRuntimeUsesNativeRootAndExecutor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout := none.Layout{Base: filepath.Join(root, "node")}
	var calls []string
	params := noneops.Params{
		Log:       slog.New(slog.DiscardHandler),
		Config:    &agentconfig.AgentConfig{Isolation: "none"},
		GoalState: hostnodetest.GoalState(noneops.Slot, false),
		Overrides: noneops.Overrides{Layout: layout, SystemdDir: filepath.Join(root, "systemd"),
			Run: func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
				calls = append(calls, strings.Join(append([]string{name}, args...), " "))
				return "native", nil
			}},
		Hooks: hostnode.Hooks{HostRoot: root},
	}
	node := NewNoneRuntime(params)
	for range 2 {
		gs, err := node.GoalState()
		if err != nil {
			t.Fatal(err)
		}
		mapped, ok := gs.RootFS.Root().(none.Root)
		if !ok || mapped.HostRoot != root || mapped.Layout != layout || gs.NodeStart.Root() != mapped {
			t.Fatalf("SDK root was not retained: %+v", gs.RootFS.Root())
		}
		version, err := node.Version()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := mapped.Path("/usr/local/bin/kubelet"), filepath.Join(layout.VersionDir(version), "bin/kubelet"); got != want {
			t.Fatalf("kubelet path = %q, want %q", got, want)
		}
		if output, err := gs.NodeStart.Exec(t.Context(), params.Log, "systemctl", "is-active", none.UnitKubelet); err != nil || output != "native" {
			t.Fatalf("native execution = %q, %v", output, err)
		}
	}
	if !slices.Equal(calls, []string{"systemctl is-active " + none.UnitKubelet, "systemctl is-active " + none.UnitKubelet}) {
		t.Fatalf("unexpected executor calls: %v", calls)
	}
}

func TestNewNoneRuntimePropagatesResolutionFailure(t *testing.T) {
	t.Parallel()
	failure := errors.New("goal resolution failed")
	node := NewNoneRuntime(noneops.Params{Log: slog.New(slog.DiscardHandler), Hooks: hostnode.Hooks{
		HostRoot: t.TempDir(), Resolve: func(slot string) (*goalstates.MachineGoalState, error) { return nil, failure },
	}})
	for range 2 {
		if _, err := node.GoalState(); !errors.Is(err, failure) {
			t.Fatalf("GoalState = %v, want resolution failure", err)
		}
	}
}
