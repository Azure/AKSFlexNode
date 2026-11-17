package agentdeployment

import (
	"os"
	"path/filepath"
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
	options := agentServiceOptionsFromConfig(cfg)
	if !options.OmitMachinesTarget {
		t.Fatal("OCI service requires machines.target")
	}

	unit, err := renderAgentServiceUnit("/opt/unbounded/agent/bin/aks-flex-node", options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "machines.target") {
		t.Fatal("OCI service references machines.target")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ServiceUnitName), unit, 0o644); err != nil {
		t.Fatal(err)
	}
	installed, err := installedAgentServiceOptions(dir)
	if err != nil || installed != options {
		t.Fatalf("installed options = %+v, %v; want %+v", installed, err, options)
	}
}
