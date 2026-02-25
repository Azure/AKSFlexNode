package utils

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// RunSystemCommand executes a system command for privileged operations.
// The agent runs as root, so no sudo wrapping is needed.
func RunSystemCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...) // #nosec - will be addressed in the next refactor PR
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunCommandWithOutput executes a command and returns its combined output.
// The agent runs as root, so no sudo wrapping is needed.
func RunCommandWithOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...) // #nosec - will be addressed in the next refactor PR
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// RunCommandWithOutputContext executes a command with context and returns its combined output.
func RunCommandWithOutputContext(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- same pattern as RunCommandWithOutput
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// RunCommandSilentContext executes a command with context and returns only whether it succeeded.
func RunCommandSilentContext(ctx context.Context, name string, args ...string) bool {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- same pattern as RunSystemCommand
	return cmd.Run() == nil
}

// FileExists checks if a file exists
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// FileExistsAndValid checks if a file exists and is not empty (useful for binaries)
func FileExistsAndValid(path string) bool {
	stat, err := os.Stat(path)
	return err == nil && stat.Size() > 0
}

// IsServiceActive checks if a systemd service is active
func IsServiceActive(serviceName string) bool {
	output, err := RunCommandWithOutput("systemctl", "is-active", serviceName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "active"
}

// ServiceExists checks if a systemd service unit file exists
func ServiceExists(serviceName string) bool {
	err := RunSystemCommand("systemctl", "list-unit-files", serviceName+".service")
	return err == nil
}

// StopService stops a systemd service
func StopService(serviceName string) error {
	return RunSystemCommand("systemctl", "stop", serviceName)
}

// DisableService disables a systemd service
func DisableService(serviceName string) error {
	return RunSystemCommand("systemctl", "disable", serviceName)
}

// EnableAndStartService enables and starts a systemd service
func EnableAndStartService(serviceName string) error {
	return RunSystemCommand("systemctl", "enable", "--now", serviceName)
}

// RestartService restarts a systemd service
func RestartService(serviceName string) error {
	return RunSystemCommand("systemctl", "restart", serviceName)
}

// ReloadSystemd reloads systemd daemon configuration
func ReloadSystemd() error {
	return RunSystemCommand("systemctl", "daemon-reload")
}

// ignorableCleanupErrors defines patterns for errors that should be ignored during cleanup operations
var ignorableCleanupErrors = []string{
	"not loaded",
	"does not exist",
	"No such file or directory",
	"cannot remove",
	"cannot stat",
}

// shouldIgnoreCleanupError checks if an error should be ignored during cleanup operations
func shouldIgnoreCleanupError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	for _, pattern := range ignorableCleanupErrors {
		if matched, _ := regexp.MatchString(pattern, errStr); matched {
			return true
		}
	}
	return false
}

// RunCleanupCommand removes a file or directory using rm -f, ignoring "not found" errors
// This is specifically designed for cleanup operations where missing files should not be treated as errors
func RunCleanupCommand(path string) error {
	cmd := exec.Command("rm", "-f", path) // #nosec - will be addressed in the next refactor PR
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()

	// For cleanup operations, ignore common "not found" type errors
	if err != nil && !shouldIgnoreCleanupError(err) {
		// Log the error for actual failures (stderr was already shown during execution)
		fmt.Fprintf(os.Stderr, "Cleanup command failed: rm -f %s - %v\n", path, err)
		return err
	}
	return nil
}

// WaitForService waits until a systemd service is active or timeout occurs
func WaitForService(serviceName string, timeout time.Duration, logger *logrus.Logger) error {
	logger.Debugf("Waiting for service %s to be active (timeout: %v)", serviceName, timeout)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for service %s to start", serviceName)
		case <-ticker.C:
			// Check if service is active
			if err := RunSystemCommand("systemctl", "is-active", serviceName); err == nil {
				logger.Debugf("Service %s is active", serviceName)
				return nil
			}

			// Log current status for debugging
			if output, err := RunCommandWithOutput("systemctl", "status", serviceName); err == nil {
				logger.Debugf("Service %s status: %s", serviceName, output)
			}
		}
	}
}

// DirectoryExists checks if a directory exists
func DirectoryExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
}

// BinaryExists checks if a binary exists in PATH using 'which' command
func BinaryExists(binaryName string) bool {
	_, err := RunCommandWithOutput("which", binaryName)
	return err == nil
}

// RemoveFiles removes multiple files, continuing on errors and logging results
func RemoveFiles(files []string, logger *logrus.Logger) []error {
	var errors []error

	for _, file := range files {
		logger.Debugf("Removing file: %s", file)
		if err := RunSystemCommand("rm", "-f", file); err != nil {
			logger.Debugf("Failed to remove file %s: %v (may not exist)", file, err)
			errors = append(errors, fmt.Errorf("failed to remove %s: %w", file, err))
		} else {
			logger.Debugf("Removed file: %s", file)
		}
	}

	return errors
}

// RemoveDirectories removes multiple directories recursively, continuing on errors
func RemoveDirectories(directories []string, logger *logrus.Logger) []error {
	var errors []error

	for _, dir := range directories {
		logger.Infof("Removing directory: %s", dir)

		// Check if directory exists first
		if !DirectoryExists(dir) {
			logger.Debugf("Directory %s does not exist, skipping", dir)
			continue
		}

		if err := RunSystemCommand("rm", "-rf", dir); err != nil {
			logger.Errorf("Failed to remove directory %s: %v", dir, err)
			errors = append(errors, fmt.Errorf("failed to remove %s: %w", dir, err))
		} else {
			logger.Infof("Successfully removed directory: %s", dir)
		}
	}

	return errors
}

// ExtractClusterInfo extracts server URL and CA certificate data from kubeconfig
func ExtractClusterInfo(kubeconfigData []byte) (string, string, error) {
	config, err := clientcmd.Load(kubeconfigData)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// For Azure AKS admin configs, there's typically only one cluster
	if len(config.Clusters) == 0 {
		return "", "", fmt.Errorf("no clusters found in kubeconfig")
	}

	// Get the first (and usually only) cluster
	var cluster *api.Cluster
	var clusterName string
	for name, c := range config.Clusters {
		cluster = c
		clusterName = name
		break
	}

	logrus.Debugf("Using cluster: %s\n", clusterName)

	// Extract what we need
	if cluster.Server == "" {
		return "", "", fmt.Errorf("server URL is empty")
	}

	if len(cluster.CertificateAuthorityData) == 0 {
		return "", "", fmt.Errorf("CA certificate data is empty")
	}

	// CertificateAuthorityData should be base64-encoded for kubeconfig
	// The field contains raw certificate bytes, so we need to encode them
	caCertDataB64 := base64.StdEncoding.EncodeToString(cluster.CertificateAuthorityData)
	return cluster.Server, caCertDataB64, nil
}
