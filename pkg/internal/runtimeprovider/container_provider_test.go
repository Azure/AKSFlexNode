package runtimeprovider

import (
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func containerTestProvider(t *testing.T, startup ContainerStartupStore) Container {
	t.Helper()
	root := t.TempDir()
	return Container{
		Startup: startup, StartupPath: startup.Path, ActivationLockPath: filepath.Join(root, "activation.lock"),
		BinaryPath: func(slot string) string { return filepath.Join(root, "slots", slot, "aks-flex-node") },
		Teardown: &containerops.Teardown{Root: root, Layout: container.Layout{Base: filepath.Join(root, "containers")},
			SystemdDir: filepath.Join(root, "systemd"), RuntimePath: filepath.Join(root, "runc")},
	}
}

func TestContainerProviderOperatorDispatch(t *testing.T) {
	t.Parallel()
	host := HostServices{ActivationLockPath: filepath.Join(t.TempDir(), "activation.lock")}
	p, err := New(isolation.ModeContainer, host)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeContainer, ExperimentalContainerIsolation: true}}
	op, err := p.NewOperator(t.Context(), cfg, &testStateStore{state: containerTestState("kube1", "old")})
	if err != nil {
		t.Fatal(err)
	}
	containerOp, ok := op.(*containerNodeOperator)
	if !ok || containerOp.operatorBase == nil || containerOp.provider.Mode() != isolation.ModeContainer {
		t.Fatalf("operator=%T", op)
	}
	if containerOp.startup.Path != defaultContainerStartupStore().Path || containerOp.activationLockPath != host.ActivationLockPath {
		t.Fatal("factory lost provider dependencies")
	}
	if containerTestState("kube1", "old").NodeConfig.NSpawnLifecycle == nil {
		t.Fatal("lifecycle metadata missing")
	}
}
