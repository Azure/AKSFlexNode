// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"sort"
)

const kvmDevicePath = "/dev/kvm"

// DiscoverHostDevicePaths probes the host for device nodes that should be
// bind-mounted into the nspawn container and returns their paths in a stable
// order so that repeated calls produce the same config output.
func DiscoverHostDevicePaths() []string {
	var paths []string

	if p := discoverKVMDevicePath(kvmDevicePath); p != "" {
		paths = append(paths, p)
	}

	sort.Strings(paths)

	return paths
}

// discoverKVMDevicePath checks whether path exists on the filesystem and
// returns it when accessible, or an empty string on any error.
func discoverKVMDevicePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		// Treat any error (including permission denied) as absent; the
		// device is not accessible to the agent, so don't expose it to the
		// container. os.ErrNotExist is the common case on non-KVM hosts.
		return ""
	}

	return path
}
