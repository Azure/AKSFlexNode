package kubelet

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles kubelet installation and configuration
type Installer struct {
	config   *config.Config
	logger   *logrus.Logger
	mcClient *armcontainerservice.ManagedClustersClient
}

// NewInstaller creates a new kubelet Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the step name for the executor interface
func (i *Installer) GetName() string {
	return "KubeletInstaller"
}

// Execute installs and configures kubelet service
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Installing and configuring kubelet")
	// Set up mc client for getting cluster info
	if err := i.setUpClients(); err != nil {
		return fmt.Errorf("failed to set up Azure SDK clients: %w", err)
	}

	// Configure kubelet service with systemd unit file and default settings
	if err := i.configure(ctx); err != nil {
		return fmt.Errorf("failed to configure kubelet: %w", err)
	}

	i.logger.Info("Kubelet installed and configured successfully")
	return nil
}

// IsCompleted checks if kubelet service has been installed and configured
func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Check if all required files exist
	if !utils.FileExists(KubeletDefaultsPath) {
		return false
	}
	if !utils.FileExists(KubeletServicePath) {
		return false
	}
	if !utils.FileExists(KubeletContainerdConfig) {
		return false
	}
	if !utils.FileExists(KubeletTLSBootstrapConfig) {
		return false
	}

	// Check if kubeconfig exists for exec credential authentication
	if !utils.FileExists(KubeletKubeconfigPath) {
		return false
	}

	// Check if Arc token script exists
	if !utils.FileExists(KubeletTokenScriptPath) {
		return false
	}

	// Validate the configuration files have expected content
	if !i.validateKubeletConfiguration() {
		return false
	}
	return true
}

// Validate validates prerequisites for kubelet installation
func (i *Installer) Validate(_ context.Context) error {
	i.logger.Debug("Validating prerequisites for kubelet installation")
	// No specific prerequisites for kubelet configuration
	return nil
}

// configure configures kubelet service with systemd unit file and default settings
func (i *Installer) configure(ctx context.Context) error {
	i.logger.Info("Configuring kubelet")

	// Clean up any existing stale configuration files
	if err := i.cleanupExistingConfiguration(); err != nil {
		i.logger.Warnf("Failed to cleanup existing kubelet configuration: %v", err)
		// Continue anyway - we'll overwrite the files
	}

	// Create kubelet defaults file
	if err := i.createKubeletDefaultsFile(); err != nil {
		return err
	}

	// Create Arc token script for exec credential authentication
	if err := i.createArcTokenScript(); err != nil {
		return err
	}

	// Create kubeconfig with exec credential provider
	if err := i.createKubeconfigWithExecCredential(ctx); err != nil {
		return err
	}

	// Create kubelet containerd configuration
	if err := i.createKubeletContainerdConfig(); err != nil {
		return err
	}

	// Create kubelet TLS bootstrap configuration
	if err := i.createKubeletTLSBootstrapConfig(); err != nil {
		return err
	}

	// Create main kubelet service
	if err := i.createKubeletServiceFile(); err != nil {
		return err
	}

	return nil
}

// cleanupExistingConfiguration removes any existing kubelet configuration that may be corrupted
func (i *Installer) cleanupExistingConfiguration() error {
	i.logger.Debug("Cleaning up existing kubelet configuration files")

	// List of files to clean up
	kubeconfigPath := filepath.Join(i.config.Paths.Kubernetes.ConfigDir, "kubeconfig")
	filesToClean := []string{
		KubeletDefaultsPath,
		KubeletServicePath,
		KubeletContainerdConfig,
		KubeletTLSBootstrapConfig,
		kubeconfigPath,
		KubeletTokenScriptPath,
	}

	for _, file := range filesToClean {
		if utils.FileExists(file) {
			i.logger.Debugf("Removing existing kubelet config file: %s", file)
			if err := utils.RunCleanupCommand(file); err != nil {
				i.logger.Warnf("Failed to remove %s: %v", file, err)
			}
		}
	}

	return nil
}

// validateKubeletConfiguration validates that kubelet configuration files have expected content
func (i *Installer) validateKubeletConfiguration() bool {
	// Validate kubelet defaults file
	if !i.validateKubeletDefaultsFile() {
		return false
	}

	// Validate kubelet service file
	if !i.validateKubeletServiceFile() {
		return false
	}

	return true
}

// validateFileContent checks if a file contains all expected strings
func (i *Installer) validateFileContent(filePath string, expectedStrings []string, description string) bool {
	output, err := utils.RunCommandWithOutput("cat", filePath)
	if err != nil {
		i.logger.Debugf("Failed to read %s: %v", description, err)
		return false
	}

	for _, expected := range expectedStrings {
		if !strings.Contains(output, expected) {
			i.logger.Debugf("Missing expected setting in %s: %s", description, expected)
			return false
		}
	}

	return true
}

// validateKubeletDefaultsFile checks if the kubelet defaults file has expected content
func (i *Installer) validateKubeletDefaultsFile() bool {
	expectedSettings := []string{
		"KUBELET_NODE_LABELS=",
		"KUBELET_CONFIG_FILE_FLAGS=",
		"KUBELET_FLAGS=",
		"--cgroup-driver=systemd",
		"--authorization-mode=Webhook",
	}

	return i.validateFileContent(KubeletDefaultsPath, expectedSettings, "kubelet defaults file")
}

// validateKubeletServiceFile checks if the kubelet service file has expected content
func (i *Installer) validateKubeletServiceFile() bool {
	expectedSettings := []string{
		"[Unit]",
		"Description=Kubelet",
		"ExecStart=/usr/local/bin/kubelet",
		"WantedBy=multi-user.target",
	}

	return i.validateFileContent(KubeletServicePath, expectedSettings, "kubelet service file")
}

// createKubeletDefaultsFile creates the kubelet defaults configuration file
func (i *Installer) createKubeletDefaultsFile() error {
	// Create kubelet default config
	labels := make([]string, 0, len(i.config.Node.Labels))
	for key, value := range i.config.Node.Labels {
		labels = append(labels, fmt.Sprintf("%s=%s", key, value))
	}

	kubeletDefaults := fmt.Sprintf(`KUBELET_NODE_LABELS="%s"
KUBELET_CONFIG_FILE_FLAGS=""
KUBELET_FLAGS="\
  --address=0.0.0.0 \
  --anonymous-auth=false \
  --authentication-token-webhook=true \
  --authorization-mode=Webhook \
  --cgroup-driver=systemd \
  --cgroups-per-qos=true \
  --enforce-node-allocatable=pods \
  --event-qps=0  \
  --eviction-hard=%s  \
  --kube-reserved=%s  \
  --image-gc-high-threshold=%d  \
  --image-gc-low-threshold=%d  \
  --max-pods=%d  \
  --node-status-update-frequency=10s  \
  --pod-infra-container-image=%s  \
  --pod-max-pids=-1  \
  --protect-kernel-defaults=true  \
  --read-only-port=0  \
  --resolv-conf=/run/systemd/resolve/resolv.conf  \
  --streaming-connection-idle-timeout=4h  \
  --tls-cipher-suites=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_RSA_WITH_AES_256_GCM_SHA384,TLS_RSA_WITH_AES_128_GCM_SHA256 \
  "`,
		strings.Join(labels, ","),
		utils.MapToEvictionThresholds(i.config.Node.Kubelet.EvictionHard, ","),
		utils.MapToKeyValuePairs(i.config.Node.Kubelet.KubeReserved, ","),
		i.config.Node.Kubelet.ImageGCHighThreshold,
		i.config.Node.Kubelet.ImageGCLowThreshold,
		i.config.Node.MaxPods,
		i.config.Containerd.PauseImage)

	// Ensure /etc/default directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", EtcDefaultDir); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", EtcDefaultDir, err)
	}

	// Write kubelet defaults file atomically with proper permissions
	if err := utils.WriteFileAtomicSystem(KubeletDefaultsPath, []byte(kubeletDefaults), 0644); err != nil {
		return fmt.Errorf("failed to create kubelet defaults file: %w", err)
	}

	return nil
}

// createSystemdDropInFile creates a systemd drop-in file with the given content
func (i *Installer) createSystemdDropInFile(filePath, content, description string) error {
	// Ensure kubelet service.d directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", KubeletServiceDir); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", KubeletServiceDir, err)
	}

	// Write config file atomically with proper permissions
	if err := utils.WriteFileAtomicSystem(filePath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to create %s: %w", description, err)
	}

	return nil
}

// createKubeletContainerdConfig creates the kubelet containerd configuration
func (i *Installer) createKubeletContainerdConfig() error {
	containerdConf := `[Service]
Environment=KUBELET_CONTAINERD_FLAGS="--runtime-request-timeout=15m --container-runtime-endpoint=unix:///run/containerd/containerd.sock"`

	return i.createSystemdDropInFile(KubeletContainerdConfig, containerdConf, "kubelet containerd config file")
}

// createKubeletTLSBootstrapConfig creates the kubelet TLS bootstrap configuration
func (i *Installer) createKubeletTLSBootstrapConfig() error {
	tlsBootstrapConf := `[Service]
Environment=KUBELET_TLS_BOOTSTRAP_FLAGS="--kubeconfig /var/lib/kubelet/kubeconfig"`

	return i.createSystemdDropInFile(KubeletTLSBootstrapConfig, tlsBootstrapConf, "kubelet TLS bootstrap config file")
}

// createKubeletServiceFile creates the main kubelet systemd service file
func (i *Installer) createKubeletServiceFile() error {
	kubeletService := `[Unit]
Description=Kubelet
ConditionPathExists=/usr/local/bin/kubelet
[Service]
Restart=always
EnvironmentFile=/etc/default/kubelet
SuccessExitStatus=143
# Ace does not recall why this is done
ExecStartPre=/bin/bash -c "if [ $(mount | grep \"/var/lib/kubelet\" | wc -l) -le 0 ] ; then /bin/mount --bind /var/lib/kubelet /var/lib/kubelet ; fi"
ExecStartPre=/bin/mount --make-shared /var/lib/kubelet
ExecStartPre=-/sbin/ebtables -t nat --list
ExecStartPre=-/sbin/iptables -t nat --numeric --list
ExecStart=/usr/local/bin/kubelet \
        --enable-server \
        --node-labels="${KUBELET_NODE_LABELS}" \
        --v=2 \
        --volume-plugin-dir=/etc/kubernetes/volumeplugins \
        --pod-manifest-path=/etc/kubernetes/manifests/ \
        $KUBELET_TLS_BOOTSTRAP_FLAGS \
        $KUBELET_CONFIG_FILE_FLAGS \
        $KUBELET_CONTAINERD_FLAGS \
        $KUBELET_FLAGS
[Install]
WantedBy=multi-user.target`

	// Write kubelet service file atomically with proper permissions
	if err := utils.WriteFileAtomicSystem(KubeletServicePath, []byte(kubeletService), 0644); err != nil {
		return fmt.Errorf("failed to create kubelet service file: %w", err)
	}

	return nil
}

// createArcTokenScript creates the Arc token script for exec credential authentication
func (i *Installer) createArcTokenScript() error {
	// Arc HIMDS token script using proven Www-Authenticate challenge approach
	tokenScript := fmt.Sprintf(`#!/bin/bash

# Fetch an AAD token from Azure Arc HIMDS and output it in the ExecCredential format
# https://learn.microsoft.com/azure/azure-arc/servers/managed-identity-authentication

TOKEN_URL="http://127.0.0.1:40342/metadata/identity/oauth2/token?api-version=2019-11-01&resource=%s"
EXECCREDENTIAL='''
{
  "kind": "ExecCredential",
  "apiVersion": "client.authentication.k8s.io/v1beta1",
  "spec": {
    "interactive": false
  },
  "status": {
    "expirationTimestamp": .expires_on | tonumber | todate,
    "token": .access_token
  }
}
'''

# Arc IMDS requires a challenge token from a file only readable by root for security
CHALLENGE_TOKEN_PATH=$(curl -s -D - -H Metadata:true $TOKEN_URL | grep Www-Authenticate | cut -d "=" -f 2 | tr -d "[:cntrl:]")
CHALLENGE_TOKEN=$(cat $CHALLENGE_TOKEN_PATH)
if [ $? -ne 0 ]; then
    echo "Could not retrieve challenge token, double check that this command is run with root privileges."
    exit 255
fi

curl -s -H Metadata:true -H "Authorization: Basic $CHALLENGE_TOKEN" $TOKEN_URL | jq "$EXECCREDENTIAL"`, AKSServiceResourceID)

	// Ensure /var/lib/kubelet directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", KubeletVarDir); err != nil {
		return fmt.Errorf("failed to create kubelet var directory: %w", err)
	}

	// Write token script atomically with executable permissions
	if err := utils.WriteFileAtomicSystem(KubeletTokenScriptPath, []byte(tokenScript), 0755); err != nil {
		return fmt.Errorf("failed to create Arc token script: %w", err)
	}

	// Ensure the script has executable permissions (explicit chmod as backup)
	if err := utils.RunSystemCommand("chmod", "755", KubeletTokenScriptPath); err != nil {
		return fmt.Errorf("failed to set executable permissions on Arc token script: %w", err)
	}

	return nil
}

// createKubeconfigWithExecCredential creates kubeconfig with exec credential provider for Arc authentication
func (i *Installer) createKubeconfigWithExecCredential(ctx context.Context) error {
	kubeconfig, err := i.getClusterCredentials(ctx)
	if err != nil {
		return fmt.Errorf("failed to get cluster credentials: %w", err)
	}

	serverURL, caCertData, err := i.extractClusterInfo(kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to extract cluster info from kubeconfig: %w", err)
	}

	// Create cluster configuration based on whether we have CA cert
	var clusterConfig string
	if caCertData != "" {
		clusterConfig = fmt.Sprintf(`- cluster:
    certificate-authority-data: %s
    server: %s
  name: %s`, caCertData, serverURL, i.config.Azure.TargetCluster.Name)
	} else {
		clusterConfig = fmt.Sprintf(`- cluster:
    insecure-skip-tls-verify: true
    server: %s
  name: %s`, serverURL, i.config.Azure.TargetCluster.Name)
	}

	// Create kubeconfig with exec credential provider pointing to token script
	kubeconfigContent := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
%s
contexts:
- context:
    cluster: %s
    user: arc-user
  name: arc-context
current-context: arc-context
users:
- name: arc-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: /var/lib/kubelet/token.sh
      env: null
      provideClusterInfo: false
`,
		clusterConfig,
		i.config.Azure.TargetCluster.Name)

	// Write kubeconfig file to the correct location for kubelet
	if err := utils.WriteFileAtomicSystem(KubeletKubeconfigPath, []byte(kubeconfigContent), 0600); err != nil {
		return fmt.Errorf("failed to create kubeconfig file: %w", err)
	}

	return nil
}

func (i *Installer) setUpClients() error {
	cred, err := auth.NewAuthProvider().UserCredential(config.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to get authentication credential: %w", err)
	}
	clusterSubID := i.config.GetTargetClusterSubscriptionID()
	clientFactory, err := armcontainerservice.NewClientFactory(clusterSubID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create Azure Container Service client factory: %w", err)
	}
	i.mcClient = clientFactory.NewManagedClustersClient()
	return nil

}

// GetClusterCredentials retrieves cluster kube admin credentials using Azure SDK
func (i *Installer) getClusterCredentials(ctx context.Context) ([]byte, error) {
	cfg := config.GetConfig()
	clusterResourceGroup := cfg.GetTargetClusterResourceGroup()
	clusterName := cfg.GetTargetClusterName()
	i.logger.Infof("Fetching cluster credentials for cluster %s in resource group %s using Azure SDK",
		clusterName, clusterResourceGroup)

	// Get cluster admin credentials using the Azure SDK
	resp, err := i.mcClient.ListClusterAdminCredentials(ctx, clusterResourceGroup, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get cluster admin credentials for %s in resource group %s: %w", clusterName, clusterResourceGroup, err)
	}

	if len(resp.Kubeconfigs) == 0 {
		return nil, fmt.Errorf("no kubeconfig found in cluster admin credentials response")
	}

	kubeconfig := resp.Kubeconfigs[0]
	if kubeconfig == nil {
		return nil, fmt.Errorf("kubeconfig is nil in the response")
	}

	i.logger.Debugf("Found %d kubeconfig(s), using the first one of name %s", len(resp.Kubeconfigs), to.String(kubeconfig.Name))

	if len(kubeconfig.Value) == 0 {
		return nil, fmt.Errorf("kubeconfig value is empty")
	}

	// The Value field is already []byte containing the kubeconfig data, no decoding needed
	return kubeconfig.Value, nil
}

// extractClusterInfo extracts server URL and CA certificate data from kubeconfig
func (i *Installer) extractClusterInfo(kubeconfigData []byte) (string, string, error) {
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

	i.logger.Debugf("Using cluster: %s", clusterName)

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
