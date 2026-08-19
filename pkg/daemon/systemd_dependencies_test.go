package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigureNSpawnDependencies(t *testing.T) {
	t.Parallel()

	systemdDir := filepath.Join(t.TempDir(), "systemd-nspawn@kube1.service.d")
	task := ConfigureNSpawnDependencies(slog.Default(), &goalstates.RootFS{
		ServiceOverrideFile: filepath.Join(systemdDir, "override.conf"),
	}, []string{"ib_rdma_configure.service", "storage-ready.service"}).(*configureNSpawnDependenciesTask)

	reloadCount := 0
	task.reload = func(context.Context, *slog.Logger) error {
		reloadCount++
		return nil
	}
	for range 2 {
		if err := task.Do(t.Context()); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
	}
	if reloadCount != 2 {
		t.Fatalf("systemd reload count = %d, want 2", reloadCount)
	}

	data, err := os.ReadFile(filepath.Join(systemdDir, nspawnDependenciesDropIn))
	if err != nil {
		t.Fatalf("read dependency drop-in: %v", err)
	}
	content := string(data)
	for _, expected := range []string{
		"After=systemd-udev-settle.service",
		"Requires=ib_rdma_configure.service storage-ready.service",
		"After=ib_rdma_configure.service storage-ready.service",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("dependency drop-in missing %q:\n%s", expected, content)
		}
	}
}
