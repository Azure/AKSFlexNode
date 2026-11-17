package daemon

import (
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestNoneHostPreparationAndService(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}}
	provider := mustNodeProvider(t, cfg.Bootstrap.Isolation)
	name := provider.SetupHost(cfg, discardLogger(), false).Name()
	for _, forbidden := range []string{"harden-apt", "disable-containerd", "disable-kubelet", "install-systemd-container"} {
		if strings.Contains(name, forbidden) {
			t.Fatalf("none setup contains %s: %s", forbidden, name)
		}
	}
	unit, err := renderAgentServiceUnit("/agent", agentServiceOptionsFor(cfg, provider))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "machines.target") {
		t.Fatal("host-native daemon depends on nspawn")
	}
}
