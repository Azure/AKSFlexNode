// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

// RootFS defines the goal state of the machine root fs.
// This goal state produces a rootfs that is ready for running a Kubernetes node
// via systemd-nspawn from dir points to `.MachineDir`.
type RootFS struct {
	MachineDir          string
	NSpawnConfigFile    string // e.g. /etc/systemd/nspawn/node.nspawn
	ServiceOverrideFile string // e.g. /etc/systemd/system/systemd-nspawn@node.service.d/override.conf
	HostArch            string
	HostKernel          string // running kernel version from uname -r, e.g. "6.8.0-45-generic"
	Hostname            string // host hostname, written into the rootfs so the nspawn container inherits it
	ContainerdVersion   string
	RunCVersion         string
	CNIPluginVersion    string
	KubernetesVersion   string

	// OCIImage is the fully-qualified OCI image reference (e.g.
	// "ghcr.io/org/repo:tag") used to bootstrap the machine rootfs.
	// The image must use OCI media types and include a platform manifest
	// matching the host architecture.
	OCIImage string

	// Nvidia holds NVIDIA GPU state discovered on the host: device paths,
	// driver library mappings, and bind-mount specifications for the nspawn
	// container. Empty on non-GPU hosts.
	Nvidia NvidiaHost

	// HostDevicePaths lists host device node paths to be bind-mounted into
	// the nspawn container (e.g. ["/dev/kvm"]). Device nodes are discovered
	// at agent startup. Empty on hosts without any supported devices.
	HostDevicePaths []string
}
