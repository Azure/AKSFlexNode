// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilexec

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// RunCmd creates a command from the given factory, appends args, streams stdout
// at Debug and stderr at Error, and waits for it to finish.
func RunCmd(ctx context.Context, logger *slog.Logger, newCmd func(context.Context) *exec.Cmd, args ...string) error {
	cmd := newCmd(ctx)
	cmd.Args = append(cmd.Args, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stdout, slog.LevelDebug)
	}()

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stderr, slog.LevelError)
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return nil
}

// OutputCmd runs the command specified by name and args, streams stdout at
// Debug and stderr at Error, and returns the captured stdout as a string.
// Unlike RunCmd it does not require a command factory - just a binary path.
func OutputCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}

	var buf bytes.Buffer

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, io.TeeReader(stdout, &buf), slog.LevelDebug)
	}()

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stderr, slog.LevelError)
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return strings.TrimRight(buf.String(), "\n"), nil
}

// MachineRun executes a command inside the named nspawn machine using
// systemd-run --machine=<machine> --pipe --wait. It streams stdout at Debug
// and stderr at Error, and returns the captured stdout.
func MachineRun(ctx context.Context, log *slog.Logger, machine string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+machine, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return OutputCmd(ctx, log, "systemd-run", runArgs...)
}

// streamLogs reads lines from reader and logs each one at the given level.
func streamLogs(ctx context.Context, logger *slog.Logger, reader io.Reader, level slog.Level) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
			logger.Log(ctx, level, scanner.Text())
		}
	}
}

// ---------------------------------------------------------------------------
// Command factories
//
// Each factory returns a func(context.Context) *exec.Cmd that creates a fresh
// exec.Cmd for the named tool. Pass the factory to RunCmd with args, or call
// it directly for output-capture patterns (e.g. cmd.Output()).
// ---------------------------------------------------------------------------

// AptGet returns a command factory for apt-get with DEBIAN_FRONTEND=noninteractive.
func AptGet() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "apt-get")

		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

		return cmd
	}
}

// Ip returns a command factory for the ip networking utility.
func Ip() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "ip")
	}
}

// Machinectl returns a command factory for machinectl.
func Machinectl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "machinectl")
	}
}

// Sysctl returns a command factory for sysctl.
func Sysctl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sysctl")
	}
}

// Systemctl returns a command factory for systemctl.
func Systemctl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "systemctl")
	}
}
