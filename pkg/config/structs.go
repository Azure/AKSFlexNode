package config

import "os"

// Config represents the complete agent configuration structure.
// It contains Azure-specific settings and agent operational settings.
type Config struct {
	Azure      AzureConfig      `json:"azure"`
	Agent      AgentConfig      `json:"agent"`
	Containerd ContainerdConfig `json:"containerd"`
	Kubernetes KubernetesConfig `json:"kubernetes"`
	CNI        CNIConfig        `json:"cni"`
	Runc       RuntimeConfig    `json:"runc"`
	Node       NodeConfig       `json:"node"`
	Paths      PathsConfig      `json:"paths"`
	Npd        NPDConfig        `json:"npd"`
}

// AzureConfig holds Azure-specific configuration required for connecting to Azure services.
// All fields except Cloud are required for proper operation.
type AzureConfig struct {
	SubscriptionID   string                  `json:"subscriptionId"`             // Azure subscription ID
	TenantID         string                  `json:"tenantId"`                   // Azure tenant ID
	Cloud            string                  `json:"cloud"`                      // Azure cloud environment (defaults to AzurePublicCloud)
	ServicePrincipal *ServicePrincipalConfig `json:"servicePrincipal,omitempty"` // Optional service principal authentication
	Arc              *ArcConfig              `json:"arc,omitempty"`              // Azure Arc machine configuration (for VMs outside Azure)
	AzureVm          *AzureVmConfig          `json:"azureVm,omitempty"`          // Azure VM configuration (for VMs running in Azure)
	TargetCluster    *TargetClusterConfig    `json:"targetCluster"`              // Target AKS cluster configuration
}

// AzureVmConfig holds configuration for Azure VMs running natively in Azure (not Arc-enabled).
type AzureVmConfig struct {
	ManagedIdentity *ManagedIdentityConfig `json:"managedIdentity,omitempty"` // Managed identity configuration
}

// ManagedIdentityConfig holds configuration for Azure Managed Identity authentication.
type ManagedIdentityConfig struct {
	ClientID string `json:"clientId,omitempty"` // Client ID of the user-assigned managed identity
}

// ServicePrincipalConfig holds Azure service principal authentication configuration.
// When provided, service principal authentication will be used instead of Azure CLI.
type ServicePrincipalConfig struct {
	TenantID     string `json:"tenantId"`     // Azure AD tenant ID
	ClientID     string `json:"clientId"`     // Azure AD application (client) ID
	ClientSecret string `json:"clientSecret"` // Azure AD application client secret
}

// TargetClusterConfig holds configuration for the target AKS cluster the ARC machine will connect to.
type TargetClusterConfig struct {
	ResourceID        string `json:"resourceId"` // Full resource ID of the target AKS cluster
	Location          string `json:"location"`   // Azure region of the cluster (e.g., "eastus", "westus2")
	Name              string // will be populated from ResourceID
	ResourceGroup     string // will be populated from ResourceID
	SubscriptionID    string // will be populated from ResourceID
	NodeResourceGroup string // will be populated from ResourceID
}

// ArcConfig holds Azure Arc machine configuration for registering the machine with Azure Arc.
type ArcConfig struct {
	MachineName   string            `json:"machineName"`   // Name for the Arc machine resource
	Tags          map[string]string `json:"tags"`          // Tags to apply to the Arc machine
	ResourceGroup string            `json:"resourceGroup"` // Azure resource group for Arc machine
	Location      string            `json:"location"`      // Azure region for Arc machine
}

// AgentConfig holds agent-specific operational configuration.
type AgentConfig struct {
	LogLevel string `json:"logLevel"` // Logging level: debug, info, warning, error
	LogDir   string `json:"logDir"`   // Directory for log files
}

// KubernetesConfig holds configuration settings for Kubernetes components.
type KubernetesConfig struct {
	Version     string `json:"version"`
	URLTemplate string `json:"urlTemplate"`
}

// RuntimeConfig holds configuration settings for the container runtime (runc).
type RuntimeConfig struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// ContainerdConfig holds configuration settings for the containerd runtime.
type ContainerdConfig struct {
	Version        string `json:"version"`
	PauseImage     string `json:"pauseImage"`
	MetricsAddress string `json:"metricsAddress"`
}

// NodeConfig holds configuration settings for the Kubernetes node.
type NodeConfig struct {
	MaxPods int               `json:"maxPods"`
	Labels  map[string]string `json:"labels"`
	Kubelet KubeletConfig     `json:"kubelet"`
}

// KubeletConfig holds kubelet-specific configuration settings.
type KubeletConfig struct {
	KubeReserved         map[string]string `json:"kubeReserved"`
	EvictionHard         map[string]string `json:"evictionHard"`
	ImageGCHighThreshold int               `json:"imageGCHighThreshold"`
	ImageGCLowThreshold  int               `json:"imageGCLowThreshold"`
}

// PathsConfig holds file system paths used by the agent for Kubernetes and CNI configurations.
type PathsConfig struct {
	Kubernetes KubernetesPathsConfig `json:"kubernetes"`
}

// KubernetesPathsConfig holds file system paths related to Kubernetes components.
type KubernetesPathsConfig struct {
	ConfigDir       string `json:"configDir"`
	CertsDir        string `json:"certsDir"`
	ManifestsDir    string `json:"manifestsDir"`
	VolumePluginDir string `json:"volumePluginDir"`
	KubeletDir      string `json:"kubeletDir"`
}

// CNIPathsConfig holds file system paths related to CNI plugins and configurations.
type CNIConfig struct {
	Version string `json:"version"`
}

// NPDConfig holds configuration settings for the Node Problem Detector (NPD).
type NPDConfig struct {
	Version string `json:"version"`
}

// IsSPConfigured checks if service principal credentials are provided in the configuration
func (cfg *Config) IsSPConfigured() bool {
	return cfg.Azure.ServicePrincipal != nil &&
		cfg.Azure.ServicePrincipal.ClientID != "" &&
		cfg.Azure.ServicePrincipal.ClientSecret != "" &&
		cfg.Azure.ServicePrincipal.TenantID != ""
}

// GetArcMachineName returns the Arc machine name from configuration or defaults to the system hostname
func (cfg *Config) GetArcMachineName() string {
	if cfg.Azure.Arc != nil && cfg.Azure.Arc.MachineName != "" {
		return cfg.Azure.Arc.MachineName
	}
	hostname, err := os.Hostname()
	if err == nil {
		return hostname
	}
	return ""
}

// GetTargetClusterName returns the target AKS cluster name from configuration
func (cfg *Config) GetTargetClusterName() string {
	if cfg.Azure.TargetCluster != nil && cfg.Azure.TargetCluster.Name != "" {
		return cfg.Azure.TargetCluster.Name
	}
	return ""
}

// GetTargetClusterSubscriptionID returns the target AKS cluster subscription ID from configuration
func (cfg *Config) GetTargetClusterSubscriptionID() string {
	if cfg.Azure.TargetCluster != nil && cfg.Azure.TargetCluster.SubscriptionID != "" {
		return cfg.Azure.TargetCluster.SubscriptionID
	}
	return ""
}

// GetTargetClusterResourceGroup returns the target AKS cluster resource group from configuration
func (cfg *Config) GetTargetClusterResourceGroup() string {
	if cfg.Azure.TargetCluster != nil && cfg.Azure.TargetCluster.ResourceGroup != "" {
		return cfg.Azure.TargetCluster.ResourceGroup
	}
	return ""
}

// GetTargetClusterLocation returns the target AKS cluster location from configuration
func (cfg *Config) GetTargetClusterLocation() string {
	if cfg.Azure.TargetCluster != nil && cfg.Azure.TargetCluster.Location != "" {
		return cfg.Azure.TargetCluster.Location
	}
	return ""
}

// GetTargetClusterID returns the target AKS cluster resource ID from configuration
func (cfg *Config) GetTargetClusterID() string {
	if cfg.Azure.TargetCluster != nil && cfg.Azure.TargetCluster.ResourceID != "" {
		return cfg.Azure.TargetCluster.ResourceID
	}
	return ""
}

// GetArcLocation returns the Arc machine location from configuration or defaults to the target cluster location
func (cfg *Config) GetArcLocation() string {
	if cfg.Azure.Arc != nil && cfg.Azure.Arc.Location != "" {
		return cfg.Azure.Arc.Location
	}
	return cfg.GetTargetClusterLocation()
}

// GetArcResourceGroup returns the Arc machine resource group from configuration or defaults to the target cluster resource group
func (cfg *Config) GetArcResourceGroup() string {
	// Determine the resource group for Arc registration
	if cfg.Azure.Arc != nil && cfg.Azure.Arc.ResourceGroup != "" {
		return cfg.Azure.Arc.ResourceGroup
	}
	return cfg.GetTargetClusterResourceGroup()
}

// GetArcTags returns the Arc machine tags from configuration or an empty map if none are set
func (cfg *Config) GetArcTags() map[string]string {
	if cfg.Azure.Arc != nil && cfg.Azure.Arc.Tags != nil {
		return cfg.Azure.Arc.Tags
	}
	return map[string]string{}
}

// GetSubscriptionID returns the Azure subscription ID from configuration
func (cfg *Config) GetSubscriptionID() string {
	return cfg.Azure.SubscriptionID
}

// GetTenantID returns the Azure tenant ID from configuration
func (cfg *Config) GetTenantID() string {
	return cfg.Azure.TenantID
}

// GetKubernetesVersion returns the Kubernetes version from configuration
func (cfg *Config) GetKubernetesVersion() string {
	return cfg.Kubernetes.Version
}
