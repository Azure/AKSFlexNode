package daemon

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestOCIHostSetupOrdersRuntimePrerequisites(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Bootstrap: config.BootstrapConfig{
			Isolation: isolation.ModeContainer,
		},
	}
	name := setupHost(cfg, slog.New(slog.DiscardHandler), false).Name()
	modules := strings.Index(name, "load-kernel-modules")
	containerd := strings.Index(name, "disable-containerd")
	kubelet := strings.Index(name, "disable-kubelet")
	apt := strings.Index(name, "remove-apt-hardening")
	if modules < 0 || containerd <= modules || kubelet <= modules || apt <= modules || strings.Contains(name, "harden-apt") {
		t.Fatalf("OCI host setup tasks = %s", name)
	}
}
