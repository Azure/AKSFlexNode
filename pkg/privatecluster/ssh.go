package privatecluster

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// SSHClient provides SSH operations to a remote host.
type SSHClient struct {
	config SSHConfig
	logger *logrus.Logger
}

// NewSSHClient creates a new SSHClient instance.
func NewSSHClient(config SSHConfig, logger *logrus.Logger) *SSHClient {
	return &SSHClient{
		config: config,
		logger: logger,
	}
}

// buildSSHArgs builds common SSH arguments.
func (s *SSHClient) buildSSHArgs() []string {
	return []string{
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", fmt.Sprintf("ConnectTimeout=%d", s.config.Timeout),
		"-i", s.config.KeyPath,
	}
}

// Execute runs a command on the remote host and returns the output.
func (s *SSHClient) Execute(ctx context.Context, command string) (string, error) {
	args := s.buildSSHArgs()
	args = append(args, fmt.Sprintf("%s@%s", s.config.User, s.config.Host), command)

	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- ssh with trusted internal args
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("SSH command failed: %w\nOutput: %s", err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

// ExecuteSilent runs a command on the remote host, returning only success/failure.
func (s *SSHClient) ExecuteSilent(ctx context.Context, command string) bool {
	args := s.buildSSHArgs()
	args = append(args, fmt.Sprintf("%s@%s", s.config.User, s.config.Host), command)

	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- ssh with trusted internal args
	return cmd.Run() == nil
}

// ExecuteScript runs a multi-line script on the remote host.
func (s *SSHClient) ExecuteScript(ctx context.Context, script string) error {
	args := s.buildSSHArgs()
	args = append(args, fmt.Sprintf("%s@%s", s.config.User, s.config.Host))

	cmd := exec.CommandContext(ctx, "ssh", args...) // #nosec G204 -- ssh with trusted internal args
	cmd.Stdin = strings.NewReader(script)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("SSH script execution failed: %w\nOutput: %s", err, string(output))
	}
	return nil
}

// TestConnection tests if SSH connection is ready.
func (s *SSHClient) TestConnection(ctx context.Context) bool {
	return s.ExecuteSilent(ctx, "echo ready")
}

// WaitForConnection waits for SSH connection to be ready with retries.
func (s *SSHClient) WaitForConnection(ctx context.Context, maxAttempts int, interval time.Duration) error {
	if s.TestConnection(ctx) {
		return nil
	}

	s.logger.Infof("Waiting for SSH connection to be ready...")

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		if s.TestConnection(ctx) {
			return nil
		}

		s.logger.Debugf("Waiting for SSH... (%d/%d)", attempt, maxAttempts)
	}

	return fmt.Errorf("SSH connection timeout after %d attempts", maxAttempts)
}

// ReadRemoteFile reads a file from the remote host.
func (s *SSHClient) ReadRemoteFile(ctx context.Context, path string) (string, error) {
	return s.Execute(ctx, fmt.Sprintf("sudo cat %s 2>/dev/null || echo ''", path))
}

// CommandExists checks if a command exists on the remote host.
func (s *SSHClient) CommandExists(ctx context.Context, command string) bool {
	return s.ExecuteSilent(ctx, fmt.Sprintf("command -v %s", command))
}

// GenerateSSHKey generates an SSH key pair.
func GenerateSSHKey(keyPath string) error {
	if utils.FileExists(keyPath) {
		return nil
	}

	if err := EnsureDirectory(GetRealHome() + "/.ssh"); err != nil {
		return err
	}

	cmd := exec.Command("ssh-keygen", "-t", "rsa", "-b", "4096", "-f", keyPath, "-N", "") // #nosec G204 -- fixed args
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to generate SSH key: %w\nOutput: %s", err, string(output))
	}

	return FixSSHKeyOwnership(keyPath)
}

// RemoveSSHKeys removes SSH key pair.
func RemoveSSHKeys(keyPath string) error {
	for _, path := range []string{keyPath, keyPath + ".pub"} {
		if utils.FileExists(path) {
			if err := removeFile(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeFile(path string) error {
	cmd := exec.Command("rm", "-f", path) // #nosec G204 -- fixed command with path arg
	return cmd.Run()
}
