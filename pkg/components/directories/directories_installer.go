package directories

import (
	"context"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles creating required system directories
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new directories Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Execute creates all required system directories for Kubernetes and container runtime
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Creating required system directories")

	// Clean up any existing corrupted directories before proceeding
	if err := i.cleanupCorruptedDirectories(); err != nil {
		i.logger.Warnf("Failed to cleanup corrupted directories: %v", err)
		// Continue anyway - we'll create or fix the directories
	}

	dirs := i.getRequiredDirectories()

	// Create directories with proper error handling and validation
	for _, dir := range dirs {
		i.logger.Debugf("Creating directory: %s", dir)

		// Check if directory already exists and is valid
		if i.directoryExists(dir) && i.validateDirectoryPermissions(dir) {
			i.logger.Debugf("Directory already exists with correct permissions: %s", dir)
			continue
		}

		// Create directory with proper permissions
		if err := utils.RunSystemCommand("mkdir", "-p", dir); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}

		// Verify directory was created successfully
		if !i.directoryExists(dir) {
			return fmt.Errorf("directory creation failed for %s (directory does not exist after mkdir)", dir)
		}

		// Set proper permissions based on directory type
		if err := i.setDirectoryPermissions(dir); err != nil {
			i.logger.Warnf("Failed to set permissions on %s: %v", dir, err)
		}

		i.logger.Debugf("Successfully created directory: %s", dir)
	}

	i.logger.Info("All required directories created successfully")
	return nil
}

// IsCompleted checks if all required directories have been created
func (i *Installer) IsCompleted(ctx context.Context) bool {
	dirs := i.getRequiredDirectories()

	for _, dir := range dirs {
		if !i.directoryExists(dir) {
			return false
		}
		// Validate directory has correct permissions
		if !i.validateDirectoryPermissions(dir) {
			return false
		}
	}
	return true
}

// Validate validates prerequisites before creating directories
func (i *Installer) Validate(ctx context.Context) error {
	i.logger.Debug("Validating prerequisites for directory creation")

	// Clean up any existing corrupted directories before proceeding
	if err := i.cleanupCorruptedDirectories(); err != nil {
		i.logger.Warnf("Failed to cleanup corrupted directories: %v", err)
		// Continue anyway - we'll create or fix the directories
	}

	return nil
}

// getRequiredDirectories returns the list of directories required for the bootstrap process
func (i *Installer) getRequiredDirectories() []string {
	return []string{
		"/etc/containerd",                       // containerd config directory
		"/etc/systemd/system/kubelet.service.d", // kubelet service drop-in directory
		"/var/lib/kubelet",                      // kubelet data directory
	}
}

// directoryExists checks if a directory exists
func (i *Installer) directoryExists(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// validateDirectoryPermissions checks if a directory has the correct permissions
func (i *Installer) validateDirectoryPermissions(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		i.logger.Debugf("Failed to stat directory %s: %v", dir, err)
		return false
	}

	// Determine expected permissions based on directory type
	var expectedPerm os.FileMode
	switch dir {
	case i.config.Paths.Kubernetes.CertsDir:
		expectedPerm = DirectoryPermissionMap["certs"].Mode
	case i.config.Paths.Kubernetes.KubeletDir:
		expectedPerm = DirectoryPermissionMap["kubelet"].Mode
	default:
		expectedPerm = DirectoryPermissionMap["default"].Mode
	}

	actualPerm := info.Mode().Perm()
	if actualPerm != expectedPerm {
		i.logger.Debugf("Directory %s has incorrect permissions: got %o, expected %o", dir, actualPerm, expectedPerm)
		return false
	}

	return true
}

// setDirectoryPermissions sets appropriate permissions for a directory based on its type
func (i *Installer) setDirectoryPermissions(dir string) error {
	var perm DirectoryPermissions
	var permFound bool

	// Determine permissions based on directory type
	switch dir {
	case i.config.Paths.Kubernetes.CertsDir:
		perm, permFound = DirectoryPermissionMap["certs"]
	case i.config.Paths.Kubernetes.KubeletDir:
		perm, permFound = DirectoryPermissionMap["kubelet"]
	default:
		perm, permFound = DirectoryPermissionMap["default"]
	}

	if !permFound {
		return fmt.Errorf("no permission configuration found for directory type")
	}

	// Convert FileMode to string for chmod command
	permString := fmt.Sprintf("%o", perm.Mode)
	if err := utils.RunSystemCommand("chmod", permString, dir); err != nil {
		return fmt.Errorf("failed to set permissions %s on %s (%s): %w", permString, dir, perm.Description, err)
	}

	i.logger.Debugf("Set permissions %s on %s (%s)", permString, dir, perm.Description)
	return nil
}

// cleanupCorruptedDirectories removes any corrupted directories that may interfere with creation
func (i *Installer) cleanupCorruptedDirectories() error {
	i.logger.Debug("Checking for corrupted directories to cleanup")

	dirs := i.getRequiredDirectories()

	for _, dir := range dirs {
		if !i.directoryExists(dir) {
			continue // Directory doesn't exist, nothing to cleanup
		}

		// Check if directory is corrupted (wrong permissions, not readable, etc.)
		if !i.validateDirectoryPermissions(dir) || !i.isDirectoryUsable(dir) {
			i.logger.Debugf("Found corrupted directory, removing: %s", dir)
			if err := utils.RunSystemCommand("rm", "-rf", dir); err != nil {
				i.logger.Warnf("Failed to remove corrupted directory %s: %v", dir, err)
			}
		}
	}

	return nil
}

// isDirectoryUsable checks if a directory is readable and writable
func (i *Installer) isDirectoryUsable(dir string) bool {
	// Try to read the directory
	if _, err := os.ReadDir(dir); err != nil {
		i.logger.Debugf("Directory %s is not readable: %v", dir, err)
		return false
	}

	// Try to create a temporary file to test write permissions
	tempFile := fmt.Sprintf("%s/.test-write-%d", dir, os.Getpid())
	if err := os.WriteFile(tempFile, []byte("test"), 0644); err != nil {
		i.logger.Debugf("Directory %s is not writable: %v", dir, err)
		return false
	}

	// Clean up the test file
	_ = os.Remove(tempFile)
	return true
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "DirectoriesCreated"
}
