// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/debootstrap"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

//go:embed assets/nspawn.conf assets/service-override.conf
var nspawnAssets embed.FS

var nspawnTemplates = template.Must(
	template.New("nspawn").ParseFS(nspawnAssets, "assets/nspawn.conf", "assets/service-override.conf"),
)

type ensureNSpawnWorkspace struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// EnsureNSpawnWorkspace returns a task that bootstraps an Ubuntu rootfs into
// the machine directory (if it is empty or missing) and writes the
// systemd-nspawn configuration files needed to run a Kubernetes node inside a
// nspawn container.
func EnsureNSpawnWorkspace(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnWorkspace{log: log, goalState: goalState}
}

func (e *ensureNSpawnWorkspace) Name() string { return "ensure-nspawn-workspace" }

func (e *ensureNSpawnWorkspace) Do(ctx context.Context) error {
	if err := e.bootstrapWorkspace(ctx); err != nil {
		return fmt.Errorf("bootstrap machine directory %s: %w", e.goalState.MachineDir, err)
	}

	if err := e.writeNSpawnConfigs(); err != nil {
		return err
	}

	return nil
}

func (e *ensureNSpawnWorkspace) bootstrapWorkspace(ctx context.Context) error {
	var bootstrapTask phases.Task

	if image := e.goalState.OCIImage; image != "" {
		bootstrapTask = oci.DownloadRootFS(e.log, e.goalState.MachineDir, e.goalState.HostArch, image)
	} else {
		bootstrapTask = debootstrap.Ubuntu(e.log, e.goalState.MachineDir)
	}

	return phases.ExecuteTask(ctx, e.log, bootstrapTask)
}

// nspawnTemplateData holds the data passed to the nspawn.conf and
// service-override.conf templates. Using a struct (rather than map[string]any)
// lets us attach helper methods that the templates can call directly.
type nspawnTemplateData struct {
	HostDevicePaths      []string
	NvidiaGPUDevicePaths []string
	NvidiaLibDirMounts   []goalstates.NvidiaLibDirMount
}

// writeNSpawnConfigs renders the nspawn and service-override templates with
// device and GPU data (when present) and writes them to their configured paths.
func (e *ensureNSpawnWorkspace) writeNSpawnConfigs() error {
	templateData := nspawnTemplateData{
		HostDevicePaths:      e.goalState.HostDevicePaths,
		NvidiaGPUDevicePaths: e.goalState.Nvidia.GPUDevicePaths,
		NvidiaLibDirMounts:   e.goalState.Nvidia.LibDirMounts,
	}

	if len(e.goalState.HostDevicePaths) > 0 {
		e.log.Info("host devices detected, configuring nspawn bind-mounts",
			"count", len(e.goalState.HostDevicePaths))
	}

	if len(e.goalState.Nvidia.GPUDevicePaths) > 0 {
		e.log.Info("GPU devices detected, configuring nspawn bind-mounts",
			"count", len(e.goalState.Nvidia.GPUDevicePaths))
	}

	// Render and write the .nspawn configuration file.
	nspawnBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(nspawnBuf, "nspawn.conf", templateData); err != nil {
		return fmt.Errorf("render nspawn config template: %w", err)
	}

	if err := utilio.WriteFile(e.goalState.NSpawnConfigFile, nspawnBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write nspawn config %s: %w", e.goalState.NSpawnConfigFile, err)
	}

	// Render and write the systemd service override drop-in.
	overrideBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(overrideBuf, "service-override.conf", templateData); err != nil {
		return fmt.Errorf("render service override template: %w", err)
	}

	if err := utilio.WriteFile(e.goalState.ServiceOverrideFile, overrideBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write service override %s: %w", e.goalState.ServiceOverrideFile, err)
	}

	return nil
}
