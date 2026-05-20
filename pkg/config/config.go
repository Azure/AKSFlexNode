package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/logger"
	"k8s.io/apimachinery/pkg/util/validation"
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

// Config represents the complete agent configuration structure.
// It contains Azure-specific settings and agent operational settings.
type Config struct {
	Azure      AzureConfig      `json:"azure"`
	Agent      AgentConfig      `json:"agent"`
	Containerd ContainerdConfig `json:"containerd"`
	Kubernetes KubernetesConfig `json:"kubernetes"`
	CNI        CNIConfig        `json:"cni"`
	Runc       RuncConfig       `json:"runc"`
	Node       NodeConfig       `json:"node"`
	Npd        NPDConfig        `json:"npd"`
}

// AzureConfig holds Azure-specific configuration required for connecting to Azure services.
// All fields except Cloud are required for proper operation.
type AzureConfig struct {
	SubscriptionID   string                  `json:"subscriptionId"`             // Azure subscription ID
	TenantID         string                  `json:"tenantId"`                   // Azure tenant ID
	Cloud            string                  `json:"cloud"`                      // Azure cloud environment (defaults to AzurePublicCloud)
	ServicePrincipal *ServicePrincipalConfig `json:"servicePrincipal,omitempty"` // Optional service principal authentication
	ManagedIdentity  *ManagedIdentityConfig  `json:"managedIdentity,omitempty"`  // Optional managed identity authentication
	BootstrapToken   *BootstrapTokenConfig   `json:"bootstrapToken,omitempty"`   // Optional bootstrap token authentication
	Arc              *ArcConfig              `json:"arc"`                        // Azure Arc machine configuration
	TargetCluster    *TargetClusterConfig    `json:"targetCluster"`              // Target AKS cluster configuration
}

// ServicePrincipalConfig holds Azure service principal authentication configuration.
// When provided, service principal authentication will be used instead of Azure CLI.
type ServicePrincipalConfig struct {
	TenantID     string `json:"tenantId"`     // Azure AD tenant ID
	ClientID     string `json:"clientId"`     // Azure AD application (client) ID
	ClientSecret string `json:"clientSecret"` // Azure AD application client secret
}

// ManagedIdentityConfig holds managed identity authentication configuration.
// It can only be used when the agent is running on an Azure VM with a managed identity assigned.
type ManagedIdentityConfig struct {
	ClientID string `json:"clientId,omitempty"` // Client ID of the managed identity (optional, for VMs with multiple identities)
}

// BootstrapTokenConfig holds Kubernetes bootstrap token authentication configuration.
// Bootstrap tokens provide a lightweight authentication method for node joining.
type BootstrapTokenConfig struct {
	Token string `json:"token"` // Bootstrap token in format: <token-id>.<token-secret>
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
	Enabled       bool              `json:"enabled"`       // Whether to enable Azure Arc registration
	MachineName   string            `json:"machineName"`   // Name for the Arc machine resource
	Tags          map[string]string `json:"tags"`          // Tags to apply to the Arc machine
	ResourceGroup string            `json:"resourceGroup"` // Azure resource group for Arc machine
	Location      string            `json:"location"`      // Azure region for Arc machine
}

// JSONDuration accepts Go duration strings in config JSON while preserving
// compatibility with time.Duration's numeric nanosecond representation.
type JSONDuration time.Duration

func (d *JSONDuration) UnmarshalJSON(data []byte) error {
	var durationString string
	if err := json.Unmarshal(data, &durationString); err == nil {
		duration, err := time.ParseDuration(durationString)
		if err != nil {
			return fmt.Errorf("parse duration: %w", err)
		}
		*d = JSONDuration(duration)
		return nil
	}

	var duration time.Duration
	if err := json.Unmarshal(data, &duration); err != nil {
		return fmt.Errorf("duration must be a string or number")
	}
	*d = JSONDuration(duration)
	return nil
}

func (d JSONDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// AgentConfig holds agent-specific operational configuration.
type AgentConfig struct {
	LogLevel string `json:"logLevel"` // Logging level: debug, info, warning, error
	LogDir   string `json:"logDir"`   // Directory for log files
	// NodeName is resolved from the host hostname when omitted.
	NodeName string `json:"nodeName,omitempty"`

	// MachineReconcileInterval controls how often the daemon re-reads the AKS
	// machine resource when no Kubernetes Node event wakes the controller.
	MachineReconcileInterval JSONDuration `json:"machineReconcileInterval,omitempty"`

	// E2EMode uses the local file-backed AKS machine client. This is only for
	// end-to-end tests until the production AKS RP machine client is available.
	E2EMode bool `json:"e2eMode,omitempty"`

	// MachineOperationMode controls MachineOperation handling. Supported values:
	// "auto" detects Machina CRs, "disable" uses a noop reconciler.
	MachineOperationMode string `json:"machineOperationMode,omitempty"`
}

// KubernetesConfig holds configuration settings for Kubernetes components.
type KubernetesConfig struct {
	Version string `json:"version"`
}

// RuncConfig holds configuration settings for the container runtime (runc).
type RuncConfig struct {
	Version string `json:"version"`
}

// ContainerdConfig holds configuration settings for the containerd runtime.
type ContainerdConfig struct {
	Version string `json:"version"`
}

// NodeConfig holds configuration settings for the Kubernetes node.
type NodeConfig struct {
	MaxPods int               `json:"maxPods"`
	Labels  map[string]string `json:"labels"`
	// Taints to apply at node registration time via --register-with-taints.
	// Each entry must use the kubelet taint format: "key=value:Effect" or "key:Effect"
	// (e.g. "dedicated=infra:NoSchedule", "gpu:NoExecute").
	Taints  []string      `json:"taints,omitempty"`
	Kubelet KubeletConfig `json:"kubelet"`
}

// KubeletConfig holds kubelet-specific configuration settings.
type KubeletConfig struct {
	Verbosity            int    `json:"verbosity"`
	ImageGCHighThreshold int    `json:"imageGCHighThreshold"`
	ImageGCLowThreshold  int    `json:"imageGCLowThreshold"`
	DNSServiceIP         string `json:"dnsServiceIP"` // Cluster DNS service IP (default: 10.0.0.10 for AKS)
	ServerURL            string `json:"serverURL"`    // Kubernetes API server URL
	CACertData           string `json:"caCertData"`   // Base64-encoded CA certificate data
	NodeIP               string `json:"nodeIP"`       // IP address to advertise as the node's primary IP (--node-ip kubelet flag)
}

// CNIPathsConfig holds file system paths related to CNI plugins and configurations.
type CNIConfig struct {
	Version string `json:"version"`
}

// NPDConfig holds configuration settings for the Node Problem Detector (NPD).
type NPDConfig struct {
	Version string `json:"version"`
}

// IsARCEnabled checks if Azure Arc registration is enabled in the configuration.
func (cfg *Config) IsARCEnabled() bool {
	return cfg.Azure.Arc != nil && cfg.Azure.Arc.Enabled
}

// IsSPConfigured checks if service principal authentication is selected.
func (cfg *Config) IsSPConfigured() bool {
	return cfg.Azure.ServicePrincipal != nil
}

// IsMIConfigured checks if managed identity configuration is provided in the configuration.
func (cfg *Config) IsMIConfigured() bool {
	return cfg.Azure.ManagedIdentity != nil
}

// IsBootstrapTokenConfigured checks if bootstrap token authentication is selected.
func (cfg *Config) IsBootstrapTokenConfigured() bool {
	return cfg.Azure.BootstrapToken != nil
}

func (cfg AzureConfig) ResourceManagerEndpoint() (string, error) {
	switch cfg.Cloud {
	case "", "AzurePublicCloud":
		return "https://management.azure.com", nil
	default:
		return "", fmt.Errorf("unsupported Azure cloud %q", cfg.Cloud)
	}
}

func (cfg AzureConfig) ResourceManagerTokenScope() (string, error) {
	endpoint, err := cfg.ResourceManagerEndpoint()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(endpoint, "/") + "/.default", nil
}

// resolveNodeName resolves the Kubernetes Node name once and stores it on the
// config so bootstrap, daemon watches, and lifecycle operations use one value.
func (cfg *Config) resolveNodeName() (string, error) {
	if nodeName := strings.TrimSpace(cfg.Agent.NodeName); nodeName != "" {
		if err := validateNodeName(nodeName); err != nil {
			return "", err
		}
		cfg.Agent.NodeName = nodeName
		return nodeName, nil
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("get host hostname for node name: %w", err)
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "", fmt.Errorf("host hostname is empty")
	}
	if err := validateNodeName(hostname); err != nil {
		return "", fmt.Errorf("host hostname: %w", err)
	}
	cfg.Agent.NodeName = hostname
	return hostname, nil
}

func validateNodeName(name string) error {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("node name %q is not a valid Kubernetes DNS subdomain: %s", name, strings.Join(errs, "; "))
	}
	return nil
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

	return config, nil
}

// DeepCopy returns a copy of the config that does not share mutable sub-objects (maps/pointers)
// with the original.
func (cfg *Config) DeepCopy() *Config {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
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
		c.Agent.MachineReconcileInterval = JSONDuration(defaultMachineReconcileInterval)
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

func (c *Config) validateBootstrapToken() error {
	if !c.IsBootstrapTokenConfigured() {
		return nil
	}
	if err := c.Azure.BootstrapToken.validate(); err != nil {
		return err
	}

	// When using bootstrap token, serverURL and caCertData are required in kubelet config
	// because there's no Azure authentication to fetch them
	if c.Node.Kubelet.ServerURL == "" {
		return fmt.Errorf("node.kubelet.serverURL is required when using bootstrap token authentication")
	}
	if c.Node.Kubelet.CACertData == "" {
		return fmt.Errorf("node.kubelet.caCertData is required when using bootstrap token authentication")
	}

	return nil
}

func (c *AzureConfig) validate() error {
	if c.SubscriptionID == "" {
		return fmt.Errorf("azure.subscriptionId is required")
	}
	if c.TenantID == "" {
		return fmt.Errorf("azure.tenantId is required")
	}
	if c.TargetCluster == nil {
		return fmt.Errorf("azure.targetCluster is required")
	}
	if err := c.TargetCluster.validate(); err != nil {
		return err
	}
	if !validAzureClouds[c.Cloud] {
		return fmt.Errorf("invalid azure.cloud: %s. Valid values are: AzurePublicCloud", c.Cloud)
	}
	if err := c.ServicePrincipal.validate(); err != nil {
		return err
	}
	if err := c.ManagedIdentity.validate(); err != nil {
		return err
	}
	if err := c.BootstrapToken.validate(); err != nil {
		return err
	}
	if err := c.Arc.validate(); err != nil {
		return err
	}
	return nil
}

func (c *ServicePrincipalConfig) validate() error {
	if c == nil {
		return nil
	}
	if c.TenantID == "" {
		return fmt.Errorf("azure.servicePrincipal.tenantId is required when service principal is configured")
	}
	if c.ClientID == "" {
		return fmt.Errorf("azure.servicePrincipal.clientId is required when service principal is configured")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("azure.servicePrincipal.clientSecret is required when service principal is configured")
	}
	return nil
}

func (c *ManagedIdentityConfig) validate() error {
	return nil
}

func (c *TargetClusterConfig) validate() error {
	if c.Location == "" {
		return fmt.Errorf("azure.targetCluster.location is required")
	}
	if c.ResourceID == "" {
		return fmt.Errorf("azure.targetCluster.resourceId is required")
	}
	if err := validateAzureResourceID(c.ResourceID); err != nil {
		return fmt.Errorf("invalid azure.targetCluster.resourceId: %w", err)
	}
	return nil
}

func (c *ArcConfig) validate() error {
	if c == nil {
		return nil
	}
	if !c.Enabled {
		return nil
	}
	if c.MachineName == "" {
		return fmt.Errorf("azure.arc.machineName is required when Arc is enabled")
	}
	if c.ResourceGroup == "" {
		return fmt.Errorf("azure.arc.resourceGroup is required when Arc is enabled")
	}
	if c.Location == "" {
		return fmt.Errorf("azure.arc.location is required when Arc is enabled")
	}
	return nil
}

func (c *BootstrapTokenConfig) validate() error {
	if c == nil {
		return nil
	}
	if !BootstrapTokenPattern.MatchString(c.Token) {
		return fmt.Errorf("invalid bootstrap token format. Expected format: <token-id>.<token-secret> " +
			"where token-id is 6 lowercase alphanumeric characters and token-secret is 16 lowercase alphanumeric characters")
	}
	return nil
}

func (c *AgentConfig) validate() error {
	if _, err := logger.ParseLogLevel(c.LogLevel); err != nil {
		return fmt.Errorf("invalid agent.logLevel: %w", err)
	}
	if c.MachineReconcileInterval < 0 {
		return fmt.Errorf("agent.machineReconcileInterval must be non-negative")
	}
	if c.MachineOperationMode != "" && !validMachineOperationModes[c.MachineOperationMode] {
		return fmt.Errorf("invalid agent.machineOperationMode: %s. Valid values are: auto, disable", c.MachineOperationMode)
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
	c.setDefaults()

	if _, err := c.resolveNodeName(); err != nil {
		return fmt.Errorf("resolve node name: %w", err)
	}

	if err := c.Azure.validate(); err != nil {
		return err
	}
	if err := c.Agent.validate(); err != nil {
		return err
	}

	if err := c.validateExclusiveAuthSettings(); err != nil {
		return err
	}
	if err := c.validateBootstrapToken(); err != nil {
		return fmt.Errorf("invalid bootstrap token configuration: %w", err)
	}

	populateTargetClusterInfoFromConfig(c)

	return nil
}

func (c *Config) validateExclusiveAuthSettings() error {
	authMethodCount := 0
	for _, m := range []bool{c.IsARCEnabled(), c.IsSPConfigured(), c.IsMIConfigured(), c.IsBootstrapTokenConfigured()} {
		if m {
			authMethodCount++
		}
	}
	if authMethodCount == 0 {
		return fmt.Errorf("at least one authentication method must be configured: Arc, Service Principal, Managed Identity, or Bootstrap Token")
	}
	if authMethodCount > 1 {
		return fmt.Errorf("only one authentication method can be enabled at a time: Arc, Service Principal, Managed Identity, or Bootstrap Token")
	}

	return nil
}

// populateTargetClusterInfoFromConfig extracts cluster information from the resource ID
// This function should only be called after validateAzureResourceID confirms the format is correct
// TODO: Deprecate this helper; deriving cluster fields from resourceID/location is wrong
// for cases like custom node resource groups. Use explicit config fields or an Azure lookup instead.
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
