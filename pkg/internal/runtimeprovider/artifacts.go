package runtimeprovider

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

func StageContainerImageArchiveBindSource(log *slog.Logger, staging *goalstates.ContainerImageArchiveStaging) phases.Task {
	// TODO: Unbounded's nspawn template owns the unconditional image archive bind
	// mount; it should also ensure the bind source exists for online bootstrap.
	return rootfs.DownloadContainerImageArchives(log, staging)
}
