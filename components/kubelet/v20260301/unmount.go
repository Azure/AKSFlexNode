//go:build linux

package v20260301

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// procMountsPath is the path to the mounts file. It is a variable so
// tests can override it.
var procMountsPath = "/proc/mounts"

// unmountBelow unmounts all mount points found strictly beneath root
// (the root directory itself is not unmounted).
//
// This follows the upstream kubeadm implementation:
// https://github.com/kubernetes/kubernetes/blob/master/cmd/kubeadm/app/cmd/phases/reset/unmount_linux.go
//
// kubelet creates bind mounts, tmpfs mounts, and CSI volume mounts
// under /var/lib/kubelet that must be unmounted before the directory
// tree can be removed.
//
// EINVAL errors are silently ignored because they indicate a duplicate
// mount entry was already unmounted via its shared peer.
//
// Unmount errors are collected rather than aborting early; the
// aggregate error is returned.
func unmountBelow(root string) error {
	raw, err := os.ReadFile(procMountsPath) // #nosec G304 — path is controlled by package var
	if err != nil {
		return fmt.Errorf("read %s: %w", procMountsPath, err)
	}

	// Trailing "/" ensures that the root itself (e.g. /var/lib/kubelet)
	// is skipped — only children are unmounted. This matches kubeadm.
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}

	var errs []error
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, " ")
		if len(fields) < 2 {
			continue
		}
		mountPoint := fields[1]
		if !strings.HasPrefix(mountPoint, root) {
			continue
		}
		if err := syscall.Unmount(mountPoint, 0); err != nil {
			// EINVAL is expected when a duplicate mount entry was
			// already unmounted via its shared peer.
			if err == syscall.EINVAL { //nolint:errorlint // syscall errors are bare values
				continue
			}
			errs = append(errs, fmt.Errorf("unmount %s: %w", mountPoint, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("unmount directories below %s: %w", root, errs[0])
	}

	return nil
}
