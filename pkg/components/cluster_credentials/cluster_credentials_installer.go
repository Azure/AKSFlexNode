package cluster_credentials

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles downloading AKS cluster credentials
type Installer struct {
	config       *config.Config
	logger       *logrus.Logger
	authProvider *auth.AuthProvider
}

// NewInstaller creates a new cluster credentials Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config:       config.GetConfig(),
		logger:       logger,
		authProvider: auth.NewAuthProvider(),
	}
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "ClusterCredentialsDownloaded"
}

// Validate validates prerequisites for downloading cluster credentials
func (i *Installer) Validate(ctx context.Context) error {
	return nil
}

// Execute downloads the AKS cluster credentials using Azure CLI with Arc managed identity
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Downloading AKS cluster credentials using Azure CLI with Arc managed identity")

	// Use Azure CLI to get cluster credentials with Arc managed identity authentication
	i.logger.Infof("Fetching cluster credentials for %s in resource group %s using Azure CLI",
		i.config.Azure.TargetCluster.Name, i.config.Azure.TargetCluster.ResourceGroup)

	kubeconfigData, err := i.getClusterCredentialsWithAzureCLI()
	if err != nil {
		return fmt.Errorf("failed to fetch cluster credentials using Azure CLI: %w", err)
	}

	if len(kubeconfigData) == 0 {
		return fmt.Errorf("received empty kubeconfig data from Azure CLI")
	}

	i.logger.Infof("Successfully retrieved cluster credentials (%d bytes)", len(kubeconfigData))

	// Save kubeconfig to file with enhanced error handling
	if err := i.saveKubeconfigFile(kubeconfigData); err != nil {
		return fmt.Errorf("failed to save cluster credentials: %w", err)
	}

	i.logger.Infof("Cluster credentials downloaded and saved successfully")
	return nil
}

// IsCompleted checks if cluster credentials have been downloaded and kubeconfig is available
func (i *Installer) IsCompleted(ctx context.Context) bool {
	adminKubeconfigPath := filepath.Join(i.config.Paths.Kubernetes.ConfigDir, "admin.conf")
	return utils.FileExists(adminKubeconfigPath)
}

// saveKubeconfigFile saves the kubeconfig data to the admin.conf file
func (i *Installer) saveKubeconfigFile(kubeconfigData []byte) error {
	kubeconfigPath := filepath.Join(i.config.Paths.Kubernetes.ConfigDir, "admin.conf")

	// Ensure the kubernetes config directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", i.config.Paths.Kubernetes.ConfigDir); err != nil {
		return fmt.Errorf("failed to create kubernetes config directory: %w", err)
	}

	// Write kubeconfig file directly with proper permissions
	if err := utils.WriteFileAtomicSystem(kubeconfigPath, kubeconfigData, 0644); err != nil {
		return fmt.Errorf("failed to write kubeconfig file: %w", err)
	}

	i.logger.Info("Kubeconfig file saved successfully")
	return nil
}

// createKubeconfigWithCopyApproach simple fallback approach
func (i *Installer) createKubeconfigWithCopyApproach(kubeconfigData []byte, kubeconfigPath string) error {
	i.logger.Info("Using simple file write fallback approach...")

	// Write kubeconfig directly with proper permissions
	if err := utils.WriteFileAtomicSystem(kubeconfigPath, kubeconfigData, 0644); err != nil {
		return fmt.Errorf("failed to write kubeconfig file: %w", err)
	}

	i.logger.Info("Kubeconfig written successfully")
	return nil
}

// getClusterCredentialsWithAzureCLI downloads AKS cluster credentials using Azure CLI with Arc managed identity
func (i *Installer) getClusterCredentialsWithAzureCLI() ([]byte, error) {
	i.logger.Info("Using Azure CLI to download cluster credentials with Arc managed identity authentication")

	// Build Azure CLI command to get AKS credentials
	// Using --admin flag to get admin credentials
	// Azure CLI will automatically use Arc managed identity when available
	args := []string{
		"aks", "get-credentials",
		"--resource-group", i.config.Azure.TargetCluster.ResourceGroup,
		"--name", i.config.Azure.TargetCluster.Name,
		"--subscription", i.config.Azure.SubscriptionID,
		"--admin",                        // Get admin credentials
		"--overwrite-existing",           // Overwrite existing config
		"--file", "/tmp/kubeconfig-temp", // Write to temp file instead of default location
	}

	i.logger.Infof("Running Azure CLI command: az %v", args)

	// Execute Azure CLI command
	// Note: Azure CLI will automatically use Arc managed identity when available
	if err := utils.RunSystemCommand("az", args...); err != nil {
		return nil, fmt.Errorf("failed to download cluster credentials using Azure CLI: %w", err)
	}

	// Read the downloaded kubeconfig file
	kubeconfigData, err := os.ReadFile("/tmp/kubeconfig-temp")
	if err != nil {
		return nil, fmt.Errorf("failed to read downloaded kubeconfig file: %w", err)
	}

	// Clean up the temporary file
	if err := os.Remove("/tmp/kubeconfig-temp"); err != nil {
		i.logger.Warnf("Failed to clean up temporary kubeconfig file: %v", err)
	}

	return kubeconfigData, nil
}
