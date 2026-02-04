package config

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/spf13/viper"
)

const (
	// Default configuration values
	defaultConfigPath = "/etc/aks-flex-node/config.json"
	defaultLogDir     = "/var/log/aks-flex-node"
	defaultLogLevel   = "info"
	defaultAzureCloud = "AzurePublicCloud"

	// Environment variable prefix
	envPrefix = "AKS_NODE_CONTROLLER"
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

// LoadConfig loads configuration from a JSON file and environment variables.
// The configPath parameter is required and cannot be empty.
// Environment variables can override config file values using the AKS_NODE_CONTROLLER_ prefix.
// For example: AKS_NODE_CONTROLLER_AZURE_LOCATION=westus2
func LoadConfig(configPath string) (*Config, error) {
	// Require config path to be specified
	if configPath == "" {
		return nil, fmt.Errorf("config file path is required")
	}

	// Set up viper
	v := viper.New()
	v.SetConfigType("json")
	v.AutomaticEnv()
	v.SetEnvPrefix(envPrefix)

	// Load the specified config file
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", configPath, err)
	}

	// Unmarshal config
	config := &Config{}
	if err := v.Unmarshal(config); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	// Set defaults for any missing values
	config.SetDefaults()

	// Validate the configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	populateTargetClusterInfoFromConfig(config)

	// Set the singleton instance
	configMutex.Lock()
	defer configMutex.Unlock()
	configInstance = config

	return config, nil
}

// SetDefaults sets default values for any missing configuration fields
func (c *Config) SetDefaults() {
	c.setAzureCloudDefaults()
	c.setAgentDefaults()
	c.setPathDefaults()
	c.setNodeDefaults()
	c.setContainerdDefaults()
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
		c.Agent.LogDir = defaultLogDir
	}
}

func (c *Config) setPathDefaults() {
	// Set default paths for Kubernetes components if not provided
	if c.Paths.Kubernetes.ConfigDir == "" {
		c.Paths.Kubernetes.ConfigDir = "/etc/kubernetes"
	}
	if c.Paths.Kubernetes.CertsDir == "" {
		c.Paths.Kubernetes.CertsDir = "/etc/kubernetes/certs"
	}
	if c.Paths.Kubernetes.ManifestsDir == "" {
		c.Paths.Kubernetes.ManifestsDir = "/etc/kubernetes/manifests"
	}
	if c.Paths.Kubernetes.VolumePluginDir == "" {
		c.Paths.Kubernetes.VolumePluginDir = "/etc/kubernetes/volumeplugins"
	}
	if c.Paths.Kubernetes.KubeletDir == "" {
		c.Paths.Kubernetes.KubeletDir = "/var/lib/kubelet"
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
	// Initialize default kubelet resource reservations if not provided
	if c.Node.Kubelet.KubeReserved == nil {
		c.Node.Kubelet.KubeReserved = make(map[string]string)
	}
	if c.Node.Kubelet.EvictionHard == nil {
		c.Node.Kubelet.EvictionHard = make(map[string]string)
	}
}

func (c *Config) setContainerdDefaults() {
	if c.Containerd.MetricsAddress == "" {
		c.Containerd.MetricsAddress = "0.0.0.0:10257"
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

// validateAzureResourceID validates the format of an AKS cluster resource ID using regex pattern matching
func validateAzureResourceID(resourceID string) error {
	// Check AKS cluster resource ID format
	if !AKSClusterResourceIDPattern.MatchString(resourceID) {
		return fmt.Errorf("invalid AKS cluster resource ID format. Expected format:" +
			"/subscriptions/{subscription-id}/resourceGroups/{resource-group}/providers/Microsoft.ContainerService/managedClusters/{cluster-name}")
	}

	return nil
}

// validLogLevels defines the allowed logging levels for the agent
var validLogLevels = map[string]bool{
	"debug":   true,
	"info":    true,
	"warning": true,
	"error":   true,
}

// validAzureClouds defines the supported Azure cloud environments
// Currently only Azure Public Cloud is supported
var validAzureClouds = map[string]bool{
	"AzurePublicCloud": true,
}

// Validate validates the configuration and ensures all required fields are set
func (c *Config) Validate() error {
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

	// Validate log level
	if !validLogLevels[c.Agent.LogLevel] {
		return fmt.Errorf("invalid agent.logLevel: %s. Valid values are: debug, info, warning, error", c.Agent.LogLevel)
	}

	// Validate authentication configuration - Arc and MSI cannot coexist
	if c.IsARCEnabled() && c.IsMIConfigured() {
		return fmt.Errorf("invalid configuration: Arc and ManagedIdentity cannot be configured together. " +
			"Choose either Arc (with arc.enabled: true) or ManagedIdentity (with managedIdentity config), but not both")
	}

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
