// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// gantryHostsMarker identifies the containerd hosts file the agent wrote for
// Gantry. A file without it belongs to someone else and is left alone.
const gantryHostsMarker = "# Managed by unbounded-agent for Gantry."

// Teardown removes a none node. Its tasks slot into the same positions in the
// reset order as the nspawn machine tasks they correspond to: stop, then
// config, then the node itself, with the host-global cleanup the caller owns in
// between and after. The reset runs it only on a host where Present finds the
// node's traces, so what it removes at the shared paths is what the agent wrote
// there: the bootstrap refused a host that had a kubelet or containerd of its
// own.
type Teardown struct {
	Log *slog.Logger
	// Layout, SystemdDir and Run are as in Params.
	Layout     none.Layout
	SystemdDir string
	Run        none.Runner

	// Root prefixes every host path the teardown removes outside the layout and
	// systemd directory. Empty is the real host; tests use a temporary tree.
	Root string
	// MountInfo is the mount table. Empty means /proc/self/mountinfo.
	MountInfo string
	// Proc is the procfs scanned for container shims. Empty means /proc.
	Proc string
	// CgroupRoot is the cgroup v2 mount the pods' cgroups are killed under.
	// Empty means /sys/fs/cgroup when Proc is also empty, and none otherwise.
	CgroupRoot string
}

func (t *Teardown) logger() *slog.Logger { return t.Log }

func (t *Teardown) path(p string) string { return filepath.Join(t.Root, p) }

func (t *Teardown) backend() *none.Backend {
	proc, cgroups := t.Proc, t.CgroupRoot
	if proc == "" {
		proc = "/proc"

		// The real cgroup tree only together with the real procfs, so a test
		// that points Proc at a temporary tree never signals the host's pods.
		if cgroups == "" {
			cgroups = "/sys/fs/cgroup"
		}
	}

	return &none.Backend{Log: t.logger(), Layout: t.Layout, SystemdDir: t.SystemdDir, Run: t.Run, Proc: proc, CgroupRoot: cgroups, HostRoot: t.Root}
}

// Present reports whether the host has any trace of a none node: the layout
// directory or an agent-owned unit. Reset uses it to choose the none teardown
// even after the applied config is gone.
func (t *Teardown) Present() bool {
	if _, err := os.Lstat(t.path("/etc/aks-flex-node/none-reset")); err == nil {
		return true
	}
	if _, err := os.Lstat(filepath.Dir(t.Layout.CurrentLink())); err == nil {
		return true
	}

	dir := t.SystemdDir
	if dir == "" {
		dir = none.SystemdDir
	}

	for _, u := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS, none.UnitKubeletLaunchCheck} {
		if _, err := os.Lstat(filepath.Join(dir, u)); err == nil {
			return true
		}
	}

	return false
}

// Present is the package level form of Teardown.Present for the real host.
func Present() bool { return (&Teardown{}).Present() }

// Stop returns the task that stops the node stack's units.
func (t *Teardown) Stop() phases.Task {
	return hostnode.Func("stop-none-node", func(ctx context.Context) error {
		b := t.backend()

		var errs []error

		// The units may be absent on a half installed node; Destroy tolerates
		// that, Stop must too.
		for _, u := range []string{none.UnitKubelet, none.UnitContainerd, none.UnitLocalDNS} {
			if _, err := os.Lstat(filepath.Join(unitDir(t.SystemdDir), u)); err != nil {
				continue
			}

			if _, err := b.Exec(ctx, nil, execSpec("systemctl", "stop", u)); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", u, err))
			}
		}

		return errors.Join(errs...)
	})
}

// RemoveConfig returns the task that removes the configuration the agent wrote
// at the shared host paths.
func (t *Teardown) RemoveConfig() phases.Task {
	return hostnode.Func("remove-none-config", func(context.Context) error {
		var errs []error

		// The agent's own containerd configuration, never the distro's
		// /etc/containerd/config.toml, which a none node does not write.
		files := []string{
			none.ContainerdConfigPath,
			goalstates.NvidiaRuntimeDropInPath,
			goalstates.CDISpecFile,
			goalstates.KubeletAPIServerCACertPath,
			filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSSliceUnit),
			ModulesLoadPath,
		}
		for _, f := range files {
			if err := os.Remove(t.path(f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}

		if err := t.removeGantryHosts(); err != nil {
			errs = append(errs, err)
		}

		if err := os.RemoveAll(t.path("/etc/unbounded/localdns")); err != nil {
			errs = append(errs, err)
		}

		// Directories the agent created and nothing else is expected in. Remove
		// only when empty so anything an operator or a workload put there stays.
		for _, d := range []string{filepath.Dir(none.ContainerdConfigPath), goalstates.ContainerdConfDropInDir, goalstates.CDISpecDir, goalstates.ContainerdDefaultHostsDir, goalstates.KubeletStaticPodManifestsDir, goalstates.KubeletPKIDir} {
			if err := os.Remove(t.path(d)); err != nil && !errors.Is(err, os.ErrNotExist) && !IsNotEmpty(err) {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	})
}

func (t *Teardown) removeGantryHosts() error {
	p := t.path(goalstates.ContainerdDefaultHostsPath)

	data, err := os.ReadFile(p) //nolint:gosec // fixed Gantry hosts path under the teardown root
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	if !bytes.Contains(data, []byte(gantryHostsMarker)) {
		return nil
	}

	return os.Remove(p)
}

// Destroy returns the task that removes the node: units, drop-ins and versions
// through the backend (which also ends container shims), then the node's data
// directories after unmounting whatever pods left mounted below them.
func (t *Teardown) Destroy() phases.Task {
	return hostnode.Func("remove-none-node", func(ctx context.Context) error {
		// The backend may remove its units and binaries before reporting a pod
		// reaping failure. Keep selecting the none teardown on subsequent resets
		// until Flex's final runtime-directory cleanup removes this marker.
		if err := utilio.WriteFileDurable(t.path("/etc/aks-flex-node/none-reset"), []byte("resetting\n"), 0o600); err != nil {
			return err
		}
		if err := t.backend().Destroy(ctx, none.Root{Layout: t.Layout}); err != nil {
			return err
		}
		return t.removeData(ctx)
	})
}

// dataDirs are node state with no meaning once the node is gone. In the nspawn
// mode they live in the machine rootfs and disappear with it. The CNI plugin
// directory is not one: the backend removes the links it made there and leaves
// the plugins other components installed.
var dataDirs = []string{"/var/lib/kubelet", "/var/lib/containerd", goalstates.CNIConfigDir, hostnode.CNIStateDir}

// unmountedDataDirs are the data directories pods can leave mounts below.
var unmountedDataDirs = []string{hostnode.KubeletDir, "/var/lib/containerd", hostnode.CNIStateDir}

// removeData unmounts what pods left mounted below the data directories, then
// removes them. A directory with a mount that could not be unmounted is kept
// and the failure is reported: RemoveAll would follow the remaining mount and
// delete the volume's data (a CSI, NFS or subPath volume). A mount at the
// directory itself is detached too; detaching never deletes data, and the
// removal then only touches the empty directory underneath. An unreadable
// mount table keeps every directory that could hold mounts.
func (t *Teardown) removeData(ctx context.Context) error {
	var errs []error

	failed, err := hostnode.UnmountBelowByPrefix(ctx, t.logger(), t.Run, t.MountInfo, unmountedDataDirs...)
	if err != nil {
		errs = append(errs, err)

		failed = map[string]error{}
		for _, d := range unmountedDataDirs {
			failed[d] = err
		}
	}

	for _, d := range unmountedDataDirs {
		if failed[d] != nil && err == nil {
			errs = append(errs, failed[d])
		}
	}

	for _, d := range dataDirs {
		if failed[d] != nil {
			t.logger().Warn("keeping a data directory because mounts below it could not be unmounted", "dir", d)

			continue
		}

		if err := os.RemoveAll(t.path(d)); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// IsNotEmpty reports whether err says a directory could not be removed because
// it still has entries.
func IsNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}

func unitDir(d string) string {
	if d == "" {
		return none.SystemdDir
	}
	return d
}
