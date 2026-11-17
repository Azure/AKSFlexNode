package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

type containerProvider = provider.Container

var requireNoContainerStartupForAgentActivation = provider.RequireNoContainerStartupForAgentActivation

func containerTestProvider(t *testing.T, startup containerStartupStore) containerProvider {
	t.Helper()
	root := t.TempDir()
	return containerProvider{
		Host: hostProviderServices(), OperatorFactory: newContainerOperatorFactory(startup, filepath.Join(root, "activation.lock")),
		PlannedHostRoot: PlannedHostRoot,
		Startup:         startup, StartupPath: startup.path, ActivationLockPath: filepath.Join(root, "activation.lock"),
		BinaryPath: func(slot string) string { return filepath.Join(root, "slots", slot, "aks-flex-node") },
		Teardown: &containerops.Teardown{Root: root, Layout: container.Layout{Base: filepath.Join(root, "containers")},
			SystemdDir: filepath.Join(root, "systemd"), RuntimePath: filepath.Join(root, "runc")},
	}
}

func TestContainerProviderDiscoversStartupWithoutAppliedState(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"preparing", "starting", "finished", "corrupt", "invalid frozen state", "invalid phase", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			p := containerTestProvider(t, activationFenceJournal(t, scenario))
			if p.Teardown.Present() {
				t.Fatal("fixture has SDK footprints")
			}
			selected, err := discoverNodeProvider(nil, discardLogger(), []NodeProvider{p})
			if err != nil || selected.Mode() != isolation.ModeContainer {
				t.Fatalf("discovery=%T, %v", selected, err)
			}
			service := activationFenceService(t, containerStartupStore{path: p.StartupPath}, nil)
			service.provider = selected
			executor := &hostAgentUpgradeExecutor{provider: selected, state: &fakeNodeOperator{}, signals: agentUpgradeSignalStore{path: service.paths.SignalPath}}
			for range 2 {
				for _, check := range []func() error{
					func() error { return executor.RecordPending(t.Context(), "upgrade") },
					func() error { _, err := service.Preflight(t.Context(), service.paths.CurrentPath); return err },
					func() error { return service.Prepare(t.Context(), service.paths.CurrentPath) },
					func() error { return service.synchronizeActiveNode(t.Context(), "/unused") },
				} {
					if err := check(); err == nil || !strings.Contains(err.Error(), "container startup") {
						t.Fatalf("missing startup fence: %v", err)
					}
				}
			}
			if _, err := os.Lstat(service.paths.SignalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("activation recorded signal: %v", err)
			}
		})
	}
}

func TestContainerProviderOperatorDispatch(t *testing.T) {
	t.Parallel()
	p := mustNodeProvider(t, isolation.ModeContainer)
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeContainer, ExperimentalContainerIsolation: true}}
	op, err := p.NewOperator(context.Background(), cfg, &testStateStore{state: containerTestState("kube1", "old")})
	if err != nil {
		t.Fatal(err)
	}
	containerOp, ok := op.(*containerNodeOperator)
	if !ok || containerOp.operatorBase == nil || containerOp.provider.Mode() != isolation.ModeContainer {
		t.Fatalf("operator=%T", op)
	}
	if containerOp.startup.path != defaultContainerStartupStore().path || containerOp.activationLockPath != agentUpgradeLockPath {
		t.Fatal("factory lost provider dependencies")
	}
	if containerTestState("kube1", "old").NodeConfig.NSpawnLifecycle == nil || p.NSpawnRuntime() != nil {
		t.Fatal("lifecycle metadata must not enable nspawn execution")
	}
}
