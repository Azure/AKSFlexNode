// Package utilexec provides simple command execution helpers for running
// commands on the host and inside nspawn machines with slog-based logging.
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

// Interface abstracts command creation for code that needs test injection.
type Interface interface {
	Command(name string, args ...string) *exec.Cmd
	CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd
}

type executor struct{}

// New returns the default command executor.
func New() Interface {
	return executor{}
}

func (executor) Command(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...) //nolint:gosec // callers pass trusted binary names
}

func (executor) CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...) //nolint:gosec // callers pass trusted binary names
}

// RunCmd creates a command from newCmd, appends args, streams stdout at Debug
// and stderr at Info, and waits for it to finish.
func RunCmd(ctx context.Context, logger *slog.Logger, newCmd func(context.Context) *exec.Cmd, args ...string) error {
	return RunCmdAt(ctx, logger, slog.LevelInfo, newCmd, args...)
}

// RunCmdAt is like RunCmd but streams stderr at stderrLevel.
func RunCmdAt(ctx context.Context, logger *slog.Logger, stderrLevel slog.Level, newCmd func(context.Context) *exec.Cmd, args ...string) error {
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
		streamLogs(ctx, logger, stderr, stderrLevel)
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", cmd.Path, err)
	}
	return nil
}

// OutputCmd runs the command specified by name and args, and returns the
// captured stdout as a string. Stderr is logged at Info level.
func OutputCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	return OutputCmdAt(ctx, logger, slog.LevelInfo, name, args...)
}

// OutputCmdAt is like OutputCmd but streams stderr at stderrLevel.
func OutputCmdAt(ctx context.Context, logger *slog.Logger, stderrLevel slog.Level, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // callers pass trusted binary names

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
		streamLogs(ctx, logger, stderr, stderrLevel)
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return strings.TrimRight(buf.String(), "\n"), nil
}

// MachineRun executes a command inside the named nspawn machine using
// systemd-run --machine=<machine> --pipe --wait and returns stdout.
func MachineRun(ctx context.Context, logger *slog.Logger, machine string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+machine, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return OutputCmd(ctx, logger, "systemd-run", runArgs...)
}

// Systemctl returns a command factory for systemctl.
func Systemctl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "systemctl") //nolint:gosec // fixed binary
	}
}

// Azcmagent returns a command factory for azcmagent.
func Azcmagent() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "azcmagent") //nolint:gosec // fixed binary
	}
}

// Dpkg returns a command factory for dpkg.
func Dpkg() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "dpkg") //nolint:gosec // fixed binary
	}
}

// Bash returns a command factory for bash.
func Bash() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "bash") //nolint:gosec // fixed binary
	}
}

// Curl returns a command factory for curl.
func Curl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "curl") //nolint:gosec // fixed binary
	}
}

// Wget returns a command factory for wget.
func Wget() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "wget") //nolint:gosec // fixed binary
	}
}

// Pgrep returns a command factory for pgrep.
func Pgrep() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "pgrep") //nolint:gosec // fixed binary
	}
}

// IsServiceActive checks whether a systemd service is active.
func IsServiceActive(ctx context.Context, logger *slog.Logger, serviceName string) bool {
	output, err := OutputCmdAt(ctx, logger, slog.LevelDebug, "systemctl", "is-active", serviceName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "active"
}

// ServiceExists checks whether a systemd unit file exists.
func ServiceExists(ctx context.Context, logger *slog.Logger, serviceName string) bool {
	return RunCmdAt(ctx, logger, slog.LevelDebug, Systemctl(), "list-unit-files", serviceName+".service") == nil
}

// StopService stops a systemd service.
func StopService(ctx context.Context, logger *slog.Logger, serviceName string) error {
	return RunCmd(ctx, logger, Systemctl(), "stop", serviceName)
}

// DisableService disables a systemd service.
func DisableService(ctx context.Context, logger *slog.Logger, serviceName string) error {
	return RunCmd(ctx, logger, Systemctl(), "disable", serviceName)
}

// ReloadSystemd reloads systemd daemon configuration.
func ReloadSystemd(ctx context.Context, logger *slog.Logger) error {
	return RunCmd(ctx, logger, Systemctl(), "daemon-reload")
}

// streamLogs reads lines from reader and logs each line at level.
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

// RemoveFileIfExists removes a file and ignores missing paths.
func RemoveFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// RemoveAllIfExists removes a file or directory tree and ignores missing paths.
func RemoveAllIfExists(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove all %s: %w", path, err)
	}
	return nil
}
