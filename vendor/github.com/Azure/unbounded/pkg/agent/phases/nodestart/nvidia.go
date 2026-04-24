// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
)

type setupNVIDIA struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// SetupNVIDIA returns a task that makes NVIDIA driver libraries from the host
// accessible inside the running nspawn machine and generates a CDI specification
// describing the available GPUs.
//
// The task performs three steps in sequence:
//  1. Creates symlinks in the container's standard library path pointing into
//     the bind-mounted /run/host-nvidia/ directories, then runs ldconfig.
//  2. Ensures the CDI spec directory exists.
//  3. Runs nvidia-ctk cdi generate inside the machine to produce the CDI spec
//     at /etc/cdi/nvidia.yaml. Most hooks are disabled because they interfere
//     with the nspawn environment (e.g. mounting tmpfs over /proc paths, CUDA
//     compat hooks on NVSwitch systems). Only create-symlinks and update-ldcache
//     are retained.
//
// This task must run after StartNSpawnMachine (the machine must be booted and
// the /run/host-nvidia/ bind mounts must be active) and before StartKubelet
// (so pods can use GPUs immediately).
//
// The task is a no-op when the NVIDIA runtime is not enabled or no library
// mappings were discovered on the host.
func SetupNVIDIA(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &setupNVIDIA{log: log, goalState: goalState}
}

func (s *setupNVIDIA) Name() string { return "setup-nvidia" }

func (s *setupNVIDIA) Do(ctx context.Context) error {
	if !s.goalState.Containerd.NvidiaRuntime.Enabled || len(s.goalState.Nvidia.LibMappings) == 0 {
		s.log.Info("NVIDIA runtime not enabled or no host libraries found, skipping")
		return nil
	}

	if err := s.setupLibraries(ctx); err != nil {
		return err
	}

	if err := s.generateCDISpec(ctx); err != nil {
		return err
	}

	return nil
}

func (s *setupNVIDIA) setupLibraries(ctx context.Context) error {
	libs := s.goalState.Nvidia.LibMappings
	machine := s.goalState.MachineName

	s.log.Info("setting up NVIDIA library symlinks inside nspawn machine",
		slog.String("machine", machine),
		slog.Int("libraries", len(libs)),
	)

	// Clean stale symlinks from a previous session that may point into
	// /run/host-nvidia/ paths that no longer exist.
	if _, err := utilexec.MachineRun(ctx, s.log, machine,
		"find", s.goalState.Nvidia.ContainerLibDir, "-maxdepth", "1",
		"-lname", goalstates.NvidiaHostLibDir+"/*", "-delete",
	); err != nil {
		return fmt.Errorf("removing stale nvidia symlinks: %w", err)
	}

	// Create symlinks: <lib>.LinkPath -> <lib>.ContainerPath
	for _, lib := range libs {
		// Remove any existing file/symlink, then create the new symlink.
		// Errors from rm -f are intentionally ignored - the file may not exist.
		utilexec.MachineRun(ctx, s.log, machine, "rm", "-f", lib.LinkPath) //nolint:errcheck // rm -f is best-effort.

		if _, err := utilexec.MachineRun(ctx, s.log, machine,
			"ln", "-s", lib.ContainerPath, lib.LinkPath,
		); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", lib.LinkPath, lib.ContainerPath, err)
		}
	}

	// Update the dynamic linker cache so the libraries are discoverable.
	if _, err := utilexec.MachineRun(ctx, s.log, machine, "ldconfig"); err != nil {
		return fmt.Errorf("ldconfig failed: %w", err)
	}

	s.log.Info("NVIDIA library symlinks created and ldconfig updated",
		slog.Int("count", len(libs)),
	)

	return nil
}

func (s *setupNVIDIA) generateCDISpec(ctx context.Context) error {
	machine := s.goalState.MachineName

	// Ensure the CDI spec directory exists in the machine rootfs (host-side path).
	cdiDir := filepath.Join(s.goalState.MachineDir, goalstates.CDISpecDir)
	if err := os.MkdirAll(cdiDir, 0o755); err != nil {
		return fmt.Errorf("create CDI spec directory %s: %w", cdiDir, err)
	}

	s.log.Info("generating CDI spec inside nspawn machine",
		slog.String("machine", machine),
		slog.String("output", goalstates.CDISpecFile),
	)

	if _, err := utilexec.MachineRun(ctx, s.log, machine,
		goalstates.NvidiaCTKPath, "cdi", "generate",
		"--output", goalstates.CDISpecFile,
		"--disable-hook", "all",
		"--enable-hook", "create-symlinks",
		"--enable-hook", "update-ldcache",
	); err != nil {
		return fmt.Errorf("nvidia-ctk cdi generate in %s: %w", machine, err)
	}

	s.log.Info("CDI spec generated", slog.String("path", goalstates.CDISpecFile))

	return nil
}
