package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
)

func TestContainerHostRecipeAndHelper(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "container", ExperimentalContainerIsolation: true}}
	provider := containerTestProvider(t, activationFenceJournal(t, "absent"))
	recipe := provider.SetupHost(cfg, discardLogger(), false).Name()
	for _, required := range []string{"disable-containerd", "disable-kubelet", "remove-apt-hardening", "load-kernel-modules"} {
		if !strings.Contains(recipe, required) {
			t.Errorf("missing %s: %s", required, recipe)
		}
	}
	if strings.Contains(recipe, "harden-apt") || strings.Contains(recipe, "install-systemd-container") {
		t.Fatalf("container recipe includes nspawn host preparation: %s", recipe)
	}
	node, err := runtimeprovider.NewContainerNode(containerops.Params{Log: discardLogger(), Slot: "kube1",
		GoalState: hostnodetest.GoalState("kube1", false),
		Overrides: containerops.Overrides{HelperPath: config.HostBinaryPath()}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if node.Backend().HelperPath != defaultAgentUpgradePaths().BinaryPath {
		t.Fatal("container helper is not the stable Flex executable")
	}
	unit, err := renderAgentServiceUnit("/agent", agentServiceOptionsFor(cfg, provider))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "machines.target") || strings.Contains(string(unit), "# Node isolation:") {
		t.Fatalf("incorrect container agent service: %s", unit)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ServiceUnitName), unit, 0o600); err != nil {
		t.Fatal(err)
	}
	paths := testAgentUpgradePaths(t)
	service := &flexDaemonActivationService{
		log:            discardLogger(),
		paths:          paths,
		state:          &testStateStore{state: containerTestState("kube1", "42")},
		provider:       provider,
		systemdDir:     dir,
		recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) { return false, nil },
	}
	if _, err := service.Preflight(t.Context(), paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(service.serviceOptions.WantedTargets, provider.WantedTargets()) {
		t.Fatalf("activation did not retain container provider targets: %+v", service.serviceOptions)
	}
}
