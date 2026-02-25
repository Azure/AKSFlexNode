package utils

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"
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

// FileExists checks if a file exists
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
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

// DirectoryExists checks if a directory exists
func DirectoryExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return info.IsDir()
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
