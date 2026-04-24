// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

type NodeStart struct {
	// MachineName is the local systemd-nspawn machine name (e.g. "kube1").
	// Used by machinectl commands and nspawn service management.
	MachineName string

	// KubeMachineName is the Kubernetes Machine CR name (e.g. "agent-e2e").
	// This is the name that appears in the cluster and may differ from
	// the local nspawn machine name.
	KubeMachineName string

	MachineDir string // e.g. /var/lib/machines/node
	Containerd Containerd
	Kubelet    Kubelet

	// Nvidia holds NVIDIA GPU state discovered on the host. After the nspawn
	// boots, the setup-nvidia-libraries task uses LibMappings to create
	// symlinks inside the container's library path pointing into the
	// bind-mounted /run/host-nvidia/ directories.
	Nvidia NvidiaHost
}
