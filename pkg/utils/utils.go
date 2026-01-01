package utils

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// sudoCommandLists holds the command lists for sudo determination
var (
	alwaysNeedsSudo = []string{"apt", "apt-get", "dpkg", "systemctl", "mount", "umount", "modprobe", "sysctl", "azcmagent", "usermod"}
	conditionalSudo = []string{"mkdir", "cp", "chmod", "chown", "mv", "tar", "rm", "bash", "install", "ln", "cat"}
	systemPaths     = []string{"/etc/", "/usr/", "/var/", "/opt/", "/boot/", "/sys/"}
)

// requiresSudoAccess determines if a command needs sudo based on command name and arguments
func requiresSudoAccess(name string, args []string) bool {
	// Check if this command always needs sudo
	for _, sudoCmd := range alwaysNeedsSudo {
		if name == sudoCmd {
			return true
		}
	}

	// Check if this command needs sudo based on the paths involved
	for _, sudoCmd := range conditionalSudo {
		if name == sudoCmd {
			// Check if any argument involves system paths
			for _, arg := range args {
				for _, sysPath := range systemPaths {
					if strings.HasPrefix(arg, sysPath) {
						return true
					}
				}
			}
			break
		}
	}

	return false
}

// createCommand creates an exec.Cmd with appropriate sudo handling
func createCommand(name string, args []string) *exec.Cmd {
	if requiresSudoAccess(name, args) && os.Geteuid() != 0 {
		allArgs := append([]string{"-E", name}, args...)
		return exec.Command("sudo", allArgs...)
	}
	// Run directly (either doesn't need sudo or already running as root)
	return exec.Command(name, args...)
}

// RunSystemCommand executes a system command with sudo when needed for privileged operations
func RunSystemCommand(name string, args ...string) error {
	cmd := createCommand(name, args)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunCommandWithOutput executes a command and returns output with sudo when needed
func RunCommandWithOutput(name string, args ...string) (string, error) {
	cmd := createCommand(name, args)
	output, err := cmd.Output()
	return string(output), err
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

// GetServiceStatus returns the status of a systemd service (active, inactive, failed, etc.)
func GetServiceStatus(serviceName string) string {
	output, err := RunCommandWithOutput("systemctl", "is-active", serviceName)
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(output)
}

// IsServiceHealthyStopped checks if a service is stopped in a healthy way (not failed)
func IsServiceHealthyStopped(serviceName string) bool {
	// Check if service is not active
	if IsServiceActive(serviceName) {
		return false
	}

	// Check the service status to ensure it's properly stopped (not failed)
	status := GetServiceStatus(serviceName)
	return status == "inactive" || status == "unknown"
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

// EnableService enables a systemd service
func EnableService(serviceName string) error {
	return RunSystemCommand("systemctl", "enable", serviceName)
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

// IsKubeletActive checks if the kubelet service exists and is active
func IsKubeletActive() bool {
	// First check if kubelet service exists
	_, err := RunCommandWithOutput("systemctl", "cat", "kubelet")
	if err != nil {
		// Service doesn't exist
		return false
	}

	// Service exists, check if it's active
	return IsServiceActive("kubelet")
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
	cmd := createCommand("rm", []string{"-f", path})
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

// CreateTempFile creates a temporary file with given pattern and content
func CreateTempFile(pattern string, content []byte) (*os.File, error) {
	tempFile, err := os.CreateTemp("", pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary file: %w", err)
	}

	if _, err := tempFile.Write(content); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
		return nil, fmt.Errorf("failed to write to temporary file: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempFile.Name())
		return nil, fmt.Errorf("failed to close temporary file: %w", err)
	}

	// Reopen for reading
	reopened, err := os.Open(tempFile.Name())
	if err != nil {
		_ = os.Remove(tempFile.Name())
		return nil, fmt.Errorf("failed to reopen temporary file: %w", err)
	}

	return reopened, nil
}

// CleanupTempFile removes a temporary file
func CleanupTempFile(filePath string) {
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		logrus.Warnf("Failed to cleanup temporary file %s: %v", filePath, err)
	}
}

// WriteFileAtomic writes data to a file atomically using a temporary file and rename operation
// This prevents partial writes and corruption during system failures
func WriteFileAtomic(filename string, data []byte, perm os.FileMode) error {
	// Create temporary file in the same directory as the target file
	dir := filepath.Dir(filename)
	tmpFile, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(filename)+"-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}

	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath) // Clean up temp file on error
	}()

	// Write data to temporary file
	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("failed to write to temporary file: %w", err)
	}

	// Ensure data is flushed to disk
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temporary file: %w", err)
	}

	// Close the temporary file
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary file: %w", err)
	}

	// Set the correct permissions
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("failed to set file permissions: %w", err)
	}

	// Atomic rename to final location
	if err := os.Rename(tmpPath, filename); err != nil {
		return fmt.Errorf("failed to rename temporary file: %w", err)
	}

	return nil
}

// WriteFileAtomicSystem writes data to a file atomically with system-level permissions
// Uses sudo for privileged paths that require elevated permissions
func WriteFileAtomicSystem(filename string, data []byte, perm os.FileMode) error {
	// For system paths, use the temporary file approach with sudo copy/move
	if requiresSudoAccess("cp", []string{filename}) {
		// Create temp file in user-writable location
		tempFile, err := CreateTempFile("atomic-write-*.tmp", data)
		if err != nil {
			return fmt.Errorf("failed to create temporary file: %w", err)
		}
		defer CleanupTempFile(tempFile.Name())

		// Close the temp file before sudo operations
		_ = tempFile.Close()

		// Create temporary file in target directory using sudo
		tempPath := filename + ".tmp"
		if err := RunSystemCommand("cp", tempFile.Name(), tempPath); err != nil {
			return fmt.Errorf("failed to copy to temporary location: %w", err)
		}

		// Set proper permissions
		if err := RunSystemCommand("chmod", fmt.Sprintf("%o", perm), tempPath); err != nil {
			return fmt.Errorf("failed to set permissions: %w", err)
		}

		// Atomic rename
		if err := RunSystemCommand("mv", tempPath, filename); err != nil {
			return fmt.Errorf("failed to rename to final location: %w", err)
		}

		return nil
	}

	// For non-privileged paths, use regular atomic write
	return WriteFileAtomic(filename, data, perm)
}

// CreateAzureCliCommand creates an exec.Cmd for Azure CLI with sudo handling
func CreateAzureCliCommand(ctx context.Context, args ...string) *exec.Cmd {
	actualUser := os.Getenv("SUDO_USER")
	if actualUser != "" {
		// We're running under sudo, so run the az command as the original user
		cmdArgs := append([]string{"-u", actualUser, "az"}, args...)
		cmd := exec.CommandContext(ctx, "sudo", cmdArgs...)
		return cmd
	}
	// Not running under sudo, run normally
	cmd := exec.CommandContext(ctx, "az", args...)
	return cmd
}

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

// MapToKeyValuePairs converts a map to key=value pairs joined by separator
func MapToKeyValuePairs(m map[string]string, separator string) string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(pairs, separator)
}

// MapToEvictionThresholds converts a map to key<value pairs for kubelet eviction thresholds
func MapToEvictionThresholds(m map[string]string, separator string) string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, fmt.Sprintf("%s<%s", k, v))
	}
	return strings.Join(pairs, separator)
}

// DownloadFile downloads a file from URL to destination
func DownloadFile(url, destination string) error {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 10 * time.Minute,
	}

	// Make request
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download from %s: %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d for %s", resp.StatusCode, url)
	}

	// Create destination file
	out, err := os.Create(destination)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", destination, err)
	}
	defer func() {
		_ = out.Close()
	}()

	// Copy response body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write file %s: %w", destination, err)
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

// BinaryExists checks if a binary exists in PATH using 'which' command
func BinaryExists(binaryName string) bool {
	_, err := RunCommandWithOutput("which", binaryName)
	return err == nil
}

// RemovePackages removes multiple packages using apt-get with error collection
func RemovePackages(packages []string, logger *logrus.Logger) []error {
	var errors []error

	for _, pkg := range packages {
		logger.Infof("Removing package: %s", pkg)
		if err := RunSystemCommand("apt-get", "remove", "-y", pkg); err != nil {
			logger.Warnf("Failed to remove package %s: %v", pkg, err)
			errors = append(errors, fmt.Errorf("failed to remove package %s: %w", pkg, err))
		} else {
			logger.Infof("Successfully removed package: %s", pkg)
		}
	}

	return errors
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

		if err := RunSystemCommand("sudo", "rm", "-rf", dir); err != nil {
			logger.Errorf("Failed to remove directory %s: %v", dir, err)
			errors = append(errors, fmt.Errorf("failed to remove %s: %w", dir, err))
		} else {
			logger.Infof("Successfully removed directory: %s", dir)
		}
	}

	return errors
}

// CleanPackageCache runs apt package cleanup commands
func CleanPackageCache(logger *logrus.Logger) {
	logger.Info("Cleaning up package cache")

	if err := RunSystemCommand("apt-get", "autoremove", "-y"); err != nil {
		logger.Warnf("Failed to autoremove packages: %v", err)
	}

	if err := RunSystemCommand("apt-get", "autoclean"); err != nil {
		logger.Warnf("Failed to autoclean packages: %v", err)
	}

	if err := RunSystemCommand("apt-get", "update"); err != nil {
		logger.Warnf("Failed to update package cache: %v", err)
	}
}

// GetArc retrieves the system architecture in a format matching reference scripts
func GetArc() (string, error) {
	// Get architecture using same logic as reference script
	arch, err := RunCommandWithOutput("uname", "-m")
	if err != nil {
		return "", fmt.Errorf("failed to get architecture: %w", err)
	}
	arch = strings.TrimSpace(arch)

	// Map architecture names to match reference script logic
	switch arch {
	case "armv7l", "armv7":
		arch = "arm"
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}
	return arch, nil
}
