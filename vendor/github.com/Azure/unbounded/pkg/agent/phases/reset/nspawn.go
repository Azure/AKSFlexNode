// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type removeNSpawnConfig struct {
	log         *slog.Logger
	machineName string
}

// RemoveNSpawnConfig returns a task that removes the nspawn configuration file
// and the systemd service override directory for the named machine.
func RemoveNSpawnConfig(log *slog.Logger, machineName string) phases.Task {
	return &removeNSpawnConfig{log: log, machineName: machineName}
}

func (t *removeNSpawnConfig) Name() string { return "remove-nspawn-config" }

func (t *removeNSpawnConfig) Do(_ context.Context) error {
	nspawnFile := fmt.Sprintf("%s/%s.nspawn", goalstates.SystemdNSpawnDir, t.machineName)
	overrideDir := fmt.Sprintf("%s/systemd-nspawn@%s.service.d", goalstates.SystemdSystemDir, t.machineName)

	t.log.Info("removing nspawn configuration", "nspawn_file", nspawnFile, "override_dir", overrideDir)

	removeFileIfExists(t.log, nspawnFile)
	removeAllIfExists(t.log, overrideDir)

	return nil
}

// CleanupMachine returns a composite task that removes all artifacts of an
// nspawn machine: its nspawn configuration and rootfs.
func CleanupMachine(log *slog.Logger, machineName string) phases.Task {
	return phases.Serial(log,
		RemoveNSpawnConfig(log, machineName),
		RemoveMachine(log, machineName),
	)
}
