package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/logger"
)

const (
	// ConfigDir is the base directory for AKS Flex Node configuration files
	// installed on the host.
	ConfigDir = "/etc/aks-flex-node"

	// Default configuration values
	DefaultLogDir                   = "/var/log/aks-flex-node"
	defaultLogLevel                 = "info"
	defaultMachineOperationMode     = "auto"
	defaultAzureCloud               = "AzurePublicCloud"
	defaultMachineReconcileInterval = 10 * time.Minute
)

// Singleton instance for configuration
var (
	configInstance *Config
	configMutex    sync.RWMutex
)

// GetConfig returns the singleton configuration instance.
// Returns nil if configuration has not been loaded yet. Use LoadConfig() first.
// This function is thread-safe and handles concurrent access correctly.
func GetConfig() *Config {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return configInstance
}

// LoadConfig loads configuration from a JSON file.
// The configPath parameter is required and cannot be empty.
func LoadConfig(configPath string) (*Config, error) {
	// Require config path to be specified
	if configPath == "" {
		return nil, fmt.Errorf("config file path is required")
	}

	data, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", configPath, err)
	}

	config := &Config{}
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	// Set the singleton instance
	configMutex.Lock()
	defer configMutex.Unlock()
	configInstance = config

	return config, nil
}

func (c *Config) setDefaults() {
	c.setAzureCloudDefaults()
	c.setAgentDefaults()
	c.setNodeDefaults()
	c.setRuncDefaults()
	c.setNpdDefaults()
}

func (c *Config) setAzureCloudDefaults() {
	// Set default Azure cloud if not provided
	if c.Azure.Cloud == "" {
		c.Azure.Cloud = defaultAzureCloud
	}
}

func (c *Config) setAgentDefaults() {
	// Set default agent configuration if not provided
	if c.Agent.LogLevel == "" {
		c.Agent.LogLevel = defaultLogLevel
	}
	if c.Agent.LogDir == "" {
		c.Agent.LogDir = DefaultLogDir
	}
	if c.Agent.MachineReconcileInterval == 0 {
		c.Agent.MachineReconcileInterval = defaultMachineReconcileInterval
	}
	if c.Agent.MachineOperationMode == "" {
		c.Agent.MachineOperationMode = defaultMachineOperationMode
	}
}

func (c *Config) setNodeDefaults() {
	// Set default node configuration if not provided
	if c.Node.MaxPods == 0 {
		c.Node.MaxPods = 110 // Default Kubernetes node pod limit
	}

	// set default node labels if not provided
	if c.Node.Labels == nil {
		c.Node.Labels = make(map[string]string)
	}
	// Mark node as unmanaged by cloud controller manager by default, otherwise ccm will delete this node if node is not ready
	// doc: https://cloud-provider-azure.sigs.k8s.io/topics/cross-resource-group-nodes/#unmanaged-nodes
	c.Node.Labels["kubernetes.azure.com/managed"] = "false"

	// Set default kubelet configuration if not provided
	if c.Node.Kubelet.Verbosity == 0 {
		c.Node.Kubelet.Verbosity = 2
	}
	if c.Node.Kubelet.ImageGCHighThreshold == 0 {
		c.Node.Kubelet.ImageGCHighThreshold = 85 // start GC when disk usage > 85%
	}
	if c.Node.Kubelet.ImageGCLowThreshold == 0 {
		c.Node.Kubelet.ImageGCLowThreshold = 80 // stop GC when disk usage < 80%
	}
	// Set default DNS service IP if not provided
	// Note: This default assumes the standard AKS service CIDR (10.0.0.0/16)
	// Clusters with custom service CIDRs should specify this value explicitly
	if c.Node.Kubelet.DNSServiceIP == "" {
		c.Node.Kubelet.DNSServiceIP = "10.0.0.10"
	}
}

func (c *Config) setRuncDefaults() {
	// Set default runc configuration if not provided
	if c.Runc.Version == "" {
		c.Runc.Version = "1.1.12"
	}
}

func (c *Config) setNpdDefaults() {
	// Set default NPD configuration if not provided
	if c.Npd.Version == "" {
		c.Npd.Version = "v1.35.1"
	}
}

// AKSClusterResourceIDPattern is AKS cluster resource ID regex pattern with capture groups
// Format: /subscriptions/{subscription-id}/resourceGroups/{resource-group}/providers/Microsoft.ContainerService/managedClusters/{cluster-name}
// Pattern is case insensitive to handle variations in Azure resource path casing
var AKSClusterResourceIDPattern = regexp.MustCompile(`(?i)^/subscriptions/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/resourcegroups/([a-zA-Z0-9_\-\.]+)/providers/microsoft\.containerservice/managedclusters/([a-zA-Z0-9_\-\.]+)$`)

// BootstrapTokenPattern is the regex pattern for Kubernetes bootstrap tokens
// Format: <token-id>.<token-secret> where token-id is 6 chars [a-z0-9] and token-secret is 16 chars [a-z0-9]
var BootstrapTokenPattern = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)

// validateAzureResourceID validates the format of an AKS cluster resource ID using regex pattern matching
func validateAzureResourceID(resourceID string) error {
	// Check AKS cluster resource ID format
	if !AKSClusterResourceIDPattern.MatchString(resourceID) {
		return fmt.Errorf("invalid AKS cluster resource ID format. Expected format:" +
			"/subscriptions/{subscription-id}/resourceGroups/{resource-group}/providers/Microsoft.ContainerService/managedClusters/{cluster-name}")
	}

	return nil
}

// validateBootstrapToken validates the bootstrap token configuration
func validateBootstrapToken(cfg *Config) error {
	tokenCfg := cfg.Azure.BootstrapToken
	if tokenCfg == nil {
		return fmt.Errorf("bootstrap token configuration is nil")
	}

	// Validate token format
	if !BootstrapTokenPattern.MatchString(tokenCfg.Token) {
		return fmt.Errorf("invalid bootstrap token format. Expected format: <token-id>.<token-secret> " +
			"where token-id is 6 lowercase alphanumeric characters and token-secret is 16 lowercase alphanumeric characters")
	}

	// When using bootstrap token, serverURL and caCertData are required in kubelet config
	// because there's no Azure authentication to fetch them
	if cfg.Node.Kubelet.ServerURL == "" {
		return fmt.Errorf("node.kubelet.serverURL is required when using bootstrap token authentication")
	}
	if cfg.Node.Kubelet.CACertData == "" {
		return fmt.Errorf("node.kubelet.caCertData is required when using bootstrap token authentication")
	}

	return nil
}

// validAzureClouds defines the supported Azure cloud environments
// Currently only Azure Public Cloud is supported
var validAzureClouds = map[string]bool{
	"AzurePublicCloud": true,
}

var validMachineOperationModes = map[string]bool{
	"auto":    true,
	"disable": true,
}

func (c *Config) validate() error {
	// encoding/json deserializes "managedIdentity": {} into a non-nil pointer, so
	// presence can be detected directly.
	c.isMIExplicitlySet = c.Azure.ManagedIdentity != nil

	c.setDefaults()

	if _, err := c.resolveNodeName(); err != nil {
		return fmt.Errorf("resolve node name: %w", err)
	}

	// Validate required Azure configuration (core requirements for Arc discovery)
	if c.Azure.SubscriptionID == "" {
		return fmt.Errorf("azure.subscriptionId is required")
	}
	if c.Azure.TenantID == "" {
		return fmt.Errorf("azure.tenantId is required")
	}
	if c.Azure.TargetCluster.Location == "" {
		return fmt.Errorf("azure.targetCluster.location is required")
	}
	if c.Azure.TargetCluster.ResourceID == "" {
		return fmt.Errorf("azure.targetCluster.resourceId is required")
	}

	// Validate Azure resource ID format
	if err := validateAzureResourceID(c.Azure.TargetCluster.ResourceID); err != nil {
		return fmt.Errorf("invalid azure.targetCluster.resourceId: %w", err)
	}

	// Validate Azure cloud
	if !validAzureClouds[c.Azure.Cloud] {
		return fmt.Errorf("invalid azure.cloud: %s. Valid values are: AzurePublicCloud", c.Azure.Cloud)
	}

	if _, err := logger.ParseLogLevel(c.Agent.LogLevel); err != nil {
		return fmt.Errorf("invalid agent.logLevel: %w", err)
	}
	if c.Agent.MachineReconcileInterval < 0 {
		return fmt.Errorf("agent.machineReconcileInterval must be non-negative")
	}
	if c.Agent.MachineOperationMode != "" && !validMachineOperationModes[c.Agent.MachineOperationMode] {
		return fmt.Errorf("invalid agent.machineOperationMode: %s. Valid values are: auto, disable", c.Agent.MachineOperationMode)
	}

	// Validate authentication configuration - ensure mutual exclusivity
	authMethodCount := 0
	if c.IsARCEnabled() {
		authMethodCount++
	}
	if c.IsSPConfigured() {
		authMethodCount++
	}
	if c.IsMIConfigured() {
		authMethodCount++
	}
	if c.IsBootstrapTokenConfigured() {
		authMethodCount++
	}

	if authMethodCount == 0 {
		return fmt.Errorf("at least one authentication method must be configured: Arc, Service Principal, Managed Identity, or Bootstrap Token")
	}
	if authMethodCount > 1 {
		return fmt.Errorf("only one authentication method can be enabled at a time: Arc, Service Principal, Managed Identity, or Bootstrap Token")
	}

	// Validate bootstrap token if configured
	if c.IsBootstrapTokenConfigured() {
		if err := validateBootstrapToken(c); err != nil {
			return fmt.Errorf("invalid bootstrap token configuration: %w", err)
		}
	}

	populateTargetClusterInfoFromConfig(c)

	return nil
}

// populateTargetClusterInfoFromConfig extracts cluster information from the resource ID
// This function should only be called after validateAzureResourceID confirms the format is correct
func populateTargetClusterInfoFromConfig(cfg *Config) {
	matches := AKSClusterResourceIDPattern.FindStringSubmatch(cfg.Azure.TargetCluster.ResourceID)
	if len(matches) < 4 {
		// This should not happen if validation occurred first, but handle gracefully
		return
	}

	subscriptionID := matches[1]
	resourceGroupName := matches[2]
	clusterName := matches[3]

	// AKS node resource group follows the pattern: MC_{cluster-resource-group}_{cluster-name}_{location}
	mcResourceGroup := fmt.Sprintf("MC_%s_%s_%s",
		resourceGroupName,
		clusterName,
		cfg.Azure.TargetCluster.Location)

	cfg.Azure.TargetCluster.Name = clusterName
	cfg.Azure.TargetCluster.ResourceGroup = resourceGroupName
	cfg.Azure.TargetCluster.SubscriptionID = subscriptionID
	cfg.Azure.TargetCluster.NodeResourceGroup = mcResourceGroup
}
