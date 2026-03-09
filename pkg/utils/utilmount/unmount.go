//go:build linux

package utilmount

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// mountInfoPath is the path to the mountinfo file. It is a variable so
// tests can override it.
var mountInfoPath = "/proc/self/mountinfo"

// UnmountBelow unmounts all mount points found beneath root (inclusive)
// in reverse-depth order so that children are unmounted before parents.
//
// This mirrors the approach taken by `kubeadm reset` before removing
// directories — kubelet creates many bind mounts, tmpfs mounts, and
// CSI volume mounts under /var/lib/kubelet that must be unmounted
// before the directory tree can be removed.
//
// Each mount point is unmounted with MNT_DETACH so that the unmount
// succeeds even if the filesystem is still busy (e.g. processes using
// deleted pods).
//
// Unmount errors are collected rather than aborting early; the first
// error is returned wrapped in context.
//
// ref: https://man7.org/linux/man-pages/man5/proc_pid_mountinfo.5.html
func UnmountBelow(root string) error {
	mounts, err := mountsBelow(root)
	if err != nil {
		return fmt.Errorf("read mount points below %s: %w", root, err)
	}

	// Sort in reverse lexicographic order so that deeper paths
	// (children) are unmounted before shallower ones (parents).
	sort.Sort(sort.Reverse(sort.StringSlice(mounts)))

	var errs []error
	for _, mp := range mounts {
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w", mp, err))
		}
	}

	if len(errs) > 0 {
		return errs[0]
	}

	return nil
}

// mountsBelow returns all mount points that are equal to or nested
// under root. It parses /proc/self/mountinfo (field 5 is the mount
// point).
//
// /proc/self/mountinfo format (space-separated):
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//	                       ^^^^^ field[4] = mount point
//
// ref: https://man7.org/linux/man-pages/man5/proc_pid_mountinfo.5.html
func mountsBelow(root string) ([]string, error) {
	f, err := os.Open(mountInfoPath) // #nosec G304 — path is controlled by package var
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	root = filepath.Clean(root)
	prefix := root + "/"

	var mounts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mp := fields[4]
		if mp == root || strings.HasPrefix(mp, prefix) {
			mounts = append(mounts, mp)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return mounts, nil
}
