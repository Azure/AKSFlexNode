package kube_proxy

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles kube-proxy installation and configuration
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new kube-proxy Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the step name for the executor interface
func (i *Installer) GetName() string {
	return "KubeProxyInstaller"
}

// Execute installs and configures kube-proxy service
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Installing and configuring kube-proxy")

	// Download and install kube-proxy binary
	if err := i.downloadKubeProxyBinary(); err != nil {
		return fmt.Errorf("failed to download kube-proxy binary: %w", err)
	}

	// Create required directories
	if err := i.createRequiredDirectories(); err != nil {
		return fmt.Errorf("failed to create required directories: %w", err)
	}

	// Create kube-proxy kubeconfig
	if err := i.createKubeProxyKubeconfig(); err != nil {
		return fmt.Errorf("failed to create kube-proxy kubeconfig: %w", err)
	}

	// Create kube-proxy configuration file
	if err := i.createKubeProxyConfig(); err != nil {
		return fmt.Errorf("failed to create kube-proxy configuration: %w", err)
	}

	// Create kube-proxy systemd service
	if err := i.createKubeProxyService(); err != nil {
		return fmt.Errorf("failed to create kube-proxy service: %w", err)
	}

	// Reload systemd to pick up new service file
	if err := utils.RunSystemCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w", err)
	}

	// Enable the service (but don't start it - that's handled by the services installer)
	if err := utils.RunSystemCommand("systemctl", "enable", kubeProxyServiceName); err != nil {
		return fmt.Errorf("failed to enable kube-proxy service: %w", err)
	}

	i.logger.Info("Kube-proxy installed and configured successfully")
	return nil
}

// IsCompleted checks if kube-proxy service has been installed and configured
func (i *Installer) IsCompleted(ctx context.Context) bool {
	i.logger.Debug("Checking kube-proxy installation status")

	// Check if binary exists
	if !utils.FileExistsAndValid(kubeProxyBinaryPath) {
		i.logger.Debug("kube-proxy binary not found")
		return false
	}

	// Check if configuration files exist
	if !utils.FileExistsAndValid(kubeProxyConfigPath) {
		i.logger.Debug("kube-proxy configuration not found")
		return false
	}

	if !utils.FileExistsAndValid(kubeProxyKubeConfig) {
		i.logger.Debug("kube-proxy kubeconfig not found")
		return false
	}

	// Check if service file exists
	if !utils.FileExistsAndValid(kubeProxyServicePath) {
		i.logger.Debug("kube-proxy service file not found")
		return false
	}

	// Check if service is enabled (but not necessarily running - that's handled by services installer)
	if !utils.IsServiceEnabled(kubeProxyServiceName) {
		i.logger.Debug("kube-proxy service is not enabled")
		return false
	}

	i.logger.Debug("Kube-proxy appears to be properly installed and configured")
	return true
}

// Validate validates prerequisites for kube-proxy installation
func (i *Installer) Validate(ctx context.Context) error {
	i.logger.Debug("Validating prerequisites for kube-proxy installation")
	if !utils.FileExistsAndValid(kubelet.KubeletKubeconfigPath) {
		return fmt.Errorf("kubelet kubeconfig not found at %s - ensure kubelet installer ran first", kubelet.KubeletKubeconfigPath)
	}
	return nil
}

// createRequiredDirectories creates all necessary directories for kube-proxy
func (i *Installer) createRequiredDirectories() error {
	dirs := []string{
		kubeProxyVarDir,
		filepath.Dir(kubeProxyKubeConfig),
		kubeProxyServiceDropIn,
	}

	for _, dir := range dirs {
		if err := utils.RunSystemCommand("mkdir", "-p", dir); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}

		if err := utils.RunSystemCommand("chmod", fmt.Sprintf("%o", dirPerm), dir); err != nil {
			i.logger.Warnf("Failed to set permissions on directory %s: %v", dir, err)
		}
	}

	return nil
}

// createKubeProxyKubeconfig creates kubeconfig for kube-proxy
func (i *Installer) createKubeProxyKubeconfig() error {
	i.logger.Info("Creating kube-proxy kubeconfig")

	// For now, kube-proxy will use the same kubeconfig as kubelet
	// This ensures the same Arc authentication mechanism works for both
	// Read kubelet kubeconfig using system command (handles elevated permissions)
	kubeletConfig, err := utils.RunCommandWithOutput("cat", kubelet.KubeletKubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to read kubelet kubeconfig from %s: %w", kubelet.KubeletKubeconfigPath, err)
	}

	// Write kube-proxy kubeconfig (same as kubelet for now)
	if err := utils.WriteFileAtomicSystem(kubeProxyKubeConfig, []byte(kubeletConfig), configFilePerm); err != nil {
		return fmt.Errorf("failed to write kube-proxy kubeconfig: %w", err)
	}

	i.logger.Info("Kube-proxy kubeconfig created successfully")
	return nil
}

// createKubeProxyConfig creates the kube-proxy configuration file
func (i *Installer) createKubeProxyConfig() error {
	i.logger.Info("Creating kube-proxy configuration")

	// Determine cluster CIDR from CNI configuration or use default
	clusterCIDR := defaultClusterCIDR
	if i.config.Azure.VPNGateway != nil && i.config.Azure.VPNGateway.PodCIDR != "" {
		clusterCIDR = i.config.Azure.VPNGateway.PodCIDR
	}

	kubeProxyConfigContent := fmt.Sprintf(`apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
bindAddress: %s
clientConnection:
  kubeconfig: %s
clusterCIDR: %s
configSyncPeriod: 15m0s
conntrack:
  maxPerCore: 32768
  min: 131072
  tcpCloseWaitTimeout: 1h0m0s
  tcpEstablishedTimeout: 24h0m0s
enableProfiling: false
healthzBindAddress: %s:%d
iptables:
  masqueradeAll: false
  masqueradeBit: 14
  minSyncPeriod: 0s
  syncPeriod: 30s
ipvs:
  excludeCIDRs: null
  minSyncPeriod: 0s
  scheduler: ""
  syncPeriod: 30s
metricsBindAddress: %s:%d
mode: %s
nodePortAddresses: null
oomScoreAdj: -999
portRange: ""
udpIdleTimeout: 250ms
winkernel:
  enableDSR: false
  networkName: ""
  sourceVip: ""
`, defaultBindAddress, kubeProxyKubeConfig, clusterCIDR, defaultBindAddress, defaultHealthzPort,
		defaultBindAddress, defaultMetricsPort, defaultProxyMode)

	if err := utils.WriteFileAtomicSystem(kubeProxyConfigPath, []byte(kubeProxyConfigContent), configFilePerm); err != nil {
		return fmt.Errorf("failed to write kube-proxy configuration: %w", err)
	}

	i.logger.Info("Kube-proxy configuration created successfully")
	return nil
}

// createKubeProxyService creates the systemd service for kube-proxy
func (i *Installer) createKubeProxyService() error {
	i.logger.Info("Creating kube-proxy systemd service")

	serviceContent := fmt.Sprintf(`[Unit]
Description=Kubernetes Kube-Proxy
Documentation=https://github.com/kubernetes/kubernetes
After=network.target

[Service]
ExecStart=%s --config=%s --v=2
Restart=always
RestartSec=5
KillMode=process

[Install]
WantedBy=multi-user.target
`, kubeProxyBinaryPath, kubeProxyConfigPath)

	if err := utils.WriteFileAtomicSystem(kubeProxyServicePath, []byte(serviceContent), configFilePerm); err != nil {
		return fmt.Errorf("failed to write kube-proxy service file: %w", err)
	}

	// Create drop-in configuration for additional environment variables if needed
	dropInContent := `[Service]
Environment="KUBECONFIG=/etc/kubernetes/proxy.conf"
`

	if err := utils.WriteFileAtomicSystem(kubeProxyDropInPath, []byte(dropInContent), configFilePerm); err != nil {
		return fmt.Errorf("failed to write kube-proxy drop-in configuration: %w", err)
	}

	i.logger.Info("Kube-proxy systemd service created successfully")
	return nil
}

// downloadKubeProxyBinary downloads the kube-proxy binary for the configured Kubernetes version
func (i *Installer) downloadKubeProxyBinary() error {
	// Check if binary already exists and is valid
	if utils.FileExistsAndValid(kubeProxyBinaryPath) {
		i.logger.Info("Kube-proxy binary already exists, skipping download")
		return nil
	}

	i.logger.Info("Downloading kube-proxy binary")

	// Construct kube-proxy download URL
	fileName, downloadURL, err := i.constructKubeProxyDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct kube-proxy download URL: %w", err)
	}

	// Create temporary file
	tempFile := fmt.Sprintf("/tmp/%s", fileName)
	defer func() {
		if err := utils.RunCleanupCommand(tempFile); err != nil {
			i.logger.Warnf("Failed to clean up temp file %s: %v", tempFile, err)
		}
	}()

	// Download the binary
	if err := i.downloadFile(downloadURL, tempFile); err != nil {
		return fmt.Errorf("failed to download kube-proxy: %w", err)
	}

	// Move to final location and set permissions
	if err := utils.RunSystemCommand("mv", tempFile, kubeProxyBinaryPath); err != nil {
		return fmt.Errorf("failed to move kube-proxy binary to final location: %w", err)
	}

	// Set executable permissions
	if err := utils.RunSystemCommand("chmod", "0755", kubeProxyBinaryPath); err != nil {
		return fmt.Errorf("failed to set executable permissions on kube-proxy: %w", err)
	}

	i.logger.Info("Kube-proxy binary downloaded and installed successfully")
	return nil
}

// downloadFile downloads a file from URL to destination path
func (i *Installer) downloadFile(url, dest string) error {
	// Use curl to download the file
	if err := utils.RunSystemCommand("curl", "-L", "-o", dest, url); err != nil {
		return fmt.Errorf("failed to download file from %s: %w", url, err)
	}

	// Verify the file was downloaded
	if !utils.FileExistsAndValid(dest) {
		return fmt.Errorf("downloaded file is empty or doesn't exist: %s", dest)
	}

	return nil
}

// constructKubeProxyDownloadURL constructs the download URL and filename for kube-proxy binary
func (i *Installer) constructKubeProxyDownloadURL() (string, string, error) {
	// Get Kubernetes version from configuration
	kubeVersion := i.config.GetKubernetesVersion()
	if kubeVersion == "" {
		return "", "", fmt.Errorf("kubernetes version not configured")
	}

	// Get system architecture
	arch, err := utils.GetArc()
	if err != nil {
		return "", "", fmt.Errorf("failed to determine system architecture: %w", err)
	}

	// Construct download URL
	downloadURL := fmt.Sprintf(kubeProxyDownloadURL, kubeVersion, arch)
	fileName := fmt.Sprintf("kube-proxy-%s-%s", kubeVersion, arch)

	i.logger.Infof("Constructed kube-proxy download URL: %s", downloadURL)
	return fileName, downloadURL, nil
}
