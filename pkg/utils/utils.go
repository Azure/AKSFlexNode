package utils

import (
	"context"
	"encoding/base64"
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

// IsServiceEnabled checks if a systemd service is enabled
func IsServiceEnabled(serviceName string) bool {
	output, err := RunCommandWithOutput("systemctl", "is-enabled", serviceName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "enabled"
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

// WriteFileAtomicSystem writes data to a file atomically with system-level permissions.
// Since the agent runs as root, this delegates directly to WriteFileAtomic.
func WriteFileAtomicSystem(filename string, data []byte, perm os.FileMode) error {
	return WriteFileAtomic(filename, data, perm)
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

// GetVPNInterface returns the first available VPN interface (tun0, tun1, etc.)
func GetVPNInterface() (string, error) {
	const vpnInterfacePrefix = "tun"
	const maxVPNInterfaces = 10

	// Check for tun interfaces
	for j := 0; j < maxVPNInterfaces; j++ {
		iface := fmt.Sprintf("%s%d", vpnInterfacePrefix, j)
		if _, err := os.Stat(fmt.Sprintf("/sys/class/net/%s", iface)); err == nil {
			return iface, nil
		}
	}
	return "", fmt.Errorf("no VPN interface found")
}

// GetVPNInterfaceIP returns the IP address of the given VPN interface
func GetVPNInterfaceIP(iface string) (string, error) {
	output, err := RunCommandWithOutput("ip", "addr", "show", iface)
	if err != nil {
		return "", fmt.Errorf("failed to get interface %s info: %w", iface, err)
	}

	// Parse IP address from output
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "inet ") && !strings.Contains(line, "inet6") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ip := strings.Split(fields[1], "/")[0]
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("no IP address found for interface %s", iface)
}

// ValidateAzureResourceID validates that the provided resource ID follows Azure resource ID format
// Expected format: /subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/{resourceProvider}/{resourceType}/{resourceName}
func ValidateAzureResourceID(resourceID, expectedResourceType string) error {
	if resourceID == "" {
		return fmt.Errorf("resource ID cannot be empty")
	}

	// Azure resource IDs must start with /subscriptions/
	if !strings.HasPrefix(resourceID, "/subscriptions/") {
		return fmt.Errorf("resource ID must start with '/subscriptions/', got: %s", resourceID)
	}

	// Split the resource ID into parts
	parts := strings.Split(resourceID, "/")

	// Azure resource ID should have at least 9 parts:
	// ["", "subscriptions", "{subscriptionId}", "resourceGroups", "{resourceGroupName}", "providers", "{resourceProvider}", "{resourceType}", "{resourceName}"]
	if len(parts) < 9 {
		return fmt.Errorf("resource ID has invalid format, expected at least 9 segments, got %d: %s", len(parts), resourceID)
	}

	// Validate the fixed parts of the resource ID format
	expectedSegments := map[int]string{
		1: "subscriptions",
		3: "resourceGroups",
		5: "providers",
	}

	for index, expectedValue := range expectedSegments {
		if index >= len(parts) || parts[index] != expectedValue {
			return fmt.Errorf("resource ID segment %d should be '%s', got '%s': %s", index, expectedValue, parts[index], resourceID)
		}
	}

	// Validate that required segments are not empty
	requiredSegments := map[int]string{
		2: "subscription ID",
		4: "resource group name",
		6: "resource provider",
		7: "resource type",
		8: "resource name",
	}

	for index, segmentName := range requiredSegments {
		if index >= len(parts) || strings.TrimSpace(parts[index]) == "" {
			return fmt.Errorf("resource ID %s cannot be empty: %s", segmentName, resourceID)
		}
	}

	// Validate the resource type matches expected type
	if expectedResourceType != "" && parts[7] != expectedResourceType {
		return fmt.Errorf("expected resource type '%s', got '%s': %s", expectedResourceType, parts[7], resourceID)
	}

	// Validate that it's a Microsoft.Network provider for VNet
	if expectedResourceType == "virtualNetworks" && parts[6] != "Microsoft.Network" {
		return fmt.Errorf("VNet resource must use Microsoft.Network provider, got '%s': %s", parts[6], resourceID)
	}

	return nil
}

// GetResourceGroupFromResourceID extracts the resource group name from an Azure resource ID
func GetResourceGroupFromResourceID(resourceID string) string {
	parts := strings.Split(resourceID, "/")
	for i, part := range parts {
		if strings.EqualFold(part, "resourceGroups") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// GetResourceNameFromResourceID extracts the resource name from an Azure resource ID
func GetResourceNameFromResourceID(resourceID string) string {
	parts := strings.Split(resourceID, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}

// GetSubscriptionIDFromResourceID extracts the subscription ID from an Azure resource ID
func GetSubscriptionIDFromResourceID(resourceID string) string {
	parts := strings.Split(resourceID, "/")
	for i, part := range parts {
		if strings.EqualFold(part, "subscriptions") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
