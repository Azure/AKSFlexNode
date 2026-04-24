// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import "path/filepath"

// Containerd describes the containerd configuration goal state.
type Containerd struct {
	SandboxImage      string
	ContainerdBinPath string
	RuncBinaryPath    string
	CNIBinDir         string
	CNIConfDir        string
	MetricsAddress    string
	NvidiaRuntime     NvidiaRuntime
}

// ResolveContainerd returns the containerd configuration goal state.
func ResolveContainerd() Containerd {
	return Containerd{
		SandboxImage:      SandboxImage,
		ContainerdBinPath: filepath.Join("/"+BinDir, "containerd"),
		RuncBinaryPath:    filepath.Join("/"+BinDir, "runc"),
		CNIBinDir:         CNIBinDir,
		CNIConfDir:        CNIConfigDir,
		MetricsAddress:    ContainerdMetricsAddress,
		NvidiaRuntime:     resolveNvidiaRuntime(),
	}
}
