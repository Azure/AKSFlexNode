package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	runtimeprovider "github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type nspawnProvider = runtimeprovider.NSpawn
type noneProvider = runtimeprovider.None

var newNoneRuntime = runtimeprovider.NewNoneRuntime
var resetNoneRuntime = runtimeprovider.ResetNoneRuntime

func mustNodeProvider(t *testing.T, mode isolation.Mode) NodeProvider {
	t.Helper()
	provider, err := NewNodeProvider(mode)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestBootstrapProviderAppliedStatePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		configured, applied isolation.Mode
		wantErr             bool
	}{
		{name: "legacy nspawn rebootstrap", configured: isolation.ModeNSpawn},
		{name: "nspawn rebootstrap", configured: isolation.ModeNSpawn, applied: isolation.ModeNSpawn},
		{name: "none already installed", configured: isolation.ModeNone, applied: isolation.ModeNone, wantErr: true},
		{name: "cannot adopt nspawn", configured: isolation.ModeNone, applied: isolation.ModeNSpawn, wantErr: true},
		{name: "cannot adopt none", configured: isolation.ModeNSpawn, applied: isolation.ModeNone, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Applied none is rejected before SDK preflight or any host mutation.
			p := mustNodeProvider(t, tc.configured)
			applied := &runtimeprovider.State{Isolation: tc.applied}
			if applied.Isolation == "" {
				applied.Isolation = isolation.ModeNSpawn
			}
			err := checkBootstrapProvider(t.Context(), discardLogger(), p, applied, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("bootstrap=%v", err)
			}
		})
	}
}

type resetActionProvider struct {
	NodeProvider
	resets *int
}

func (resetActionProvider) BootstrapPresent(*slog.Logger) bool { return true }
func (p resetActionProvider) ResetNode(*slog.Logger) phases.Task {
	return hostnode.Func("fake-reset", func(context.Context) error { *p.resets++; return nil })
}

func TestResetInvokesDiscoveredActions(t *testing.T) {
	t.Parallel()
	resets := 0
	provider := resetActionProvider{NodeProvider: mustNodeProvider(t, "none"), resets: &resets}
	task, err := resetNodeFromState(t.Context(), discardLogger(), &testStateStore{}, []NodeProvider{provider})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := task.Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if resets != 2 {
		t.Fatalf("reset actions=%d", resets)
	}
}

type presenceProvider struct {
	NodeProvider
	present bool
	probes  *int
}

func (p presenceProvider) BootstrapPresent(*slog.Logger) bool { *p.probes++; return p.present }

func TestResetProviderDiscovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		state   *runtimeprovider.State
		loadErr error
		present []isolation.Mode
		want    isolation.Mode
		wantErr bool
	}{
		{name: "default", want: isolation.ModeNSpawn},
		{name: "partial none", present: []isolation.Mode{isolation.ModeNone}, want: isolation.ModeNone},
		{name: "ambiguous footprints", present: []isolation.Mode{isolation.ModeNone, isolation.ModeNSpawn}, wantErr: true},
		{name: "applied state owns retry", state: &runtimeprovider.State{Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "none"}}, want: isolation.ModeNone},
		{name: "state precedes footprints", state: &runtimeprovider.State{Isolation: isolation.ModeNSpawn, ActiveMachine: "kube1"}, present: []isolation.Mode{isolation.ModeNone}, want: isolation.ModeNSpawn},
		{name: "corruption precedes footprints", loadErr: errors.New("checksum mismatch"), present: []isolation.Mode{isolation.ModeNone}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probes := 0
			var candidates []NodeProvider
			for _, mode := range tc.present {
				candidates = append(candidates, presenceProvider{NodeProvider: mustNodeProvider(t, mode), present: true, probes: &probes})
			}
			p, err := discoverResetProvider(t.Context(), &testStateStore{state: tc.state, err: tc.loadErr}, discardLogger(), candidates)
			if (err != nil) != tc.wantErr {
				t.Fatalf("discover=%v, %v", p, err)
			}
			if !tc.wantErr && p.Mode() != tc.want {
				t.Fatalf("mode=%s, want %s", p.Mode(), tc.want)
			}
			if (tc.state != nil || tc.loadErr != nil) && probes != 0 {
				t.Fatal("inspected footprints despite authoritative applied state")
			}
		})
	}
}

type recordingProvider struct {
	NodeProvider
	calls     *[]string
	guards    int
	failGuard int
}

func (p *recordingProvider) ValidateAgentActivation(context.Context) error {
	*p.calls = append(*p.calls, "guard")
	p.guards++
	if p.guards == p.failGuard {
		return errors.New("activation fenced")
	}
	return nil
}
func (p *recordingProvider) ValidateActiveSlot(string) error {
	*p.calls = append(*p.calls, "slot")
	return nil
}
func (p *recordingProvider) ValidateAgent(string, string) error {
	*p.calls = append(*p.calls, "validate")
	return nil
}
func (p *recordingProvider) SynchronizeAgent(string, string) error {
	*p.calls = append(*p.calls, "sync")
	return nil
}
func (p *recordingProvider) RollbackAgent(string, string) error {
	*p.calls = append(*p.calls, "rollback")
	return nil
}

type recordingStateStore struct {
	stateStore
	calls *[]string
}

func (s recordingStateStore) Load(ctx context.Context) (*runtimeprovider.State, error) {
	*s.calls = append(*s.calls, "load")
	return s.stateStore.Load(ctx)
}

func TestDirectActivationGuardsStateLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fail int
		want []string
	}{
		{name: "allowed", want: []string{"guard", "load", "guard"}},
		{name: "already fenced", fail: 1, want: []string{"guard"}},
		{name: "fenced during read", fail: 2, want: []string{"guard", "load", "guard"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			p := &recordingProvider{NodeProvider: mustNodeProvider(t, "nspawn"), calls: &calls, failGuard: tc.fail}
			service := &flexDaemonActivationService{provider: p, state: recordingStateStore{stateStore: &testStateStore{}, calls: &calls}}
			_, err := service.loadActivationState(t.Context())
			if (err != nil) != (tc.fail != 0) || !slices.Equal(calls, tc.want) {
				t.Fatalf("calls=%v, error=%v", calls, err)
			}
		})
	}
}

type recordingStateLoader struct{ calls *[]string }

func (s recordingStateLoader) LoadState(context.Context) (*runtimeprovider.State, error) {
	*s.calls = append(*s.calls, "load")
	return &runtimeprovider.State{Isolation: isolation.ModeNSpawn, ActiveMachine: "kube1"}, nil
}

func TestActivationDiscoversPendingRuntimeWithoutAppliedState(t *testing.T) {
	t.Parallel()
	probes := 0
	var calls []string
	pending := &recordingProvider{
		NodeProvider: presenceProvider{NodeProvider: mustNodeProvider(t, "none"), present: true, probes: &probes},
		calls:        &calls, failGuard: 1,
	}
	selected, err := discoverNodeProvider(nil, discardLogger(), []NodeProvider{pending})
	if err != nil || selected != pending {
		t.Fatalf("discovered=%v, error=%v", selected, err)
	}
	service := &flexDaemonActivationService{provider: selected, state: recordingStateStore{stateStore: &testStateStore{}, calls: &calls}}
	if _, err := service.loadActivationState(t.Context()); err == nil {
		t.Fatal("partial bootstrap did not fence activation")
	}
	if !slices.Equal(calls, []string{"guard"}) || probes != 1 {
		t.Fatalf("calls=%v, probes=%d", calls, probes)
	}
}

func TestAgentActivationGuardsStateLoad(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fail int
		want []string
	}{
		{name: "allowed", want: []string{"guard", "load", "guard"}},
		{name: "already fenced", fail: 1, want: []string{"guard"}},
		{name: "fenced during read", fail: 2, want: []string{"guard", "load", "guard"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			p := &recordingProvider{NodeProvider: mustNodeProvider(t, "nspawn"), calls: &calls, failGuard: tc.fail}
			_, err := loadAgentActivationState(t.Context(), recordingStateLoader{calls: &calls}, p)
			if (err != nil) != (tc.fail != 0) || !slices.Equal(calls, tc.want) {
				t.Fatalf("calls=%v, error=%v", calls, err)
			}
		})
	}
}

func TestAgentUpgradeUsesSelectedActions(t *testing.T) {
	t.Parallel()
	paths := testAgentUpgradePaths(t)
	var calls []string
	p := &recordingProvider{NodeProvider: mustNodeProvider(t, "nspawn"), calls: &calls}
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := recordPendingAgentUpgrade(t.Context(), signals, recordingStateLoader{calls: &calls}, "upgrade", "daemon", p); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"guard", "load", "guard", "slot"}) {
		t.Fatalf("pending actions=%v", calls)
	}
	for _, path := range []string{paths.BluePath, paths.GreenPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("agent"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.GreenPath, paths.LastGoodPath); err != nil {
		t.Fatal(err)
	}
	signal := &agentUpgradeSignal{CandidatePath: paths.BluePath, ActiveMachine: "kube1", SwitchCommitted: true}
	calls = nil
	if err := validateStartedAgentUpgrade(paths, signal, p); err != nil {
		t.Fatal(err)
	}
	if err := rollbackAgentUpgradeFiles(paths, signal, p); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"guard", "validate", "rollback"}) {
		t.Fatalf("handoff actions=%v", calls)
	}
}

func TestDirectActivationUsesSelectedActions(t *testing.T) {
	t.Parallel()
	var calls []string
	p := &recordingProvider{NodeProvider: mustNodeProvider(t, "nspawn"), calls: &calls}
	paths := testAgentUpgradePaths(t)
	service := &flexDaemonActivationService{
		provider: p, state: &testStateStore{state: &runtimeprovider.State{ActiveMachine: "kube1"}}, log: discardLogger(), paths: paths,
		systemdDir: t.TempDir(), recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) { return false, nil },
	}
	if _, err := service.Preflight(t.Context(), paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := service.Prepare(t.Context(), paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := service.synchronizeActiveNode(t.Context(), "candidate"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"guard", "guard", "guard", "guard", "guard", "guard", "sync"}) {
		t.Fatalf("activation actions=%v", calls)
	}
}
