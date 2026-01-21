package kubelet

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/sirupsen/logrus"

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
	// enforce reconfiguration every time for kubelet to ensure latest settings
	// so that any config changes are applied
	return false
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

	// Ensure required packages are installed
	if err := i.ensureRequiredPackages(); err != nil {
		return fmt.Errorf("failed to install required packages: %w", err)
	}

	// Create required directories
	if err := i.createRequiredDirectories(); err != nil {
		return fmt.Errorf("failed to create required directories: %w", err)
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
		kubeletDefaultsPath,
		kubeletServicePath,
		kubeletContainerdConfig,
		kubeletTLSBootstrapConfig,
		kubeconfigPath,
		kubeletTokenScriptPath,
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

// createRequiredDirectories creates directories that kubelet expects to exist
func (i *Installer) createRequiredDirectories() error {
	i.logger.Info("Creating required directories for kubelet")

	directories := []string{
		kubeletManifestsDir,    // For pod manifests
		kubeletVolumePluginDir, // For volume plugins
		kubeletVarDir,          // For kubelet data
	}

	for _, dir := range directories {
		i.logger.Debugf("Creating directory: %s", dir)
		if err := utils.RunSystemCommand("mkdir", "-p", dir); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	i.logger.Info("Required directories created successfully")
	return nil
}

// ensureRequiredPackages installs packages required by kubelet (jq for token script, iptables for service)
func (i *Installer) ensureRequiredPackages() error {
	packages := []string{"jq", "iptables"}

	i.logger.Info("Checking for required kubelet packages")

	for _, pkg := range packages {
		if err := utils.RunSystemCommand("which", pkg); err != nil {
			i.logger.Infof("Installing %s...", pkg)
			if err := utils.RunSystemCommand("apt", "install", "-y", pkg); err != nil {
				return fmt.Errorf("failed to install %s: %w", pkg, err)
			}
			i.logger.Infof("Successfully installed %s", pkg)
		} else {
			i.logger.Debugf("%s is already installed", pkg)
		}
	}

	i.logger.Info("All required kubelet packages are available")
	return nil
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
		mapToEvictionThresholds(i.config.Node.Kubelet.EvictionHard, ","),
		mapToKeyValuePairs(i.config.Node.Kubelet.KubeReserved, ","),
		i.config.Node.Kubelet.ImageGCHighThreshold,
		i.config.Node.Kubelet.ImageGCLowThreshold,
		i.config.Node.MaxPods,
		i.config.Containerd.PauseImage)

	// Ensure /etc/default directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", etcDefaultDir); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", etcDefaultDir, err)
	}

	// Write kubelet defaults file atomically with proper permissions
	if err := utils.WriteFileAtomicSystem(kubeletDefaultsPath, []byte(kubeletDefaults), 0644); err != nil {
		return fmt.Errorf("failed to create kubelet defaults file: %w", err)
	}

	return nil
}

// createSystemdDropInFile creates a systemd drop-in file with the given content
func (i *Installer) createSystemdDropInFile(filePath, content, description string) error {
	// Ensure kubelet service.d directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", kubeletServiceDir); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", kubeletServiceDir, err)
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

	return i.createSystemdDropInFile(kubeletContainerdConfig, containerdConf, "kubelet containerd config file")
}

// createKubeletTLSBootstrapConfig creates the kubelet TLS bootstrap configuration
func (i *Installer) createKubeletTLSBootstrapConfig() error {
	tlsBootstrapConf := `[Service]
Environment=KUBELET_TLS_BOOTSTRAP_FLAGS="--kubeconfig /var/lib/kubelet/kubeconfig"`

	return i.createSystemdDropInFile(kubeletTLSBootstrapConfig, tlsBootstrapConf, "kubelet TLS bootstrap config file")
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
	if err := utils.WriteFileAtomicSystem(kubeletServicePath, []byte(kubeletService), 0644); err != nil {
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

curl -s -H Metadata:true -H "Authorization: Basic $CHALLENGE_TOKEN" $TOKEN_URL | jq "$EXECCREDENTIAL"`, aksServiceResourceID)

	// Ensure /var/lib/kubelet directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", kubeletVarDir); err != nil {
		return fmt.Errorf("failed to create kubelet var directory: %w", err)
	}

	// Write token script atomically with executable permissions
	if err := utils.WriteFileAtomicSystem(kubeletTokenScriptPath, []byte(tokenScript), 0755); err != nil {
		return fmt.Errorf("failed to create Arc token script: %w", err)
	}

	// Ensure the script has executable permissions (explicit chmod as backup)
	if err := utils.RunSystemCommand("chmod", "755", kubeletTokenScriptPath); err != nil {
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

	serverURL, caCertData, err := utils.ExtractClusterInfo(kubeconfig)
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
	if err := utils.WriteFileAtomicSystem(kubeletKubeconfigPath, []byte(kubeconfigContent), 0600); err != nil {
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

// mapToKeyValuePairs converts a map to key=value pairs joined by separator
func mapToKeyValuePairs(m map[string]string, separator string) string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(pairs, separator)
}

// mapToEvictionThresholds converts a map to key<value pairs for kubelet eviction thresholds
func mapToEvictionThresholds(m map[string]string, separator string) string {
	pairs := make([]string, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, fmt.Sprintf("%s<%s", k, v))
	}
	return strings.Join(pairs, separator)
}
