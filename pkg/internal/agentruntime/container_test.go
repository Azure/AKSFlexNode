package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func stageTestArchives(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task {
	return hostnode.Func("stage-test-archives", func(context.Context) error { return nil })
}

func TestContainerStartPreflightAndRuntimeInstallation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		runtime  os.FileMode
		cgroup   bool
		systemd  string
		writable error
		selinux  bool
		want     string
		install  bool
	}{
		{
			name:    "fresh host",
			cgroup:  true,
			systemd: "systemd 255",
			install: true,
			want:    "runtime installation reached",
		},
		{
			name:    "executable runtime",
			runtime: 0o700,
			cgroup:  true,
			systemd: "systemd 255",
			install: true,
			want:    "runtime installation reached",
		},
		{
			name:    "non-executable runtime",
			runtime: 0o600,
			cgroup:  true,
			systemd: "systemd 255",
			want:    "not executable",
		},
		{
			name:    "missing cgroup",
			systemd: "systemd 255",
			want:    "cgroup v2",
		},
		{
			name:    "old systemd",
			cgroup:  true,
			systemd: "systemd 253",
			want:    "too old",
		},
		{
			name:     "read-only host",
			cgroup:   true,
			systemd:  "systemd 255",
			writable: errors.New("read only"),
			want:     "read only",
		},
		{
			name:    "enforcing SELinux",
			cgroup:  true,
			systemd: "systemd 255",
			selinux: true,
			want:    "selinux label check",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			runtimePath := filepath.Join(dir, "runc")
			cgroupPath := filepath.Join(dir, "cgroup.controllers")
			if tc.runtime != 0 {
				if err := os.WriteFile(runtimePath, []byte("x"), tc.runtime); err != nil {
					t.Fatal(err)
				}
			}
			if tc.cgroup {
				if err := os.WriteFile(cgroupPath, []byte("cpu"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.selinux {
				if err := os.WriteFile(filepath.Join(dir, "enforce"), []byte("1"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			installed := false
			worker := containerRuntime{
				stageArchives: stageTestArchives,
				newNode: func(params containerops.Params) *containerops.Node {
					params.RuntimePath = runtimePath
					params.Host = &container.HostFS{
						CgroupControllers: cgroupPath,
						SELinuxEnforce:    filepath.Join(dir, "enforce"),
						SELinuxContext:    filepath.Join(dir, "context"),
					}
					params.Run = func(context.Context, *slog.Logger, string, ...string) (string, error) {
						return tc.systemd, nil
					}
					params.InstallRuntime = func(context.Context) error {
						installed = true
						return errors.New("runtime installation reached")
					}
					node := containerops.New(params)
					node.Backend().WritableCheck = func(...string) error { return tc.writable }
					return node
				},
			}
			input := Inputs{
				Config: &config.Config{},
				Node: Node{
					Slot:      "kube1",
					GoalState: hostnodetest.GoalState("kube1", false),
				},
			}
			err := worker.Start(slog.New(slog.DiscardHandler), input).Do(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if installed != tc.install {
				t.Fatalf("InstallRuntime called = %t, want %t", installed, tc.install)
			}
		})
	}
}

func TestContainerFreshStartInstallsHelperBeforeActivation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layout := container.Layout{Base: filepath.Join(dir, "containers")}
	runtimePath := filepath.Join(dir, "runc")
	cgroupPath := filepath.Join(dir, "cgroup.controllers")
	if err := os.WriteFile(cgroupPath, []byte("cpu"), 0o600); err != nil {
		t.Fatal(err)
	}
	var installed, helperSeen bool
	worker := containerRuntime{
		stageArchives: stageTestArchives,
		newNode: func(params containerops.Params) *containerops.Node {
			params.Layout = layout
			params.SystemdDir = filepath.Join(dir, "systemd")
			params.RuntimePath = runtimePath
			params.ImageArchiveDir = filepath.Join(dir, "images")
			params.Host = &container.HostFS{
				CgroupControllers: cgroupPath,
				SELinuxEnforce:    filepath.Join(dir, "absent"),
			}
			params.HostRoot = filepath.Join(dir, "host")
			params.SetupNetwork = func(context.Context) error { return nil }
			params.WaitLocalDNS = func(context.Context, *goalstates.NodeStart) error { return nil }
			params.InstallRuntime = func(context.Context) error {
				installed = true
				return os.WriteFile(runtimePath, []byte("runtime"), 0o750)
			}
			params.InstallHelper = func(context.Context) error { return nil }
			params.Unpack = func(_ context.Context, path string) error { return os.MkdirAll(path, 0o750) }
			params.ProvisionRootfs = func(context.Context) error { return nil }
			params.Run = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
				call := name + " " + strings.Join(args, " ")
				if call == "systemctl --version" {
					return "systemd 255", nil
				}
				if strings.Contains(call, "systemctl enable --now "+container.UnitName("kube1")) {
					_, err := os.Stat(filepath.Join(layout.RootfsDir("kube1"), "usr/local/bin/aks-flex-node"))
					helperSeen = err == nil
				}
				if strings.Contains(call, "systemctl show --property=SystemState") {
					return "running", nil
				}
				return "", nil
			}
			node := containerops.New(params)
			node.Backend().WritableCheck = func(...string) error { return nil }
			return node
		},
	}
	input := Inputs{
		Config: &config.Config{
			Bootstrap: config.BootstrapConfig{
				OfflineArtifacts: config.OfflineArtifactsConfig{Source: "offline"},
			},
		},
		Node: Node{
			Slot:      "kube1",
			GoalState: hostnodetest.GoalState("kube1", false),
		},
	}
	if err := worker.Start(slog.New(slog.DiscardHandler), input).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !installed || !helperSeen {
		t.Fatalf("runtime installed=%t helper present at activation=%t", installed, helperSeen)
	}
}

func TestContainerUpgradeUnpackInstallsHelper(t *testing.T) {
	t.Parallel()

	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "unpack failure"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			layout := container.Layout{Base: t.TempDir()}
			unpackErr := errors.New("unpack failed")
			node := containerops.New(containerops.Params{
				Log:       slog.New(slog.DiscardHandler),
				Slot:      "kube2",
				GoalState: hostnodetest.GoalState("kube2", false),
				Overrides: containerops.Overrides{
					Layout: layout,
					Unpack: func(_ context.Context, dir string) error {
						if fail {
							return unpackErr
						}
						return os.MkdirAll(dir, 0o750)
					},
				},
			})
			cfg := &config.Config{
				Bootstrap: config.BootstrapConfig{
					OfflineArtifacts: config.OfflineArtifactsConfig{Source: "offline"},
				},
			}
			gs, err := bindContainerArtifacts(node, slog.New(slog.DiscardHandler), cfg)
			if err != nil {
				t.Fatal(err)
			}
			err = node.Backend().Unpack(t.Context(), isolation.NodeSpec{Slot: "kube2"}, layout.RootfsDir("kube2"))
			if fail && !errors.Is(err, unpackErr) {
				t.Fatalf("unpack error = %v, want %v", err, unpackErr)
			}
			if !fail && err != nil {
				t.Fatalf("unpack: %v", err)
			}
			helper := gs.RootFS.Root().Path("/usr/local/bin/aks-flex-node")
			info, statErr := os.Stat(helper)
			if fail {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("helper installed after failed unpack: %v", statErr)
				}
			} else if statErr != nil || info.Mode().Perm() != 0o750 {
				t.Fatalf("helper missing or incorrect mode: %v", statErr)
			}
		})
	}
}

func TestContainerUpgradeInstallsHelperBeforeProvisionAndActivation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	layout := container.Layout{Base: filepath.Join(dir, "containers")}
	mountInfo := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(mountInfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stopBeforeActivation := errors.New("stop after checking helper")
	var helperSeen, oldStopped bool
	worker := containerRuntime{
		stageArchives: stageTestArchives,
		newNode: func(params containerops.Params) *containerops.Node {
			params.Layout = layout
			params.SystemdDir = filepath.Join(dir, "systemd")
			params.RuntimePath = filepath.Join(dir, "runc")
			params.MountInfoPath = mountInfo
			params.HostRoot = filepath.Join(dir, "host")
			params.Run = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
				if name == "systemctl" && len(args) > 1 && args[0] == "stop" && strings.Contains(args[1], "kube1") {
					oldStopped = true
				}
				if strings.HasSuffix(name, "runc") && len(args) > 0 && args[0] == "state" {
					return "", errors.New("not running")
				}
				return "", nil
			}
			params.InstallRuntime = func(context.Context) error { return nil }
			params.InstallHelper = func(context.Context) error { return nil }
			params.Unpack = func(_ context.Context, path string) error { return os.MkdirAll(path, 0o750) }
			params.ProvisionRootfs = func(context.Context) error {
				_, err := os.Stat(filepath.Join(layout.RootfsDir("kube2"), "usr/local/bin/aks-flex-node"))
				helperSeen = err == nil
				return stopBeforeActivation
			}
			return containerops.New(params)
		},
	}
	previous := Node{Slot: "kube1", GoalState: hostnodetest.GoalState("kube1", false)}
	input := Inputs{
		Config: &config.Config{
			Bootstrap: config.BootstrapConfig{
				OfflineArtifacts: config.OfflineArtifactsConfig{Source: "offline"},
			},
		},
		Node: Node{
			Slot:      "kube2",
			GoalState: hostnodetest.GoalState("kube2", false),
		},
		Previous: &previous,
	}
	err := worker.Start(slog.New(slog.DiscardHandler), input).Do(t.Context())
	if !errors.Is(err, stopBeforeActivation) || !helperSeen || oldStopped {
		t.Fatalf("error=%v helper installed=%t old slot stopped=%t", err, helperSeen, oldStopped)
	}
}

type containerTeardownFixture struct {
	teardown       *containerops.Teardown
	modules        string
	sharedConfig   string
	unrelated      string
	unit           string
	stops          int
	missingStops   int
	stopErr        error
	containerAlive bool
}

func newContainerTeardownFixture(t *testing.T) *containerTeardownFixture {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "host")
	layout := container.Layout{Base: filepath.Join(dir, "containers")}
	systemdDir := filepath.Join(dir, "systemd")
	f := &containerTeardownFixture{
		modules:      filepath.Join(root, noneops.ModulesLoadPath),
		sharedConfig: filepath.Join(root, "var/lib/kubelet/config.yaml"),
		unrelated:    filepath.Join(root, "unrelated"),
		unit:         filepath.Join(systemdDir, container.UnitName("kube1")),
	}
	for _, path := range []string{layout.RootfsDir("kube1"), systemdDir, filepath.Dir(f.modules), filepath.Dir(f.sharedConfig)} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{f.modules, f.sharedConfig, f.unrelated} {
		if err := os.WriteFile(path, []byte("retained"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mountInfo := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(mountInfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.teardown = &containerops.Teardown{
		Log:          slog.New(slog.DiscardHandler),
		Root:         root,
		Layout:       layout,
		SystemdDir:   systemdDir,
		RuntimePath:  filepath.Join(dir, "runtime", "runc"),
		MountInfo:    mountInfo,
		GoneTimeout:  5 * time.Millisecond,
		PollInterval: time.Millisecond,
		Run:          f.run,
	}
	return f
}

func (f *containerTeardownFixture) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	if name == "systemctl" && len(args) >= 2 && args[0] == "stop" {
		f.stops++
		if _, err := os.Lstat(filepath.Join(f.teardown.SystemdDir, args[1])); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				f.missingStops++
				return "", errors.New("unit not loaded")
			}
			return "", err
		}
		if f.stopErr == nil {
			f.containerAlive = false
		}
		return "", f.stopErr
	}
	if strings.HasSuffix(name, "runc") && len(args) > 0 && args[0] == "state" {
		if f.containerAlive {
			return `{"status":"running"}`, nil
		}
		return "", errors.New("not running")
	}
	return "", nil
}

func TestContainerTeardownPreservesSharedFilesUntilSafe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		createUnit bool
		wantStops  int
	}{
		{
			name: "interrupted prepare without unit",
		},
		{
			name:       "unit removed before mounted rootfs",
			createUnit: true,
			wantStops:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newContainerTeardownFixture(t)
			if tc.createUnit {
				if err := os.WriteFile(f.unit, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mount := "100 1 0:50 / " + f.teardown.Layout.RootfsDir("kube1") + "/volume rw - xfs /dev/sda rw\n"
			if err := os.WriteFile(f.teardown.MountInfo, []byte(mount), 0o600); err != nil {
				t.Fatal(err)
			}
			task := (&ociTeardown{Teardown: f.teardown}).Destroy()
			if err := task.Do(t.Context()); err == nil || !strings.Contains(err.Error(), "still mounted") {
				t.Fatalf("expected mount safety failure, got %v", err)
			}
			for _, path := range []string{f.modules, f.sharedConfig, f.teardown.Layout.RootfsDir("kube1")} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("unsafe teardown removed %s: %v", path, err)
				}
			}
			if _, err := os.Stat(f.unit); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("expected supervisor absent before retry: %v", err)
			}
			if err := os.WriteFile(f.teardown.MountInfo, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatalf("safe teardown retry: %v", err)
				}
			}
			if f.stops != tc.wantStops || f.missingStops != 0 {
				t.Fatalf("stop calls=%d, missing-unit stops=%d", f.stops, f.missingStops)
			}
			for _, path := range []string{f.modules, f.sharedConfig} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("teardown retained %s: %v", path, err)
				}
			}
			if _, err := os.Stat(f.unrelated); err != nil {
				t.Fatalf("removed unrelated SDK artifact: %v", err)
			}
		})
	}
}

func TestContainerTeardownStopsRunningSupervisor(t *testing.T) {
	t.Parallel()

	f := newContainerTeardownFixture(t)
	if err := os.WriteFile(f.unit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.containerAlive = true
	if err := (&ociTeardown{Teardown: f.teardown}).Destroy().Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.stops != 1 || f.missingStops != 0 || f.containerAlive {
		t.Fatalf("stops=%d missing-unit stops=%d live=%t", f.stops, f.missingStops, f.containerAlive)
	}
	for _, path := range []string{f.modules, f.sharedConfig} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("teardown retained %s: %v", path, err)
		}
	}
	if _, err := os.Stat(f.unrelated); err != nil {
		t.Fatalf("removed unrelated SDK artifact: %v", err)
	}
}

func TestContainerTeardownPropagatesStopFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		alive bool
	}{
		{
			name: "container gone",
		},
		{
			name:  "container still alive",
			alive: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newContainerTeardownFixture(t)
			if err := os.WriteFile(f.unit, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f.stopErr = errors.New("stop failed")
			f.containerAlive = tc.alive
			task := (&ociTeardown{Teardown: f.teardown}).Destroy()
			if err := task.Do(t.Context()); !errors.Is(err, f.stopErr) {
				t.Fatalf("stop error lost: %v", err)
			}
			for _, path := range []string{f.modules, f.sharedConfig, f.unrelated} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("failed stop removed %s: %v", path, err)
				}
			}
			if tc.alive {
				for _, path := range []string{f.unit, f.teardown.Layout.RootfsDir("kube1")} {
					if _, err := os.Stat(path); err != nil {
						t.Fatalf("live container lost %s: %v", path, err)
					}
				}
			}
			f.stopErr = nil
			f.containerAlive = false
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatalf("retry after stop failure: %v", err)
				}
			}
			for _, path := range []string{f.modules, f.sharedConfig} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("teardown retained %s after retry: %v", path, err)
				}
			}
			if _, err := os.Stat(f.unrelated); err != nil {
				t.Fatalf("removed unrelated SDK artifact: %v", err)
			}
		})
	}
}

func TestContainerTeardownPropagatesConfigRemovalFailure(t *testing.T) {
	t.Parallel()

	f := newContainerTeardownFixture(t)
	if err := os.Remove(f.modules); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.modules, 0o750); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(f.modules, "blocked")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := (&ociTeardown{Teardown: f.teardown}).Destroy().Do(t.Context())
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("configuration removal error = %v, want nonempty-directory failure", err)
	}
	if _, err := os.Stat(f.sharedConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SDK destruction did not precede configuration removal: %v", err)
	}
	for _, path := range []string{blocker, f.unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("configuration removal failure changed %s: %v", path, err)
		}
	}
}

func TestContainerWorkerUsesSDKRootAndExecutor(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	worker := containerRuntime{
		newNode: func(params containerops.Params) *containerops.Node {
			params.Layout = container.Layout{Base: filepath.Join(dir, "containers")}
			params.RuntimePath = filepath.Join(dir, "runc")
			params.HostRoot = filepath.Join(dir, "host")
			params.Run = func(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
				return name + " " + strings.Join(args, " "), nil
			}
			return containerops.New(params)
		},
	}
	view, err := worker.Worker(slog.New(slog.DiscardHandler), Node{
		Slot:      "kube2",
		GoalState: hostnodetest.GoalState("kube2", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Root.Slot() != "kube2" || view.Mode != isolation.ModeContainer || view.KubeletUnit != "kubelet.service" {
		t.Fatalf("unexpected worker context: %+v", view)
	}
	if got := view.Root.Path("/var/lib/kubelet/config.yaml"); got != filepath.Join(dir, "host/var/lib/kubelet/config.yaml") {
		t.Fatalf("shared kubelet path = %s", got)
	}
	layout := container.Layout{Base: filepath.Join(dir, "containers")}
	if got := view.Root.Path("/etc/containerd/config.toml"); got != filepath.Join(layout.RootfsDir("kube2"), "etc/containerd/config.toml") {
		t.Fatalf("private containerd path = %s", got)
	}
	out, err := view.Exec(t.Context(), "true")
	if err != nil || !strings.Contains(out, "exec unbounded-kube2 true") {
		t.Fatalf("worker exec = %q, %v", out, err)
	}
}

func TestContainerHelperUsesStableHostAlias(t *testing.T) {
	t.Parallel()

	worker := containerRuntime{newNode: containerops.New}
	node := worker.node(slog.New(slog.DiscardHandler), Node{Slot: "kube1"})
	if node.Backend().HelperPath != "/opt/unbounded/agent/bin/aks-flex-node" {
		t.Fatalf("helper = %s", node.Backend().HelperPath)
	}
}
