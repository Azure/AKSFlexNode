package yurthub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestWorkerFilesystemMappings(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			var view isolation.NodeRoot = isolation.DirRoot{Dir: base, SlotName: "kube1"}
			unit := "kubelet.service"
			if mode == isolation.ModeNone {
				view = none.Root{HostRoot: base, Layout: none.Layout{Base: t.TempDir()}, Version: "v1"}
				unit = none.UnitKubelet
			}
			if mode == isolation.ModeContainer {
				view = container.Root{Dir: base, HostRoot: t.TempDir(), SlotName: "kube1"}
			}
			if err := os.MkdirAll(view.Path("/usr/local/bin"), 0o750); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, view.Path("/"+directKubeconfig), directFixture)
			root, err := openWorkerRoot(view, unit)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close() //nolint:errcheck // directory handle
			binary := "/" + installedBinary
			if mode == isolation.ModeNone {
				binary = view.Path(binary)
			}
			if err := root.WriteFile(installedBinary, []byte("verified hub binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := stageService(root, binary); err != nil {
				t.Fatal(err)
			}
			if err := configureRouting(view, unit); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(view.Path("/" + servicePath))
			if err != nil || !strings.Contains(string(data), "ExecStart="+binary+" ") {
				t.Fatalf("wrong Hub executable mapping: %v", err)
			}
			if data, err := os.ReadFile(view.Path("/" + installedBinary)); err != nil || string(data) != "verified hub binary" {
				t.Fatalf("binary not installed through SDK mapping: %v", err)
			}
			dropIn := "/etc/systemd/system/" + unit + ".d/99-aks-flex-yurthub.conf"
			if _, err := os.Stat(view.Path(dropIn)); err != nil {
				t.Fatal(err)
			}
			if mode == isolation.ModeContainer {
				if _, err := os.Stat(filepath.Join(base, directKubeconfig)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("wrote kubelet state beneath hidden rootfs directory: %v", err)
				}
			}
			for i, want := range []bool{true, false} {
				removed, err := restoreRouting(view, unit)
				if err != nil || removed != want {
					t.Fatalf("restore #%d: removed=%t, err=%v", i, removed, err)
				}
			}
		})
	}
}

func TestHostHubResetVerifiesDirectRoutingBeforeCleanup(t *testing.T) {
	t.Parallel()
	for _, verified := range []bool{false, true} {
		name := "routing verification fails"
		if verified {
			name = "direct route verified"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			fs, err := os.OpenRoot(base)
			if err != nil {
				t.Fatal(err)
			}
			defer fs.Close() //nolint:errcheck // directory handle
			root := &workerRoot{root: fs, kubeletUnit: none.UnitKubelet}
			for path, data := range map[string]string{
				servicePath: string(serviceUnit), "etc/systemd/system/" + none.UnitKubelet + ".d/99-aks-flex-yurthub.conf": string(routingUnit),
				"var/lib/yurthub/pki/cert": "Hub identity", "var/lib/yurthub/cache/secret": "cached secret", directKubeconfig: "kubelet identity",
			} {
				writeFixture(t, filepath.Join(base, path), data)
			}
			var commands []string
			run := func(_ context.Context, args ...string) (string, error) {
				command := strings.Join(args, " ")
				commands = append(commands, command)
				if strings.HasPrefix(command, "systemctl is-active") {
					return "active", nil
				}
				if strings.Contains(command, "--property=MainPID") {
					return "123", nil
				}
				if command == "cat /proc/123/cmdline" {
					route := proxyKubeconfigArg
					if verified {
						route = directKubeconfigArg
					}
					return "kubelet\x00" + route + "\x00", nil
				}
				return "", nil
			}
			for range 2 {
				err := resetHost(t.Context(), root, run)
				if (err == nil) != verified {
					t.Fatalf("reset error=%v, verified=%t", err, verified)
				}
			}
			stop := slices.Index(commands, "systemctl disable --now "+serviceName)
			if verified {
				restart := slices.Index(commands, "systemctl restart "+none.UnitKubelet)
				if stop < 0 || restart < 0 || restart >= stop {
					t.Fatalf("unsafe cleanup order: %v", commands)
				}
				if _, err := os.Stat(filepath.Join(base, "var/lib/yurthub")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Hub identity/cache retained: %v", err)
				}
			} else {
				if stop >= 0 {
					t.Fatal("stopped Hub before verifying direct API routing")
				}
				if _, err := os.Stat(filepath.Join(base, "var/lib/yurthub/pki/cert")); err != nil {
					t.Fatal("removed Hub identity after failed route restoration")
				}
			}
			data, err := os.ReadFile(filepath.Join(base, directKubeconfig))
			if err != nil || string(data) != "kubelet identity" {
				t.Fatal("reset changed kubelet identity before SDK repave")
			}
		})
	}
}
