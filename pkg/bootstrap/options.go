package bootstrap

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

const (
	defaultConfigPath              = "/etc/aks-flex-node/config.json"
	defaultInstallDir              = "/usr/local/bin"
	defaultRepository              = "Azure/AKSFlexNode"
	defaultResourceManagerEndpoint = "https://management.azure.com"
	defaultAuthorityHost           = "https://login.microsoftonline.com"
	defaultBootstrapDataAPIVersion = "2026-05-02-preview"
	updateGuardEnvironment         = "AKS_FLEX_NODE_AGENT_UPDATE_APPLIED"
)

type Options struct {
	AuthMode                string
	MSIClientID             string
	SPTenantID              string
	SPClientID              string
	SPClientSecret          string
	SPClientSecretFile      string
	SPClientCertificateFile string

	FetchBootstrapData      bool
	BootstrapDataAPIVersion string
	ClusterResourceID       string
	AgentPoolName           string
	ResourceManagerEndpoint string
	AuthorityHost           string

	BootstrapOCIImage               string
	BootstrapOfflineArtifactsSource string
	ConfigOverrides                 []string
	BaseConfigPath                  string
	ConfigPath                      string

	AgentURL     string
	AgentVersion string
	AgentSHA256  string
	InstallDir   string
}

func DefaultOptions() Options {
	return Options{
		BootstrapDataAPIVersion: defaultBootstrapDataAPIVersion,
		AuthorityHost:           defaultAuthorityHost,
		ConfigPath:              defaultConfigPath,
		InstallDir:              defaultInstallDir,
	}
}

func (o *Options) ApplyEnvironment(flags *pflag.FlagSet) error {
	stringDefaults := map[string]struct {
		target *string
		env    string
	}{
		"auth":                               {&o.AuthMode, "AKS_FLEX_NODE_AUTH"},
		"msi-client-id":                      {&o.MSIClientID, "AKS_FLEX_NODE_MSI_CLIENT_ID"},
		"sp-tenant-id":                       {&o.SPTenantID, "AKS_FLEX_NODE_SP_TENANT_ID"},
		"sp-client-id":                       {&o.SPClientID, "AKS_FLEX_NODE_SP_CLIENT_ID"},
		"sp-client-secret-file":              {&o.SPClientSecretFile, "AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE"},
		"sp-client-certificate-file":         {&o.SPClientCertificateFile, "AKS_FLEX_NODE_SP_CLIENT_CERTIFICATE_FILE"},
		"bootstrap-data-api-version":         {&o.BootstrapDataAPIVersion, "AKS_FLEX_NODE_BOOTSTRAP_DATA_API_VERSION"},
		"cluster-resource-id":                {&o.ClusterResourceID, "AKS_FLEX_NODE_CLUSTER_RESOURCE_ID"},
		"agent-pool-name":                    {&o.AgentPoolName, "AKS_FLEX_NODE_AGENT_POOL_NAME"},
		"resource-manager-endpoint":          {&o.ResourceManagerEndpoint, "AKS_FLEX_NODE_RESOURCE_MANAGER_ENDPOINT"},
		"bootstrap-oci-image":                {&o.BootstrapOCIImage, "AKS_FLEX_NODE_BOOTSTRAP_OCI_IMAGE"},
		"bootstrap-offline-artifacts-source": {&o.BootstrapOfflineArtifactsSource, "AKS_FLEX_NODE_BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE"},
		"config":                             {&o.BaseConfigPath, "AKS_FLEX_NODE_BASE_CONFIG_FILE"},
		"config-path":                        {&o.ConfigPath, "AKS_FLEX_NODE_CONFIG_PATH"},
		"agent-url":                          {&o.AgentURL, "AKS_FLEX_NODE_AGENT_URL"},
		"agent-version":                      {&o.AgentVersion, "AKS_FLEX_NODE_AGENT_VERSION"},
		"agent-sha256":                       {&o.AgentSHA256, "AKS_FLEX_NODE_AGENT_SHA256"},
		"install-dir":                        {&o.InstallDir, "AKS_FLEX_NODE_INSTALL_DIR"},
	}
	for name, value := range stringDefaults {
		if !flags.Changed(name) {
			if envValue := os.Getenv(value.env); envValue != "" {
				*value.target = envValue
			}
		}
	}
	if !flags.Changed("fetch-bootstrap-data") {
		value := strings.TrimSpace(os.Getenv("AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA"))
		if value != "" {
			switch strings.ToLower(value) {
			case "1", "true", "yes", "y":
				o.FetchBootstrapData = true
			case "0", "false", "no", "n":
				o.FetchBootstrapData = false
			default:
				return fmt.Errorf("parse AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA: expected boolean, got %q", value)
			}
		}
	}
	if envOverride := os.Getenv("AKS_FLEX_NODE_CONFIG_OVERRIDES"); envOverride != "" {
		o.ConfigOverrides = append([]string{envOverride}, o.ConfigOverrides...)
	}
	o.SPClientSecret = os.Getenv("AKS_FLEX_NODE_SP_CLIENT_SECRET")
	_ = os.Unsetenv("AKS_FLEX_NODE_SP_CLIENT_SECRET")
	return nil
}

func (o Options) HasOnboardingInputs() bool {
	return o.FetchBootstrapData || o.AuthMode != "" || o.ClusterResourceID != "" ||
		o.AgentPoolName != "" || o.ResourceManagerEndpoint != "" ||
		o.BootstrapOCIImage != "" || o.BootstrapOfflineArtifactsSource != "" ||
		len(o.ConfigOverrides) != 0
}
