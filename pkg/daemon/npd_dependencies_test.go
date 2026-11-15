package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestNSpawnNPDKernelLogPermission(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mounts []agentconfig.AdditionalHostMount
		want   bool
	}{
		{name: "no kernel mount"},
		{name: "automatic read-only mount", mounts: []agentconfig.AdditionalHostMount{{Source: "/dev/kmsg", ReadOnly: true}}, want: true},
		{name: "explicit same-path target", mounts: []agentconfig.AdditionalHostMount{{Source: "/dev/kmsg", Target: "/dev/kmsg", ReadOnly: true}}, want: true},
		{name: "different device", mounts: []agentconfig.AdditionalHostMount{{Source: "/dev/console", Target: "/dev/kmsg"}}},
		{name: "remapped device", mounts: []agentconfig.AdditionalHostMount{{Source: "/dev/kmsg", Target: "/dev/custom-kmsg"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			task := ConfigureNSpawnDependencies(slog.New(slog.DiscardHandler), &goalstates.RootFS{
				ServiceOverrideFile: filepath.Join(dir, "override.conf"), AdditionalHostMounts: tc.mounts,
			}, nil).(*configureNSpawnDependenciesTask)
			task.reload = func(context.Context, *slog.Logger) error { return nil }
			var previous string
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(filepath.Join(dir, nspawnDependenciesDropIn))
				if err != nil {
					t.Fatal(err)
				}
				content := string(data)
				if strings.Contains(content, "DeviceAllow=/dev/kmsg r\n") != tc.want {
					t.Fatalf("kernel log read permission mismatch: %s", content)
				}
				if strings.Contains(content, "DeviceAllow=/dev/kmsg rw") {
					t.Fatalf("automatic NPD permission allows device writes: %s", content)
				}
				if previous != "" && previous != content {
					t.Fatalf("repeated configuration changed drop-in: %s", content)
				}
				previous = content
			}
		})
	}
}

func TestNSpawnNPDKernelLogPermissionRemovedWhenDisabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, enabled := range []bool{true, false} {
		var mounts []agentconfig.AdditionalHostMount
		if enabled {
			mounts = []agentconfig.AdditionalHostMount{{Source: "/dev/kmsg", ReadOnly: true}}
		}
		task := ConfigureNSpawnDependencies(slog.New(slog.DiscardHandler), &goalstates.RootFS{
			ServiceOverrideFile: filepath.Join(dir, "override.conf"), AdditionalHostMounts: mounts,
		}, nil).(*configureNSpawnDependenciesTask)
		task.reload = func(context.Context, *slog.Logger) error { return nil }
		if err := task.Do(t.Context()); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, nspawnDependenciesDropIn))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "/dev/kmsg") != enabled {
			t.Fatalf("enabled=%t, drop-in=%s", enabled, data)
		}
	}
}
