package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/logger"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// ConfigDir is the base directory for AKS Flex Node configuration files
	// installed on the host.
	ConfigDir = "/etc/aks-flex-node"

	// Default configuration values
	DefaultLogDir                   = "/var/log/aks-flex-node"
	defaultLogLevel                 = "info"
	defaultMachineClientMode        = MachineClientModeARM
	defaultMachineOperationMode     = "auto"
	defaultMachineReconcileInterval = 10 * time.Minute
	defaultTargetAgentPoolName      = "aksflexnodes"

	// Machine client modes.
	MachineClientModeARM       = "arm"
	MachineClientModeInCluster = "in-cluster"
	MachineClientModeE2E       = "e2e"

	defaultInClusterMachineEndpointURL = "/api/v1/namespaces/aks-flex-system/services/http:aks-flex-controller:80/proxy"

	// DefaultResourceManagerEndpointURL is the public Azure Resource Manager
	// endpoint used when azure.resourceManagerEndpoint is omitted.
	DefaultResourceManagerEndpointURL = "https://management.azure.com"

	// DefaultResourceManagerAudience is the public Azure Resource Manager token
	// audience used by Azure SDK for Go's ARM pipeline.
	DefaultResourceManagerAudience = "https://management.azure.com"

	// DefaultResourceManagerTokenScope is the OAuth scope for public Azure
	// Resource Manager tokens.
	DefaultResourceManagerTokenScope = DefaultResourceManagerAudience + "/.default"
)

// DefaultInClusterMachineEndpointURL returns the Kubernetes API service-proxy
// path used when agent.machineClient.mode is "in-cluster" and endpointUrl is
// omitted.
func DefaultInClusterMachineEndpointURL() string {
	return defaultInClusterMachineEndpointURL
}

// Config represents the complete agent configuration structure.
// It contains Azure-specific settings and agent operational settings.
type Config struct {
	Azure       AzureConfig       `json:"azure"`
	Agent       AgentConfig       `json:"agent"`
	Components  ComponentsConfig  `json:"components"`
	Bootstrap   BootstrapConfig   `json:"bootstrap"`
	Networking  NetworkingConfig  `json:"networking"`
	Node        NodeConfig        `json:"node"`
	Npd         NPDConfig         `json:"npd"`
	HostRouting HostRoutingConfig `json:"hostRouting"`
}

// AzureConfig holds Azure-specific configuration required for connecting to Azure services.
type AzureConfig struct {
	SubscriptionID             string                  `json:"subscriptionId"`                    // Azure subscription ID; defaults from targetCluster.resourceId when omitted
	TenantID                   string                  `json:"tenantId"`                          // Azure tenant ID
	Cloud                      string                  `json:"cloud,omitempty"`                   // Optional Azure cloud label used when resourceManagerEndpoint is omitted
	ResourceManagerEndpointURL string                  `json:"resourceManagerEndpoint,omitempty"` // Azure Resource Manager endpoint; defaults to https://management.azure.com
	ServicePrincipal           *ServicePrincipalConfig `json:"servicePrincipal,omitempty"`        // Optional service principal authentication
	ManagedIdentity            *ManagedIdentityConfig  `json:"managedIdentity,omitempty"`         // Optional managed identity authentication
	BootstrapToken             *BootstrapTokenConfig   `json:"bootstrapToken,omitempty"`          // Optional bootstrap token authentication
	Arc                        *ArcConfig              `json:"arc"`                               // Azure Arc machine configuration
	TargetCluster              *TargetClusterConfig    `json:"targetCluster"`                     // Target AKS cluster configuration
	TargetAgentPoolName        string                  `json:"targetAgentPoolName"`               // Target AKS agent pool for FlexNode machines
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
	ResourceID     string `json:"resourceId"` // Full resource ID of the target AKS cluster
	Location       string `json:"location"`   // Azure region of the cluster (e.g., "eastus", "westus2")
	Name           string // will be populated from ResourceID
	ResourceGroup  string // will be populated from ResourceID
	SubscriptionID string // will be populated from ResourceID
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

	// MachineClient selects how the agent reads the AKS machine resource.
	MachineClient MachineClientConfig `json:"machineClient,omitempty"`

	// MachineReconcileInterval controls how often the daemon re-reads the AKS
	// machine resource when no Kubernetes Node event wakes the controller.
	MachineReconcileInterval JSONDuration `json:"machineReconcileInterval,omitempty"`

	// RequireMachineRegistration fails bootstrap if the AKS machine resource
	// cannot be read or created. When false, registration is best-effort.
	RequireMachineRegistration bool `json:"requireMachineRegistration,omitempty"`

	// MachineOperationMode controls MachineOperation handling. Supported values:
	// "auto" detects Machina CRs, "disable" uses a noop reconciler.
	MachineOperationMode string `json:"machineOperationMode,omitempty"`
}

// MachineClientConfig configures the machine resource backend.
type MachineClientConfig struct {
	// Mode selects the machine backend: "arm", "in-cluster", or "e2e".
	Mode string `json:"mode,omitempty"`

	// EndpointURL optionally points at the selected backend. In arm mode it is a
	// dev-test ARM proxy URL. In in-cluster mode it is the Kubernetes API service
	// proxy path or absolute URL for the read-only machine endpoint. E2E mode uses
	// the built-in local file path unless a future test backend consumes this.
	EndpointURL string `json:"endpointUrl,omitempty"`
}

// ComponentsConfig is the AKS RP component version contract used by the agent
// at runtime.
type ComponentsConfig struct {
	Kubernetes   string `json:"kubernetes,omitempty"`
	Containerd   string `json:"containerd,omitempty"`
	Runc         string `json:"runc,omitempty"`
	SandboxImage string `json:"sandboxImage,omitempty"`
}

// BootstrapConfig holds bootstrap settings that are not Kubernetes component
// versions.
type BootstrapConfig struct {
	// OCIImage is the nspawn rootfs OCI image used by the shared agent library.
	// When empty, the agent uses its built-in default image selection.
	OCIImage string `json:"ociImage,omitempty"`

	// OfflineArtifacts points at a complete offline binary artifact source.
	// When source is set, bootstrap resolves Kubernetes, containerd, runc, CNI,
	// crictl, and optional sandbox image archive artifacts from this source.
	OfflineArtifacts OfflineArtifactsConfig `json:"offlineArtifacts,omitempty"`

	// AdditionalHostDevices lists extra host device nodes under /dev to expose to
	// the nspawn machine in addition to devices discovered by the shared agent.
	AdditionalHostDevices []string `json:"additionalHostDevices,omitempty"`
}

// OfflineArtifactsConfig mirrors Unbounded's OfflineArtifacts bootstrap
// setting in the AKS Flex public config shape.
type OfflineArtifactsConfig struct {
	// Source is a Go template string that resolves to an absolute filesystem
	// path, file:// URL, or oci:// artifact reference. The template may use
	// .KubernetesVersion and .KubernetesVersionNoV.
	Source string `json:"source,omitempty"`
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
	ClusterFQDN          string `json:"clusterFQDN,omitempty"` // Kubernetes API server FQDN from AKS RP bootstrap data
	CACertData           string `json:"caCertData"`            // Base64-encoded CA certificate data
	NodeIP               string `json:"nodeIP"`                // IP address to advertise as the node's primary IP (--node-ip kubelet flag)
}

// NetworkingConfig is the AKS RP networking contract used by the agent at runtime.
type NetworkingConfig struct {
	DNSServiceIP string `json:"dnsServiceIP,omitempty"` // Cluster DNS service IP (default: 10.0.0.10 for AKS)
	CNIVersion   string `json:"cniVersion,omitempty"`
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

// resolveNodeName resolves the Kubernetes Node name once and stores it on the
// config so bootstrap, daemon watches, and lifecycle operations use one value.
func (cfg *Config) resolveNodeName(hostnameFunc func() (string, error)) (string, error) {
	if nodeName := strings.TrimSpace(cfg.Agent.NodeName); nodeName != "" {
		if err := validateNodeName(nodeName); err != nil {
			return "", err
		}
		cfg.Agent.NodeName = nodeName
		return nodeName, nil
	}
	hostname, err := hostnameFunc()
	if err != nil {
		return "", fmt.Errorf("get host hostname for node name: %w", err)
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return "", fmt.Errorf("host hostname is empty")
	}
	if err := validateNodeName(hostname); err != nil {
		return "", fmt.Errorf("host hostname: %w; set agent.nodeName to a valid lowercase Kubernetes node name", err)
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
	if err := adaptLegacyConfigData(data, config); err != nil {
		return nil, fmt.Errorf("adapt legacy config data: %w", err)
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
	c.setAzureDefaults()
	c.setAgentDefaults()
	c.setNodeDefaults()
	c.setRuncDefaults()
	c.setNpdDefaults()
}

func (c *Config) setAzureDefaults() {
	if endpoint := strings.TrimSpace(c.Azure.ResourceManagerEndpointURL); endpoint == "" {
		c.Azure.ResourceManagerEndpointURL = resourceManagerEndpointFromCloud(c.Azure.Cloud)
	} else {
		c.Azure.ResourceManagerEndpointURL = strings.TrimRight(endpoint, "/")
	}
	c.Azure.TargetAgentPoolName = strings.TrimSpace(c.Azure.TargetAgentPoolName)
	if c.Azure.TargetAgentPoolName == "" {
		c.Azure.TargetAgentPoolName = defaultTargetAgentPoolName
	}
}

func resourceManagerEndpointFromCloud(cloud string) string {
	switch strings.TrimSpace(cloud) {
	case "AzureUSGovernment", "AzureGovernmentCloud":
		return "https://management.usgovcloudapi.net"
	case "AzureChinaCloud":
		return "https://management.chinacloudapi.cn"
	case "", "AzurePublicCloud":
		return DefaultResourceManagerEndpointURL
	default:
		return DefaultResourceManagerEndpointURL
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
	if c.Agent.MachineClient.Mode == "" {
		c.Agent.MachineClient.Mode = defaultMachineClientMode
	}
	if c.Agent.MachineClient.Mode == MachineClientModeInCluster && c.Agent.MachineClient.EndpointURL == "" {
		c.Agent.MachineClient.EndpointURL = defaultInClusterMachineEndpointURL
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
	if c.Networking.DNSServiceIP == "" {
		c.Networking.DNSServiceIP = "10.0.0.10"
	}
}

func (c *Config) setRuncDefaults() {
	// Offline artifact manifests are the source of truth for runtime versions.
	// Do not synthesize a runc version that would conflict with the manifest.
	if c.Bootstrap.OfflineArtifacts.Source != "" {
		return
	}
	if c.Components.Runc == "" {
		c.Components.Runc = "1.1.12"
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

	// When using bootstrap token, clusterFQDN and caCertData are required in kubelet config
	// because there's no Azure authentication to fetch them
	if c.APIServerURL() == "" {
		return fmt.Errorf("node.kubelet.clusterFQDN is required when using bootstrap token authentication")
	}
	if err := validateAbsoluteHTTPSURL(c.APIServerURL(), httpsURLValidationOptions{
		fieldName: "kube-apiserver URL",
		allowPort: true,
	}); err != nil {
		return fmt.Errorf("node.kubelet.clusterFQDN must resolve to a valid kube-apiserver URL: %w", err)
	}
	if c.Node.Kubelet.CACertData == "" {
		return fmt.Errorf("node.kubelet.caCertData is required when using bootstrap token authentication")
	}

	return nil
}

// APIServerURL returns the kube-apiserver URL derived from the RP cluster FQDN.
func (c *Config) APIServerURL() string {
	if c == nil {
		return ""
	}
	return serverURLFromClusterFQDN(c.Node.Kubelet.ClusterFQDN)
}

func (c *AzureConfig) validate() error {
	if c.requiresTenantID() && c.TenantID == "" {
		return fmt.Errorf("azure.tenantId is required")
	}
	if c.TargetCluster == nil {
		return fmt.Errorf("azure.targetCluster is required")
	}
	if err := c.TargetCluster.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.TargetAgentPoolName) == "" {
		return fmt.Errorf("azure.targetAgentPoolName is required")
	}
	if c.SubscriptionID == "" {
		return fmt.Errorf("azure.subscriptionId is required")
	}
	if err := c.validateResourceManagerEndpointURL(); err != nil {
		return err
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

func (c *AzureConfig) requiresTenantID() bool {
	if c == nil || c.Arc == nil {
		return false
	}
	return c.Arc.Enabled
}

func (c *AzureConfig) validateResourceManagerEndpointURL() error {
	endpoint := strings.TrimSpace(c.ResourceManagerEndpointURL)
	if endpoint == "" {
		return fmt.Errorf("azure.resourceManagerEndpoint is required")
	}
	return validateAbsoluteHTTPSURL(endpoint, httpsURLValidationOptions{
		fieldName: "azure.resourceManagerEndpoint",
		allowPort: false,
	})
}

type httpsURLValidationOptions struct {
	fieldName string
	allowPort bool
}

func validateAbsoluteHTTPSURL(rawURL string, opts httpsURLValidationOptions) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute https URL", opts.fieldName)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%s must use https", opts.fieldName)
	}
	if parsed.User != nil {
		if opts.allowPort {
			return fmt.Errorf("%s must not include user info", opts.fieldName)
		}
		return fmt.Errorf("%s must not include user info or port", opts.fieldName)
	}
	if !opts.allowPort && parsed.Port() != "" {
		return fmt.Errorf("%s must not include user info or port", opts.fieldName)
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must not include a path, query, or fragment", opts.fieldName)
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

func (c *BootstrapConfig) validate() error {
	if err := agentconfig.ValidateAdditionalHostDevices(c.AdditionalHostDevices); err != nil {
		return fmt.Errorf("invalid bootstrap.additionalHostDevices: %w", err)
	}

	return nil
}

func (c *AgentConfig) validate() error {
	if _, err := logger.ParseLogLevel(c.LogLevel); err != nil {
		return fmt.Errorf("invalid agent.logLevel: %w", err)
	}
	if err := c.MachineClient.validate(); err != nil {
		return err
	}
	if c.MachineReconcileInterval < 0 {
		return fmt.Errorf("agent.machineReconcileInterval must be non-negative")
	}
	if c.MachineOperationMode != "" && !validMachineOperationModes[c.MachineOperationMode] {
		return fmt.Errorf("invalid agent.machineOperationMode: %s. Valid values are: auto, disable", c.MachineOperationMode)
	}
	return nil
}

var validMachineClientModes = map[string]bool{
	MachineClientModeARM:       true,
	MachineClientModeInCluster: true,
	MachineClientModeE2E:       true,
}

var validMachineOperationModes = map[string]bool{
	"auto":    true,
	"disable": true,
}

func (c MachineClientConfig) validate() error {
	if c.Mode != "" && !validMachineClientModes[c.Mode] {
		return fmt.Errorf("invalid agent.machineClient.mode: %s. Valid values are: arm, in-cluster, e2e", c.Mode)
	}
	if c.EndpointURL == "" {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(c.EndpointURL))
	if err != nil {
		return fmt.Errorf("invalid agent.machineClient.endpointUrl: %w", err)
	}
	switch c.Mode {
	case MachineClientModeARM:
		if parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("invalid agent.machineClient.endpointUrl: arm mode requires an absolute URL")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("invalid agent.machineClient.endpointUrl: scheme must be http or https")
		}
	case MachineClientModeInCluster:
		if parsed.Scheme == "" {
			if !strings.HasPrefix(c.EndpointURL, "/") {
				return fmt.Errorf("invalid agent.machineClient.endpointUrl: in-cluster mode requires an absolute URL or absolute Kubernetes API path")
			}
			return nil
		}
		if parsed.Host == "" {
			return fmt.Errorf("invalid agent.machineClient.endpointUrl: in-cluster absolute URL requires a host")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("invalid agent.machineClient.endpointUrl: scheme must be http or https")
		}
	case MachineClientModeE2E:
		return fmt.Errorf("invalid agent.machineClient.endpointUrl: e2e mode does not support endpointUrl")
	}
	return nil
}

func (c *Config) validate() error {
	c.setDefaults()

	if _, err := c.resolveNodeName(os.Hostname); err != nil {
		return fmt.Errorf("resolve node name: %w", err)
	}

	populateTargetClusterInfoFromConfig(c)

	if err := c.Azure.validate(); err != nil {
		return err
	}
	if err := c.Agent.validate(); err != nil {
		return err
	}
	if err := c.Bootstrap.validate(); err != nil {
		return err
	}

	if err := c.validateAuthSettings(); err != nil {
		return err
	}
	if err := c.validateBootstrapToken(); err != nil {
		return fmt.Errorf("invalid bootstrap token configuration: %w", err)
	}

	return nil
}

func (c *Config) validateAuthSettings() error {
	armAuthMethodCount := 0
	for _, m := range []bool{c.IsARCEnabled(), c.IsSPConfigured(), c.IsMIConfigured()} {
		if m {
			armAuthMethodCount++
		}
	}
	if armAuthMethodCount == 0 && !c.IsBootstrapTokenConfigured() {
		return fmt.Errorf("at least one authentication method must be configured: Arc, Service Principal, Managed Identity, or Bootstrap Token")
	}
	if armAuthMethodCount > 1 {
		return fmt.Errorf("only one Azure authentication method can be enabled at a time: Arc, Service Principal, or Managed Identity")
	}

	return nil
}

// populateTargetClusterInfoFromConfig extracts cluster information from the resource ID.
// Invalid or absent resource IDs are ignored so validation can return the canonical error.
// TODO: Deprecate this helper; use explicit config fields or an Azure lookup instead.
func populateTargetClusterInfoFromConfig(cfg *Config) {
	if cfg == nil || cfg.Azure.TargetCluster == nil {
		return
	}
	matches := AKSClusterResourceIDPattern.FindStringSubmatch(cfg.Azure.TargetCluster.ResourceID)
	if len(matches) < 4 {
		return
	}

	subscriptionID := matches[1]
	resourceGroupName := matches[2]
	clusterName := matches[3]

	cfg.Azure.TargetCluster.Name = clusterName
	cfg.Azure.TargetCluster.ResourceGroup = resourceGroupName
	cfg.Azure.TargetCluster.SubscriptionID = subscriptionID
	if cfg.Azure.SubscriptionID == "" {
		cfg.Azure.SubscriptionID = subscriptionID
	}
}

// HostRoutingConfig groups host-level routing tasks that run before the nspawn
// machine starts.
type HostRoutingConfig struct {
	// StaticRoutes installs explicit IPv4 routes to prevent provider-installed
	// connected routes (e.g. Azure IB /16 on ND-isr SKUs) from shadowing
	// cluster CIDRs.
	StaticRoutes StaticRoutesConfig `json:"staticRoutes"`

	// RouteOverlap checks that the expected CIDRs all route via the default
	// outbound interface. Use this to catch unmitigated routing overlaps at
	// boot time instead of hours after a node silently misbehaves.
	RouteOverlap RouteOverlapConfig `json:"routeOverlap"`
}

// StaticRoutesConfig holds the spec for the static-routes systemd oneshot.
type StaticRoutesConfig struct {
	// Enabled must be set to true when routes are provided. This explicit
	// opt-in prevents accidental route injection.
	Enabled bool `json:"enabled"`

	// Routes is the list of IPv4 static routes to install before kubelet starts.
	Routes []StaticRoute `json:"routes,omitempty"`
}

// StaticRoute describes a single IPv4 route to install via `ip -4 route replace`.
type StaticRoute struct {
	// Destination is an IPv4 CIDR, e.g. "172.16.1.0/24". Required.
	Destination string `json:"destination"`

	// Gateway is the next-hop IPv4 address. When empty the script resolves the
	// default gateway on Dev at boot time (with a bounded retry for DHCP races).
	Gateway string `json:"gateway,omitempty"`

	// Dev is the outbound interface (e.g. "eth0"). When empty the script
	// resolves the IPv4 default route's outbound interface at boot time.
	Dev string `json:"dev,omitempty"`

	// Metric sets the route metric for tie-breaking. 0 means use kernel default.
	Metric uint32 `json:"metric,omitempty"`
}

// RouteOverlapConfig holds the spec for the check-route-overlap systemd oneshot.
type RouteOverlapConfig struct {
	// ExpectedCIDRs is the list of IPv4 CIDRs that must route via the default
	// outbound interface. Typically pod CIDR + service CIDR + API server prefix.
	ExpectedCIDRs []string `json:"expectedCidrs,omitempty"`

	// Mode controls behaviour on overlap detection.
	// "WARN" (default): log the overlap and let kubelet start.
	// "STRICT": log the overlap and prevent kubelet from starting.
	Mode string `json:"mode,omitempty"`
}
