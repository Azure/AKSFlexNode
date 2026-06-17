package npd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// wantKubeconfigPath is the path the kubelet actually writes its kubeconfig to.
// The historical bug used a doubled segment ("/var/lib/kubelet/kubelet/kubeconfig"),
// which made node-problem-detector panic on startup and crash-loop forever:
//
//	panic: stat /var/lib/kubelet/kubelet/kubeconfig: no such file or directory
const wantKubeconfigPath = "/var/lib/kubelet/kubeconfig"

// TestCanonicalKubeletKubeconfigPath guards the value of the shared library
// constant so the doubled-segment typo cannot reappear from a dependency bump.
func TestCanonicalKubeletKubeconfigPath(t *testing.T) {
	t.Parallel()
	if goalstates.KubeletKubeconfigPath != wantKubeconfigPath {
		t.Fatalf("goalstates.KubeletKubeconfigPath = %q, want %q",
			goalstates.KubeletKubeconfigPath, wantKubeconfigPath)
	}
}

// TestRenderedNPDUnitUsesCanonicalKubeconfig drives Start() through the real
// service-file rendering and asserts on the kubeconfig path that ends up in the
// node-problem-detector systemd unit's ExecStart. Asserting on the rendered unit
// (the externally observable artifact) rather than internal fields keeps the
// test stable across refactors while still exercising the Start() wiring.
func TestRenderedNPDUnitUsesCanonicalKubeconfig(t *testing.T) {
	t.Parallel()
	machineDir := t.TempDir()
	nodeStart := &goalstates.NodeStart{
		MachineDir:  machineDir,
		MachineName: "kube1",
		NodeName:    "vm-e2e-token-1781659839",
		Kubelet: goalstates.Kubelet{
			APIServer: "https://example.hcp.westus.azmk8s.io:443",
		},
	}
	task, ok := Start(slog.Default(), nodeStart).(*startTask)
	if !ok {
		t.Fatalf("Start did not return *startTask")
	}
	if _, err := task.ensureServiceFile(); err != nil {
		t.Fatalf("ensureServiceFile: %v", err)
	}

	unitPath := filepath.Join(machineDir, "etc/systemd/system", systemdUnitNPD)
	data, err := os.ReadFile(unitPath) //nolint:gosec // path built from test TempDir
	if err != nil {
		t.Fatalf("read rendered unit: %v", err)
	}
	rendered := string(data)

	if !strings.Contains(rendered, "auth="+wantKubeconfigPath) {
		t.Fatalf("rendered unit missing auth=%s:\n%s", wantKubeconfigPath, rendered)
	}
	if strings.Contains(rendered, "kubelet/kubelet") {
		t.Fatalf("rendered unit contains doubled 'kubelet' segment:\n%s", rendered)
	}
	if !strings.Contains(rendered, "--hostname-override=vm-e2e-token-1781659839") {
		t.Fatalf("rendered unit missing hostname override:\n%s", rendered)
	}
}
