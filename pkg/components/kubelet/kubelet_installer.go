package kubelet

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
)

// Installer handles kubelet installation and configuration
type Installer struct {
	config *config.Config
	logger *logrus.Logger
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

	// Create authentication configuration based on auth method
	if i.config.IsBootstrapTokenConfigured() {
		// Bootstrap token authentication uses a simple token-based kubeconfig
		if err := i.createKubeconfigWithBootstrapToken(ctx); err != nil {
			return err
		}
	} else {
		// Arc or Service Principal authentication uses exec credential provider
		// Create token script for exec credential authentication (Arc or Service Principal)
		if err := i.createTokenScript(); err != nil {
			return err
		}

		// Create kubeconfig with exec credential provider
		if err := i.createKubeconfigWithExecCredential(ctx); err != nil {
			return err
		}
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
		kubernetesPKIDir,       // For PKI certificates
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

	// Set --rotate-certificates based on authentication mode
	// Bootstrap token mode: true (kubelet will rotate certificates after TLS bootstrap)
	// Other modes (Arc/SP/MSI): false (authentication is handled via exec credential provider)
	rotateCerts := i.config.IsBootstrapTokenConfigured()

	kubeletDefaults := fmt.Sprintf(`KUBELET_NODE_LABELS="%s"
KUBELET_CONFIG_FILE_FLAGS=""
KUBELET_FLAGS="\
  --v=%d \
  --address=0.0.0.0 \
  --anonymous-auth=false \
  --authentication-token-webhook=true \
  --authorization-mode=Webhook \
  --client-ca-file=%s \
  --cgroup-driver=systemd \
  --cgroups-per-qos=true \
  --enforce-node-allocatable=pods \
  --cluster-dns=%s \
  --cluster-domain=cluster.local \
  --event-qps=0  \
  --eviction-hard=%s  \
  --kube-reserved=%s  \
  --image-gc-high-threshold=%d  \
  --image-gc-low-threshold=%d  \
  --max-pods=%d  \
  --node-status-update-frequency=10s  \
  --pod-max-pids=-1  \
  --protect-kernel-defaults=true  \
  --read-only-port=0  \
  --resolv-conf=/run/systemd/resolve/resolv.conf  \
  --streaming-connection-idle-timeout=4h  \
  --rotate-certificates=%t \
  --tls-cipher-suites=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,TLS_RSA_WITH_AES_256_GCM_SHA384,TLS_RSA_WITH_AES_128_GCM_SHA256 \
  "`,
		strings.Join(labels, ","),
		i.config.Node.Kubelet.Verbosity,
		apiserverClientCAPath,
		i.config.Node.Kubelet.DNSServiceIP,
		mapToEvictionThresholds(i.config.Node.Kubelet.EvictionHard, ","),
		mapToKeyValuePairs(i.config.Node.Kubelet.KubeReserved, ","),
		i.config.Node.Kubelet.ImageGCHighThreshold,
		i.config.Node.Kubelet.ImageGCLowThreshold,
		i.config.Node.MaxPods,
		rotateCerts)

	// Ensure /etc/default directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", etcDefaultDir); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", etcDefaultDir, err)
	}

	// Write kubelet defaults file atomically with proper permissions
	if err := utilio.WriteFile(kubeletDefaultsPath, []byte(kubeletDefaults), 0o644); err != nil {
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
	if err := utilio.WriteFile(filePath, []byte(content), 0o644); err != nil {
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
        --volume-plugin-dir=/etc/kubernetes/volumeplugins \
        --pod-manifest-path=/etc/kubernetes/manifests/ \
        $KUBELET_TLS_BOOTSTRAP_FLAGS \
        $KUBELET_CONFIG_FILE_FLAGS \
        $KUBELET_CONTAINERD_FLAGS \
        $KUBELET_FLAGS
[Install]
WantedBy=multi-user.target`

	// Write kubelet service file atomically with proper permissions
	if err := utilio.WriteFile(kubeletServicePath, []byte(kubeletService), 0o644); err != nil {
		return fmt.Errorf("failed to create kubelet service file: %w", err)
	}

	return nil
}

// createTokenScript creates either Arc, MSI, or Service Principal token script based on configuration
func (i *Installer) createTokenScript() error {
	if i.config.IsARCEnabled() {
		return i.createArcTokenScript()
	} else if i.config.IsMIConfigured() {
		return i.createMSITokenScript()
	} else if i.config.IsSPConfigured() {
		return i.createServicePrincipalTokenScript()
	} else if i.config.IsBootstrapTokenConfigured() {
		// Bootstrap token doesn't need a token script
		return nil
	} else {
		return fmt.Errorf("no valid authentication method configured - either Arc, MSI, Service Principal, or Bootstrap Token must be configured")
	}
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

	return i.writeTokenScript(tokenScript)
}

// createMSITokenScript creates the MSI token script for exec credential authentication using Azure VM Managed Identity
func (i *Installer) createMSITokenScript() error {
	clientIDParam := ""
	if i.config.Azure.ManagedIdentity != nil && i.config.Azure.ManagedIdentity.ClientID != "" {
		clientIDParam = fmt.Sprintf("\nCLIENT_ID=\"%s\"", i.config.Azure.ManagedIdentity.ClientID)
	}

	// Azure VM MSI token script using IMDS endpoint
	tokenScript := fmt.Sprintf(`#!/bin/bash

# Fetch an AAD token from Azure Instance Metadata Service (IMDS) using VM Managed Identity
# https://learn.microsoft.com/azure/active-directory/managed-identities-azure-resources/how-to-use-vm-token

IMDS_ENDPOINT="http://169.254.169.254/metadata/identity/oauth2/token"
API_VERSION="2018-02-01"
RESOURCE="%s"%s

# Build IMDS URL with optional client_id parameter
IMDS_URL="$IMDS_ENDPOINT?api-version=$API_VERSION&resource=$RESOURCE"
if [ -n "${CLIENT_ID:-}" ]; then
    IMDS_URL="$IMDS_URL&client_id=$CLIENT_ID"
fi

# Get token from IMDS
TOKEN_RESPONSE=$(curl -s -H Metadata:true "$IMDS_URL")

if [ $? -ne 0 ]; then
    echo "Failed to get token from Azure IMDS" >&2
    exit 255
fi

# Extract access token and expiry
ACCESS_TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.access_token')
if [ "$ACCESS_TOKEN" == "null" ] || [ -z "$ACCESS_TOKEN" ]; then
    echo "Failed to extract access token from IMDS response" >&2
    exit 255
fi

EXPIRES_ON=$(echo "$TOKEN_RESPONSE" | jq -r '.expires_on')

# Return in ExecCredential format
cat <<EOF
{
  "kind": "ExecCredential",
  "apiVersion": "client.authentication.k8s.io/v1beta1",
  "spec": {
    "interactive": false
  },
  "status": {
    "expirationTimestamp": "$(date -d @$EXPIRES_ON --iso-8601=seconds)",
    "token": "$ACCESS_TOKEN"
  }
}
EOF
`, aksServiceResourceID, clientIDParam)

	return i.writeTokenScript(tokenScript)
}

// createServicePrincipalTokenScript creates the Service Principal token script
func (i *Installer) createServicePrincipalTokenScript() error {
	sp := i.config.Azure.ServicePrincipal
	tokenScript := fmt.Sprintf(`#!/bin/bash

# Get Azure AD token using Service Principal credentials for direct AKS authentication

CLIENT_ID="%s"
CLIENT_SECRET="%s"
TENANT_ID="%s"

TOKEN_RESPONSE=$(curl -s -X POST \
  "https://login.microsoftonline.com/${TENANT_ID}/oauth2/v2.0/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "client_id=${CLIENT_ID}" \
  -d "client_secret=${CLIENT_SECRET}" \
  -d "scope=%s/.default" \
  -d "grant_type=client_credentials")

if [ $? -ne 0 ]; then
    echo "Failed to get token from Azure AD"
    exit 255
fi

ACCESS_TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.access_token')
if [ "$ACCESS_TOKEN" == "null" ] || [ -z "$ACCESS_TOKEN" ]; then
    echo "Failed to extract access token from response: $TOKEN_RESPONSE"
    exit 255
fi

EXPIRES_IN=$(echo "$TOKEN_RESPONSE" | jq -r '.expires_in')
EXPIRY_TIME=$(date -d "+${EXPIRES_IN} seconds" --iso-8601=seconds)

# Return in ExecCredential format
cat <<EOF
{
  "kind": "ExecCredential",
  "apiVersion": "client.authentication.k8s.io/v1beta1",
  "spec": {
    "interactive": false
  },
  "status": {
    "expirationTimestamp": "${EXPIRY_TIME}",
    "token": "${ACCESS_TOKEN}"
  }
}
EOF`, sp.ClientID, sp.ClientSecret, sp.TenantID, aksServiceResourceID)

	return i.writeTokenScript(tokenScript)
}

// writeTokenScript helper method to write the token script with proper permissions
func (i *Installer) writeTokenScript(tokenScript string) error {
	// Ensure /var/lib/kubelet directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", kubeletVarDir); err != nil {
		return fmt.Errorf("failed to create kubelet var directory: %w", err)
	}

	// Write token script atomically with executable permissions
	if err := utilio.WriteFile(kubeletTokenScriptPath, []byte(tokenScript), 0o755); err != nil {
		return fmt.Errorf("failed to create token script: %w", err)
	}

	// Ensure the script has executable permissions (explicit chmod as backup)
	if err := utils.RunSystemCommand("chmod", "755", kubeletTokenScriptPath); err != nil {
		return fmt.Errorf("failed to set executable permissions on token script: %w", err)
	}

	return nil
}

// getCACertFromConfigMap fetches the CA certificate from kube-root-ca.crt ConfigMap in kube-public namespace
func (i *Installer) getCACertFromConfigMap(ctx context.Context, serverURL string) (string, error) {
	i.logger.Info("Fetching CA certificate from kube-root-ca.crt ConfigMap in kube-public namespace")

	// Create in-cluster or anonymous client config pointing to the server
	config := &rest.Config{
		Host: serverURL,
		// Use TLS but skip verification since we're getting the CA cert to verify future connections
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: true,
		},
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	// Get the kube-root-ca.crt ConfigMap from kube-public namespace
	cm, err := clientset.CoreV1().ConfigMaps("kube-public").Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get kube-root-ca.crt ConfigMap from kube-public namespace: %w", err)
	}

	// Extract the CA certificate data
	caCert, exists := cm.Data["ca.crt"]
	if !exists {
		return "", fmt.Errorf("ca.crt not found in kube-root-ca.crt ConfigMap")
	}

	if caCert == "" {
		return "", fmt.Errorf("ca.crt is empty in kube-root-ca.crt ConfigMap")
	}

	i.logger.Info("Successfully retrieved CA certificate from kube-root-ca.crt ConfigMap")

	// Encode the CA certificate to base64 for use in kubeconfig
	return base64.StdEncoding.EncodeToString([]byte(caCert)), nil
}

// writeClientCACertificate writes the API server client CA certificate to disk for kubelet authentication
func (i *Installer) writeClientCACertificate(caCertData string) error {
	if caCertData == "" {
		i.logger.Debug("No CA certificate data provided, skipping client CA file creation")
		return nil
	}

	i.logger.Info("Writing API server client CA certificate")

	// Decode base64 CA certificate data
	caCertBytes, err := base64.StdEncoding.DecodeString(caCertData)
	if err != nil {
		return fmt.Errorf("failed to decode CA certificate data: %w", err)
	}

	// Write CA certificate atomically with proper permissions
	if err := utilio.WriteFile(apiserverClientCAPath, caCertBytes, 0o644); err != nil {
		return fmt.Errorf("failed to write API server client CA certificate: %w", err)
	}

	i.logger.Infof("API server client CA certificate written to %s", apiserverClientCAPath)
	return nil
}

// createKubeconfigWithExecCredential creates kubeconfig with exec credential provider for authentication
func (i *Installer) createKubeconfigWithExecCredential(ctx context.Context) error {
	// Get the server URL from configuration
	serverURL := i.config.Node.Kubelet.ServerURL
	if serverURL == "" {
		return fmt.Errorf("server URL not configured in kubelet settings")
	}

	// Fetch CA certificate from kube-root-ca.crt ConfigMap in kube-public namespace
	i.logger.Info("Fetching CA certificate from kube-root-ca.crt ConfigMap")
	caCertData, err := i.getCACertFromConfigMap(ctx, serverURL)
	if err != nil {
		return fmt.Errorf("failed to get CA certificate from ConfigMap: %w", err)
	}

	// Write CA certificate to file for kubelet client authentication
	if err := i.writeClientCACertificate(caCertData); err != nil {
		return fmt.Errorf("failed to write client CA certificate: %w", err)
	}

	// Create cluster configuration with CA cert from ConfigMap
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

	// Determine user and context names based on auth method
	var userName, contextName string
	if i.config.IsARCEnabled() {
		userName = "arc-user"
		contextName = "arc-context"
	} else {
		userName = "sp-user"
		contextName = "sp-context"
	}

	// Create kubeconfig with exec credential provider pointing to token script
	kubeconfigContent := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
%s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
current-context: %s
users:
- name: %s
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: /var/lib/kubelet/token.sh
      env: null
      provideClusterInfo: false
`,
		clusterConfig,
		i.config.Azure.TargetCluster.Name,
		userName,
		contextName,
		contextName,
		userName)

	// Write kubeconfig file to the correct location for kubelet
	if err := utilio.WriteFile(KubeletKubeconfigPath, []byte(kubeconfigContent), 0o600); err != nil {
		return fmt.Errorf("failed to create kubeconfig file: %w", err)
	}

	return nil
}

// createKubeconfigWithBootstrapToken  creates a kubeconfig file with bootstrap token authentication
func (i *Installer) createKubeconfigWithBootstrapToken(ctx context.Context) error {
	i.logger.Info("Creating bootstrap token kubeconfig")

	// Use cluster info from kubelet config (required fields validated earlier)
	serverURL := i.config.Node.Kubelet.ServerURL
	caCertData := i.config.Node.Kubelet.CACertData
	bootstrapToken := i.config.Azure.BootstrapToken.Token

	// Write CA certificate to file for kubelet client authentication
	if err := i.writeClientCACertificate(caCertData); err != nil {
		return fmt.Errorf("failed to write client CA certificate: %w", err)
	}

	// Get node hostname for audit logging
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname for bootstrap kubeconfig: %w", err)
	}

	// Include node name in username for better auditing in Kubernetes API server logs
	username := fmt.Sprintf("kubelet-bootstrap-%s", hostname)

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

	// Create kubeconfig with bootstrap token authentication
	kubeconfigContent := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
%s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
current-context: %s
users:
- name: %s
  user:
    token: %s
`,
		clusterConfig,
		i.config.Azure.TargetCluster.Name,
		username,
		i.config.Azure.TargetCluster.Name,
		i.config.Azure.TargetCluster.Name,
		username,
		bootstrapToken)

	// Write kubeconfig file to the correct location for kubelet
	if err := utilio.WriteFile(KubeletKubeconfigPath, []byte(kubeconfigContent), 0o600); err != nil {
		return fmt.Errorf("failed to create bootstrap kubeconfig file: %w", err)
	}

	i.logger.Info("Bootstrap token kubeconfig created successfully")
	return nil
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
