package agentdeployment

import (
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIServiceDoesNotRequireMachinesTarget(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Bootstrap: config.BootstrapConfig{
			Isolation: isolation.ModeContainer,
		},
	}
	targets := ServiceTargetsFromConfig(cfg, cfg.Bootstrap.Isolation)
	base := []string{"network-online.target", "systemd-udev-settle.service"}
	if !slices.Equal(targets.AfterTargets, base) || !slices.Equal(targets.WantsTargets, base) {
		t.Fatalf("OCI targets = %+v, want only network and udev dependencies", targets)
	}

	unit, err := renderAgentServiceUnit("/opt/unbounded/agent/bin/aks-flex-node", targets)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "machines.target") {
		t.Fatal("OCI service references machines.target")
	}
}
