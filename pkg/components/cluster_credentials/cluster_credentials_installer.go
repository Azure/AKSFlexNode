package cluster_credentials

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/azure"
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

// Execute downloads the AKS cluster credentials using Azure SDK
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Downloading AKS cluster credentials using Azure SDK")

	// Get Azure credentials
	cred, err := i.authProvider.UserCredential(ctx, i.config)
	if err != nil {
		return fmt.Errorf("failed to get Azure credentials: %w", err)
	}

	// Use Azure SDK to get cluster credentials
	i.logger.Infof("Fetching cluster credentials for %s in resource group %s using Azure SDK",
		i.config.Azure.TargetCluster.Name, i.config.Azure.TargetCluster.ResourceGroup)

	kubeconfigData, err := azure.GetClusterCredentials(ctx, cred, i.logger)
	if err != nil {
		return fmt.Errorf("failed to fetch cluster credentials using Azure SDK: %w", err)
	}

	if len(kubeconfigData) == 0 {
		return fmt.Errorf("received empty kubeconfig data from Azure SDK")
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

