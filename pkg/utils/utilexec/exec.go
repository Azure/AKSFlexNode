// Package utilexec provides simple command execution helpers for running
// commands on the host and inside nspawn machines with slog-based logging.
package utilexec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// OutputCmd runs the command specified by name and args, and returns the
// captured stdout as a string. Stderr is logged at Error level.
func OutputCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers pass trusted binary names

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			logger.Error(stderr.String())
		}
		return "", fmt.Errorf("%s failed: %w", name, err)
	}

	if stderr.Len() > 0 {
		logger.Debug(stderr.String())
	}

	return strings.TrimRight(stdout.String(), "\n"), nil
}

// MachineRun executes a command inside the named nspawn machine using
// systemd-run --machine=<machine> --pipe --wait and returns stdout.
func MachineRun(ctx context.Context, logger *slog.Logger, machine string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+machine, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return OutputCmd(ctx, logger, "systemd-run", runArgs...)
}
