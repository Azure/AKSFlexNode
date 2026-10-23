package yurthub

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStage(t *testing.T) {
	t.Parallel()
	for _, existingState := range []bool{false, true} {
		name := "fresh rootfs without Hub package"
		if existingState {
			name = "preserve worker state and direct kubelet configuration"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			preserved := map[string]string{
				"var/lib/kubelet/kubeconfig": "direct API configuration",
			}
			if existingState {
				preserved["var/lib/yurthub/cache/pod"] = "cached pod"
				preserved["var/lib/yurthub/pki/cert"] = "worker certificate"
				preserved["etc/aks-flex-node/yurthub.env"] = "existing configuration"
			}
			for path, content := range preserved {
				writeFixture(t, filepath.Join(dir, path), content)
			}

			task := Stage(dir)
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatalf("Stage: %v", err)
				}
			}
			for path, want := range preserved {
				got, err := os.ReadFile(filepath.Join(dir, path))
				if err != nil || string(got) != want {
					t.Errorf("preserved %s = %q, %v; want %q", path, got, err, want)
				}
			}
			got, err := os.ReadFile(filepath.Join(dir, servicePath))
			if err != nil || !bytes.Equal(got, serviceUnit) {
				t.Fatalf("staged service = %q, %v", got, err)
			}
			for _, path := range []string{
				"etc/systemd/system/multi-user.target.wants/aks-flex-yurthub.service",
				"etc/systemd/system/kubelet.service.d",
				installedBinary,
			} {
				if _, err := os.Lstat(filepath.Join(dir, path)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("staging unexpectedly created %s: %v", path, err)
				}
			}
			if !existingState {
				if _, err := os.Stat(filepath.Join(dir, "etc/aks-flex-node/yurthub.env")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("staging created activation configuration: %v", err)
				}
				for _, path := range []string{"var/lib/yurthub", "var/lib/yurthub/cache", "var/lib/yurthub/pki"} {
					info, err := os.Stat(filepath.Join(dir, path))
					if err != nil {
						t.Fatal(err)
					}
					if !info.IsDir() || info.Mode().Perm() != 0o700 {
						t.Errorf("state directory %s has mode %v", path, info.Mode())
					}
				}
			}
		})
	}
}

func TestStageRejectsServiceConflict(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"symlink", "directory"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, servicePath)
			writeFixture(t, path, "customer service")
			switch name {
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := Stage(dir).Do(t.Context()); err == nil {
				t.Fatal("expected service conflict")
			}
			switch name {
			case "symlink":
				got, err := os.Readlink(path)
				if err != nil || got != "/dev/null" {
					t.Fatalf("service mask overwritten: %q, %v", got, err)
				}
			case "directory":
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() {
					t.Fatalf("service directory overwritten: %v", err)
				}
			}
		})
	}
}

func TestStageUpdatesOwnedService(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, servicePath), "previous Flex service version")
	writeFixture(t, filepath.Join(dir, "etc/systemd/system/yurthub.service"), "independent service")
	if err := Stage(dir).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, servicePath))
	if err != nil || !bytes.Equal(got, serviceUnit) {
		t.Fatalf("updated service = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "etc/systemd/system/yurthub.service"))
	if err != nil || string(got) != "independent service" {
		t.Fatalf("independent service changed: %q, %v", got, err)
	}
}

func TestStageConfinesWritesToWorker(t *testing.T) {
	t.Parallel()
	for _, link := range []string{"etc", "var"} {
		t.Run(link, func(t *testing.T) {
			t.Parallel()
			dir, outside := t.TempDir(), t.TempDir()
			if err := os.Symlink(outside, filepath.Join(dir, link)); err != nil {
				t.Fatal(err)
			}
			if err := Stage(dir).Do(t.Context()); err == nil {
				t.Fatal("expected rootfs escape rejection")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("wrote outside worker: %v, %v", entries, err)
			}
		})
	}
}

func TestStageWorkerGenerationIsolation(t *testing.T) {
	t.Parallel()
	oldWorker, newWorker := t.TempDir(), t.TempDir()
	writeFixture(t, filepath.Join(oldWorker, "var/lib/yurthub/cache/old-pod"), "old pod")
	writeFixture(t, filepath.Join(oldWorker, "var/lib/yurthub/pki/old-cert"), "old certificate")
	if err := Stage(newWorker).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"var/lib/yurthub/cache", "var/lib/yurthub/pki"} {
		entries, err := os.ReadDir(filepath.Join(newWorker, path))
		if err != nil || len(entries) != 0 {
			t.Fatalf("new worker inherited %s: %v, %v", path, entries, err)
		}
	}
}

func TestStageCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Stage(dir).Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stage error = %v, want cancellation", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cancelled staging wrote files: %v, %v", entries, err)
	}
}

func TestServiceRequiresActivationConfiguration(t *testing.T) {
	t.Parallel()
	// Keep the dormant unit safe to stage on images that lack Hub and prevent
	// accidentally baking a bootstrap token or network-dependent boot helper.
	unit := string(serviceUnit)
	for _, required := range []string{
		"ConditionPathExists=/etc/aks-flex-node/yurthub.env",
		"EnvironmentFile=/etc/aks-flex-node/yurthub.env",
		"ExecStart=/usr/local/bin/aks-flex-yurthub",
		"--root-dir=/var/lib/yurthub",
		"--disk-cache-path=/var/lib/yurthub/cache",
		"UMask=0077",
	} {
		if !strings.Contains(unit, required) {
			t.Errorf("unit missing %q", required)
		}
	}
	for _, forbidden := range []string{"--join-token", "ExecStartPre=", "Requires=kubelet", "Before=kubelet", "After=kubelet"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("dormant unit contains %q", forbidden)
		}
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
