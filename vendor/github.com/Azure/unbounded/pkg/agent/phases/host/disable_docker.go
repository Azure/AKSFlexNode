// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilexec"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

const (
	dockerServiceUnit = "docker.service"
	dockerSocketUnit  = "docker.socket"
	daemonConfigPath  = "/etc/docker/daemon.json"
)

type disableDocker struct {
	log *slog.Logger
}

// DisableDocker returns a task that disables the Docker service and configures
// the Docker daemon with "iptables": false. This prevents Docker from
// manipulating iptables rules, which would conflict with Kubernetes networking.
func DisableDocker(log *slog.Logger) phases.Task {
	return &disableDocker{log: log}
}

func (d *disableDocker) Name() string { return "disable-docker" }

func (d *disableDocker) Do(ctx context.Context) error {
	if err := d.ensureDockerDisabled(ctx); err != nil {
		return fmt.Errorf("disabling docker: %w", err)
	}

	if err := d.ensureDaemonConfig(); err != nil {
		return fmt.Errorf("configuring docker daemon: %w", err)
	}

	return nil
}

// ensureDockerDisabled idempotently stops, disables, and masks the docker
// service and socket units so docker cannot be started.
func (d *disableDocker) ensureDockerDisabled(ctx context.Context) error {
	systemctl := utilexec.Systemctl()

	for _, unit := range []string{dockerSocketUnit, dockerServiceUnit} {
		// Stop the unit if running (ignore errors if already stopped or not loaded).
		if err := utilexec.RunCmd(ctx, d.log, systemctl, "stop", unit); err != nil {
			d.log.DebugContext(ctx, "stopping unit (may already be stopped)", "unit", unit, "err", err)
		}

		// Mask the unit to prevent future activation.
		if err := utilexec.RunCmd(ctx, d.log, systemctl, "mask", unit); err != nil {
			return fmt.Errorf("masking %s: %w", unit, err)
		}
	}

	return nil
}

// ensureDaemonConfig ensures /etc/docker/daemon.json contains
// "iptables": false.
func (d *disableDocker) ensureDaemonConfig() error {
	return ensureDaemonConfigAt(daemonConfigPath)
}

// ensureDaemonConfigAt ensures the daemon.json at the given path contains
// "iptables": false. If the file already exists, the existing configuration is
// preserved and only the iptables key is set. If the file does not exist, a new
// one is created.
func ensureDaemonConfigAt(path string) error {
	config := map[string]any{}

	existing, err := os.ReadFile(path) //#nosec G304 -- trusted path
	switch {
	case errors.Is(err, os.ErrNotExist):
		// file does not exist, will create with defaults
	case err != nil:
		return fmt.Errorf("reading %s: %w", path, err)
	default:
		if err := json.Unmarshal(existing, &config); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
	}

	if val, ok := config["iptables"]; ok {
		if enabled, isBool := val.(bool); isBool && !enabled {
			// iptables is already set to false, nothing to do
			return nil
		}
	}

	config["iptables"] = false

	content, err := marshalDaemonConfig(config)
	if err != nil {
		return err
	}

	return utilio.WriteFile(path, content, 0o644)
}

// marshalDaemonConfig serializes a daemon config map to indented JSON with a
// trailing newline, matching the conventional format for daemon.json.
func marshalDaemonConfig(config map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("marshaling daemon config: %w", err)
	}

	data = append(data, '\n')

	return data, nil
}
