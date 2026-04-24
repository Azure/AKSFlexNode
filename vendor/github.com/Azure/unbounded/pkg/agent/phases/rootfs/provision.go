// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Provision returns a composite task that provisions a complete nspawn machine
// rootfs: bootstrapping the workspace, then downloading Kubernetes, CRI, and
// CNI binaries in parallel with OS configuration.
//
// This is the shared rootfs provisioning sequence used by both the initial
// agent start and node update flows.
func Provision(log *slog.Logger, gs *goalstates.RootFS) phases.Task {
	return phases.Serial(log,
		EnsureNSpawnWorkspace(log, gs),
		phases.Parallel(log,
			DownloadKubeBinaries(log, gs),
			DownloadCRIBinaries(log, gs),
			DownloadCNIBinaries(log, gs),
			ConfigureOS(gs),
			DisableResolved(gs),
		),
	)
}
