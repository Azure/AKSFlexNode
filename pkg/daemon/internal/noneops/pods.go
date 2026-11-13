// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package noneops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// Adapted from Unbounded's none/shims.go at 7e929e099f76. The backend keeps
// reaping private; repave needs it without Backend.Upgrade's automatic rollback.
func (n *Node) reapPods() error {
	if n.p.ReapPods != nil {
		return n.p.ReapPods()
	}
	var errs []error
	if n.backend.Proc != "" {
		errs = append(errs, killShims(n.backend.Proc, sigkill))
	}
	if n.backend.CgroupRoot != "" {
		errs = append(errs, killPodCgroups(n.backend.CgroupRoot, sigkill))
	}
	return errors.Join(errs...)
}

// shimComm is the process name of the per-pod containerd shim. containerd runs
// with KillMode=process so that pods survive a containerd restart, which also
// means they survive stopping the unit. Tearing the node down has to end them.
// The name is truncated to 15 characters in /proc/<pid>/comm.
const shimComm = "containerd-shim"

// killShims sends SIGKILL to every containerd shim visible under proc that
// runs in the cgroup of the agent's containerd unit. A shim stays in the cgroup
// of the containerd that started it, so a shim of any other containerd on the
// host (a distro containerd, docker's) is left alone. A process that exits
// between the scan and the signal is not an error.
func killShims(proc string, kill func(int) error) error {
	entries, err := os.ReadDir(proc)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("scan processes: %w", err)
	}

	var errs []error

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		comm, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm")) //nolint:gosec // fixed procfs root, numeric PID and basename
		if err != nil || !strings.HasPrefix(strings.TrimSpace(string(comm)), shimComm) {
			continue
		}

		if !inAgentContainerd(filepath.Join(proc, e.Name(), "cgroup")) {
			continue
		}

		if err := kill(pid); err != nil {
			errs = append(errs, fmt.Errorf("kill containerd shim %d: %w", pid, err))
		}
	}

	return errors.Join(errs...)
}

// inAgentContainerd reports whether the /proc/<pid>/cgroup file at path places
// the process in the agent's containerd unit (or below it). An unreadable file
// is not the agent's: the process is left alone.
func inAgentContainerd(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // fixed cgroup membership file below the trusted procfs root
	if err != nil {
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2: "0::/system.slice/unbounded-containerd.service".
		// cgroup v1: "N:controllers:/system.slice/unbounded-containerd.service".
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}

		for _, elem := range strings.Split(parts[2], "/") {
			if elem == none.UnitContainerd {
				return true
			}
		}
	}

	return false
}

// sigkill sends SIGKILL to pid. A process that is already gone is not an error.
func sigkill(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}

// podCgroupNames are the top-level cgroups kubelet puts every pod in: the
// systemd driver names it kubepods.slice, the cgroupfs driver kubepods.
var podCgroupNames = []string{"kubepods.slice", "kubepods"}

// killPodCgroups ends every process in the pods' cgroups under root (the
// cgroup v2 mount). SIGKILL on a containerd shim does not do it: the pod's
// processes are reparented and keep running, holding their mounts and files
// while the node's data is removed. cgroup.kill (Linux 5.14) kills the whole
// subtree at once; without it each process listed in the subtree is signaled.
// An absent cgroup is nothing to kill.
func killPodCgroups(root string, kill func(int) error) error {
	var errs []error

	for _, name := range podCgroupNames {
		dir := filepath.Join(root, name)
		if _, err := os.Stat(dir); err != nil {
			continue
		}

		err := writeExisting(filepath.Join(dir, "cgroup.kill"), "1")
		if err == nil {
			continue
		}

		if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("kill cgroup %s: %w", dir, err))

			continue
		}

		if err := killCgroupProcesses(dir, kill); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// killCgroupProcesses signals every process in dir and its descendants, for
// kernels without cgroup.kill.
func killCgroupProcesses(dir string, kill func(int) error) error {
	var errs []error

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec // cgroup.procs under the kubelet's kernel-owned cgroup tree
		if err != nil {
			errs = append(errs, err)

			return nil
		}

		for _, f := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(f)
			if err != nil {
				continue
			}

			if err := kill(pid); err != nil {
				errs = append(errs, fmt.Errorf("kill pod process %d: %w", pid, err))
			}
		}

		return nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("scan cgroup %s: %w", dir, err))
	}

	return errors.Join(errs...)
}

// writeExisting writes to a file that must already exist: a control file, never
// created by the write, so a kernel without it is reported as ErrNotExist.
func writeExisting(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // existing kernel cgroup.kill control file below the trusted cgroup root
	if err != nil {
		return err
	}

	if _, err := f.WriteString(content); err != nil {
		_ = f.Close() //nolint:errcheck // the write error is the one to report

		return err
	}

	return f.Close()
}
