package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const testTargetAgentPoolName = "aksflexnodes"

func setTestTargetAgentPoolName(c *Config) {
	if c != nil && strings.TrimSpace(c.Azure.TargetAgentPoolName) == "" {
		c.Azure.TargetAgentPoolName = testTargetAgentPoolName
	}
}

func TestSetDefaults(t *testing.T) {
	tests := []struct {
		name   string
		config *Config
		want   func(*Config) bool // validation function
	}{
		{
			name:   "empty config gets all defaults",
			config: &Config{},
			want: func(c *Config) bool {
				return c.Azure.Cloud == "" &&
					c.Azure.ResourceManagerEndpointURL == "https://management.azure.com" &&
					c.Azure.TargetAgentPoolName == "aksflexnodes" &&
					c.Agent.LogLevel == "info" &&
					c.Agent.LogDir == "/var/log/aks-flex-node" &&
					c.Agent.MachineOperationMode == "auto" &&
					c.Node.MaxPods == 110 &&
					c.Components.Runc == "1.1.12"
			},
		},
		{
			name: "existing values are preserved",
			config: &Config{
				Azure: AzureConfig{
					Cloud:                      "AzurePublicCloud",
					ResourceManagerEndpointURL: "https://management.example.test/",
					TargetAgentPoolName:        " flexnode-edge ",
				},
				Agent: AgentConfig{
					LogLevel: "debug",
					LogDir:   "/custom/log/dir",
				},
			},
			want: func(c *Config) bool {
				return c.Agent.LogLevel == "debug" &&
					c.Agent.LogDir == "/custom/log/dir" &&
					c.Azure.Cloud == "AzurePublicCloud" &&
					c.Azure.ResourceManagerEndpointURL == "https://management.example.test" &&
					c.Azure.TargetAgentPoolName == "flexnode-edge"
			},
		},
		{
			name: "cloud fallback sets sovereign endpoint",
			config: &Config{
				Azure: AzureConfig{
					Cloud: "AzureUSGovernment",
				},
			},
			want: func(c *Config) bool {
				return c.Azure.ResourceManagerEndpointURL == "https://management.usgovcloudapi.net"
			},
		},
		{
			name: "cloud fallback supports Azure China",
			config: &Config{
				Azure: AzureConfig{
					Cloud: "AzureChinaCloud",
				},
			},
			want: func(c *Config) bool {
				return c.Azure.ResourceManagerEndpointURL == "https://management.chinacloudapi.cn"
			},
		},
		{
			name: "node kubelet defaults are set correctly",
			config: &Config{
				Node: NodeConfig{
					MaxPods: 50, // custom value should be preserved
				},
			},
			want: func(c *Config) bool {
				return c.Node.MaxPods == 50 && // preserved
					c.Node.Kubelet.Verbosity == 2 &&
					c.Node.Kubelet.ImageGCHighThreshold == 85 &&
					c.Node.Kubelet.ImageGCLowThreshold == 80
			},
		},
		{
			name:   "machine operation mode can be disabled",
			config: &Config{Agent: AgentConfig{MachineOperationMode: "disable"}},
			want: func(c *Config) bool {
				return c.Agent.MachineOperationMode == "disable"
			},
		},
		{
			name:   "require machine registration is preserved",
			config: &Config{Agent: AgentConfig{RequireMachineRegistration: true}},
			want: func(c *Config) bool {
				return c.Agent.RequireMachineRegistration
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.setDefaults()
			if !tt.want(tt.config) {
				t.Errorf("SetDefaults() failed validation for %s", tt.name)
			}
		})
	}
}

func TestDefaultResourceManagerTokenScope(t *testing.T) {
	t.Parallel()

	if DefaultResourceManagerTokenScope != "https://management.azure.com/.default" {
		t.Fatalf("DefaultResourceManagerTokenScope = %q, want https://management.azure.com/.default", DefaultResourceManagerTokenScope)
	}
}

func TestResolveNodeName(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.resolveNodeName(os.Hostname)
	if err != nil {
		t.Fatalf("resolveNodeName: %v", err)
	}
	if got == "" {
		t.Fatal("resolveNodeName returned empty node name")
	}
	if cfg.Agent.NodeName != got {
		t.Fatalf("cfg.Agent.NodeName=%q, want %q", cfg.Agent.NodeName, got)
	}

	cfg.Agent.NodeName = "custom-node"
	got, err = cfg.resolveNodeName(os.Hostname)
	if err != nil {
		t.Fatalf("resolveNodeName with existing node name: %v", err)
	}
	if got != "custom-node" {
		t.Fatalf("resolveNodeName=%q, want custom-node", got)
	}

	cfg.Agent.NodeName = "  custom-node-2  "
	got, err = cfg.resolveNodeName(os.Hostname)
	if err != nil {
		t.Fatalf("resolveNodeName with spaced node name: %v", err)
	}
	if got != "custom-node-2" || cfg.Agent.NodeName != "custom-node-2" {
		t.Fatalf("resolved node name=%q cfg=%q, want custom-node-2", got, cfg.Agent.NodeName)
	}
}

func TestResolveNodeNameRejectsInvalidConfiguredName(t *testing.T) {
	cfg := &Config{Agent: AgentConfig{NodeName: "Invalid_Node"}}
	if _, err := cfg.resolveNodeName(os.Hostname); err == nil || !strings.Contains(err.Error(), "valid Kubernetes DNS subdomain") {
		t.Fatalf("resolveNodeName error = %v, want DNS subdomain error", err)
	}
}

func TestResolveNodeNameLowercasesHostname(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.resolveNodeName(func() (string, error) { return "  PHost01  ", nil })
	if err != nil {
		t.Fatalf("resolveNodeName: %v", err)
	}
	if got != "phost01" || cfg.Agent.NodeName != "phost01" {
		t.Fatalf("resolved node name=%q cfg=%q, want phost01", got, cfg.Agent.NodeName)
	}
}

func TestResolveNodeNameRejectsInvalidHostnameWithSuggestion(t *testing.T) {
	cfg := &Config{}
	_, err := cfg.resolveNodeName(func() (string, error) { return "Invalid_Node", nil })
	if err == nil || !strings.Contains(err.Error(), "set agent.nodeName") {
		t.Fatalf("resolveNodeName error = %v, want agent.nodeName suggestion", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config passes",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing subscription ID uses target cluster subscription",
			config: &Config{
				Azure: AzureConfig{
					TenantID: "12345678-1234-1234-1234-123456789012",
					Cloud:    "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing tenant ID fails when Arc is enabled",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					Arc: &ArcConfig{
						Enabled:       true,
						ResourceGroup: "test-rg",
						MachineName:   "test-machine",
						Location:      "eastus",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.tenantId is required",
		},
		{
			name: "missing target cluster location passes",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing target cluster resource ID fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						Location: "eastus",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.targetCluster.resourceId is required",
		},
		{
			name: "invalid resource ID format fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "invalid-resource-id",
						Location:   "eastus",
					},
				},
			},
			wantErr: true,
			errMsg:  "invalid azure.targetCluster.resourceId:",
		},
		{
			name: "unknown azure cloud falls back to public endpoint",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "InvalidCloud",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "resource manager endpoint without scheme fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID:             "12345678-1234-1234-1234-123456789012",
					ResourceManagerEndpointURL: "management.azure.com",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.resourceManagerEndpoint must be an absolute https URL",
		},
		{
			name: "resource manager endpoint with http fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID:             "12345678-1234-1234-1234-123456789012",
					ResourceManagerEndpointURL: "http://management.azure.com",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.resourceManagerEndpoint must use https",
		},
		{
			name: "resource manager endpoint with path fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID:             "12345678-1234-1234-1234-123456789012",
					ResourceManagerEndpointURL: "https://management.azure.com/path",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.resourceManagerEndpoint must not include a path, query, or fragment",
		},
		{
			name: "resource manager endpoint with port fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID:             "12345678-1234-1234-1234-123456789012",
					ResourceManagerEndpointURL: "https://management.azure.com:443",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.resourceManagerEndpoint must not include user info or port",
		},
		{
			name: "resource manager endpoint with user info fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID:             "12345678-1234-1234-1234-123456789012",
					ResourceManagerEndpointURL: "https://user:pass@management.azure.com",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.resourceManagerEndpoint must not include user info or port",
		},
		{
			name: "explicit resource manager endpoint is preserved",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID:             "12345678-1234-1234-1234-123456789012",
					TenantID:                   "12345678-1234-1234-1234-123456789012",
					Cloud:                      "InvalidCloud",
					ResourceManagerEndpointURL: "https://management.example.test/",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid log level fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "invalid",
				},
			},
			wantErr: true,
			errMsg:  "invalid agent.logLevel: invalid log level 'invalid'. Valid levels are: debug, info, warning, error",
		},
		{
			name: "invalid machine operation mode fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel:             "info",
					MachineOperationMode: "arm",
				},
			},
			wantErr: true,
			errMsg:  "invalid agent.machineOperationMode",
		},
		{
			name: "valid ARM proxy override passes",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel:                  "info",
					ARMProxyURLOverrideForE2E: "http://127.0.0.1:8080/proxy",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid ARM proxy override fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel:                  "info",
					ARMProxyURLOverrideForE2E: "/proxy",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: true,
			errMsg:  "invalid agent.armProxyURLOverrideForE2E",
		},
		{
			name: "valid arc config passes",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
					Arc: &ArcConfig{
						Enabled:       true,
						ResourceGroup: "test-rg",
						MachineName:   "test-machine",
						Location:      "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestTargetAgentPoolName(tt.config)

			err := tt.config.validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate() expected error but got none for %s", tt.name)
					return
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %v", err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("Validate() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestValidateDefaultsTargetAgentPoolName(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			SubscriptionID: "12345678-1234-1234-1234-123456789012",
			Cloud:          "AzurePublicCloud",
			BootstrapToken: &BootstrapTokenConfig{
				Token: "abcdef.0123456789abcdef",
			},
			TargetCluster: &TargetClusterConfig{
				ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
			},
		},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
				CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
			},
		},
	}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() unexpected error = %v", err)
	}
	if cfg.Azure.TargetAgentPoolName != "aksflexnodes" {
		t.Fatalf("TargetAgentPoolName = %q, want aksflexnodes", cfg.Azure.TargetAgentPoolName)
	}
}

func TestLoadConfig(t *testing.T) {
	// Create a temporary directory for test config files
	tempDir, err := os.MkdirTemp("", "aks-config-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	tests := []struct {
		name       string
		configJSON string
		wantErr    bool
		errMsg     string
	}{
		{
			name: "valid config file loads successfully",
			configJSON: `{
					"azure": {
						"subscriptionId": "12345678-1234-1234-1234-123456789012",
						"tenantId": "12345678-1234-1234-1234-123456789012",
						"cloud": "AzurePublicCloud",
						"targetAgentPoolName": "flexnode-edge",
						"bootstrapToken": {
							"token": "abcdef.0123456789abcdef"
						},
						"targetCluster": {
							"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
							"location": "eastus"
						}
					},
					"agent": {
						"logLevel": "debug"
					},
					"node": {
						"kubelet": {
							"clusterFQDN": "test-cluster-abc123.hcp.eastus.azmk8s.io",
							"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R"
						}
					}
				}`,
			wantErr: false,
		},
		{
			name: "config with missing required fields fails",
			configJSON: `{
				"azure": {
					"cloud": "AzurePublicCloud"
				}
			}`,
			wantErr: true,
			errMsg:  "config validation failed",
		},
		{
			name: "invalid JSON fails",
			configJSON: `{
				"azure": {
					"subscriptionId": "invalid-json"
				`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary config file
			configFile := filepath.Join(tempDir, "config.json")
			if err := os.WriteFile(configFile, []byte(tt.configJSON), 0o644); err != nil {
				t.Fatalf("Failed to write test config file: %v", err)
			}

			// Test LoadConfig
			config, err := LoadConfig(configFile)
			if tt.wantErr {
				if err == nil {
					t.Errorf("LoadConfig() expected error but got none")
				}
				// Just verify we got an error, don't check specific message
			} else {
				if err != nil {
					t.Errorf("LoadConfig() unexpected error = %v", err)
				}
				if config == nil {
					t.Errorf("LoadConfig() returned nil config")
					return
				}

				// Verify defaults were applied
				if config.Agent.LogLevel == "debug" {
					// Custom value preserved
				} else if config.Agent.LogLevel != "info" {
					t.Errorf("Expected default log level 'info', got %s", config.Agent.LogLevel)
				}
				if config.Azure.TargetAgentPoolName != "flexnode-edge" {
					t.Errorf("Expected target agent pool name 'flexnode-edge', got %s", config.Azure.TargetAgentPoolName)
				}
			}
		})
	}
}

func TestLoadConfigPoolBootstrapData(t *testing.T) {
	t.Parallel()

	configJSON := `{
		"azure": {
			"resourceManagerEndpoint": "https://management.azure.com/",
			"targetAgentPoolName": "pool1",
			"bootstrapToken": {
				"token": "abcdef.0123456789abcdef"
			},
			"targetCluster": {
				"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster"
			}
		},
		"components": {
			"kubernetes": "1.29.0",
			"containerd": "2.0.5",
			"runc": "1.2.3"
		},
		"networking": {
			"dnsServiceIP": "10.42.0.10",
			"cniVersion": "1.5.1"
		},
		"node": {
			"maxPods": 30,
			"labels": {
				"env": "test"
			},
			"taints": [
				"dedicated=flexnode:NoSchedule"
			],
			"kubelet": {
				"clusterFQDN": "test-cluster-dns-12345678.hcp.eastus.azmk8s.io",
				"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0t"
			}
		}
	}`

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("LoadConfig() unexpected error: %v", err)
	}

	if cfg.Azure.ResourceManagerEndpointURL != "https://management.azure.com" {
		t.Fatalf("Azure.ResourceManagerEndpointURL = %q, want https://management.azure.com", cfg.Azure.ResourceManagerEndpointURL)
	}
	if cfg.Node.Kubelet.ClusterFQDN != "test-cluster-dns-12345678.hcp.eastus.azmk8s.io" {
		t.Fatalf("Node.Kubelet.ClusterFQDN = %q", cfg.Node.Kubelet.ClusterFQDN)
	}
	if cfg.APIServerURL() != "https://test-cluster-dns-12345678.hcp.eastus.azmk8s.io:443" {
		t.Fatalf("APIServerURL = %q", cfg.APIServerURL())
	}
	if cfg.Node.Kubelet.CACertData != "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0t" {
		t.Fatalf("Node.Kubelet.CACertData = %q", cfg.Node.Kubelet.CACertData)
	}
	if cfg.Node.MaxPods != 30 {
		t.Fatalf("Node.MaxPods = %d, want 30", cfg.Node.MaxPods)
	}
	if cfg.Node.Labels["env"] != "test" {
		t.Fatalf("Node.Labels[env] = %q, want test", cfg.Node.Labels["env"])
	}
	if len(cfg.Node.Taints) != 1 || cfg.Node.Taints[0] != "dedicated=flexnode:NoSchedule" {
		t.Fatalf("Node.Taints = %#v, want dedicated=flexnode:NoSchedule", cfg.Node.Taints)
	}

	agentCfg := ToAgentConfig(cfg, "flex-node-1")
	if agentCfg.Cluster.Version != "1.29.0" {
		t.Fatalf("Agent Cluster.Version = %q, want 1.29.0", agentCfg.Cluster.Version)
	}
	if agentCfg.Cluster.ClusterDNS != "10.42.0.10" {
		t.Fatalf("Agent Cluster.ClusterDNS = %q, want 10.42.0.10", agentCfg.Cluster.ClusterDNS)
	}
	if agentCfg.CRI.Containerd.Version != "2.0.5" {
		t.Fatalf("Agent CRI.Containerd.Version = %q, want 2.0.5", agentCfg.CRI.Containerd.Version)
	}
	if agentCfg.CRI.Runc.Version != "1.2.3" {
		t.Fatalf("Agent CRI.Runc.Version = %q, want 1.2.3", agentCfg.CRI.Runc.Version)
	}
	if agentCfg.CNI.PluginVersion != "1.5.1" {
		t.Fatalf("Agent CNI.PluginVersion = %q, want 1.5.1", agentCfg.CNI.PluginVersion)
	}
	if agentCfg.Kubelet.ApiServer != "https://test-cluster-dns-12345678.hcp.eastus.azmk8s.io:443" {
		t.Fatalf("Agent Kubelet.ApiServer = %q", agentCfg.Kubelet.ApiServer)
	}
	if agentCfg.Kubelet.Auth.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("Agent bootstrap token = %q", agentCfg.Kubelet.Auth.BootstrapToken)
	}
	if len(agentCfg.Kubelet.Labels) == 0 || agentCfg.Kubelet.Labels["env"] != "test" {
		t.Fatalf("Agent Kubelet.Labels = %#v, want env=test", agentCfg.Kubelet.Labels)
	}
	if len(agentCfg.Kubelet.RegisterWithTaints) != 1 || agentCfg.Kubelet.RegisterWithTaints[0] != "dedicated=flexnode:NoSchedule" {
		t.Fatalf("Agent Kubelet.RegisterWithTaints = %#v", agentCfg.Kubelet.RegisterWithTaints)
	}
}

func TestLoadConfigPoolBootstrapDataMissingOptionalFields(t *testing.T) {
	t.Parallel()

	configJSON := `{
		"azure": {
			"targetAgentPoolName": "pool1",
			"bootstrapToken": {
				"token": "abcdef.0123456789abcdef"
			},
			"targetCluster": {
				"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster"
			}
		},
		"components": {
			"kubernetes": "1.29.0"
		},
		"node": {
			"kubelet": {
				"clusterFQDN": "test-cluster-dns-12345678.hcp.eastus.azmk8s.io",
				"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0t"
			}
		}
	}`

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("LoadConfig() unexpected error: %v", err)
	}

	if cfg.Azure.Cloud != "" {
		t.Fatalf("Azure.Cloud = %q, want empty when RP omits legacy cloud", cfg.Azure.Cloud)
	}
	if cfg.Azure.SubscriptionID != "12345678-1234-1234-1234-123456789012" {
		t.Fatalf("Azure.SubscriptionID = %q", cfg.Azure.SubscriptionID)
	}
	if cfg.Azure.TargetAgentPoolName != "pool1" {
		t.Fatalf("Azure.TargetAgentPoolName = %q, want pool1", cfg.Azure.TargetAgentPoolName)
	}
	if cfg.Networking.DNSServiceIP != "10.0.0.10" {
		t.Fatalf("Node.Kubelet.DNSServiceIP = %q, want default 10.0.0.10", cfg.Networking.DNSServiceIP)
	}
	if cfg.Node.Kubelet.ClusterFQDN != "test-cluster-dns-12345678.hcp.eastus.azmk8s.io" {
		t.Fatalf("Node.Kubelet.ClusterFQDN = %q", cfg.Node.Kubelet.ClusterFQDN)
	}
	if cfg.APIServerURL() != "https://test-cluster-dns-12345678.hcp.eastus.azmk8s.io:443" {
		t.Fatalf("APIServerURL = %q", cfg.APIServerURL())
	}
	if cfg.Components.Kubernetes != "1.29.0" {
		t.Fatalf("Kubernetes.Version = %q, want 1.29.0", cfg.Components.Kubernetes)
	}
	if cfg.Components.Containerd != "" {
		t.Fatalf("Containerd.Version = %q, want empty when RP omits it", cfg.Components.Containerd)
	}
	if cfg.Components.Runc != "1.1.12" {
		t.Fatalf("Runc.Version = %q, want default 1.1.12", cfg.Components.Runc)
	}
	if cfg.Networking.CNIVersion != "" {
		t.Fatalf("CNI.Version = %q, want empty when RP omits it", cfg.Networking.CNIVersion)
	}
}

func TestLoadConfigUsesRPConfigOverLegacyAliases(t *testing.T) {
	t.Parallel()

	configJSON := `{
		"azure": {
			"subscriptionId": "12345678-1234-1234-1234-123456789012",
			"tenantId": "12345678-1234-1234-1234-123456789012",
			"cloud": "AzurePublicCloud",
			"targetAgentPoolName": "aksflexnodes",
			"bootstrapToken": {
				"token": "abcdef.0123456789abcdef"
			},
			"targetCluster": {
				"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
				"location": "eastus"
			}
		},
		"kubernetes": {
			"version": "1.30.1"
		},
		"containerd": {
			"version": "2.1.0"
		},
		"runc": {
			"version": "1.3.0"
		},
		"cni": {
			"version": "1.6.0"
		},
		"node": {
			"maxPods": 35,
			"labels": {
				"legacy": "true"
			},
			"taints": [
				"legacy=true:NoSchedule"
			],
			"kubelet": {
				"serverURL": "https://legacy.example.test:443",
				"clusterFQDN": "rp.example.test",
				"dnsServiceIP": "10.0.0.10",
				"caCertData": "bGVnYWN5LWNh"
			}
		},
		"components": {
			"kubernetes": "9.99.0",
			"containerd": "9.99.0",
			"runc": "9.99.0"
		},
		"networking": {
			"dnsServiceIP": "10.42.0.10",
			"cniVersion": "9.99.0"
		}
	}`

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("LoadConfig() unexpected error: %v", err)
	}

	if cfg.Components.Kubernetes != "9.99.0" {
		t.Fatalf("Components.Kubernetes = %q, want RP value", cfg.Components.Kubernetes)
	}
	if cfg.Components.Containerd != "9.99.0" {
		t.Fatalf("Components.Containerd = %q, want RP value", cfg.Components.Containerd)
	}
	if cfg.Components.Runc != "9.99.0" {
		t.Fatalf("Components.Runc = %q, want RP value", cfg.Components.Runc)
	}
	if cfg.Networking.CNIVersion != "9.99.0" {
		t.Fatalf("Networking.CNIVersion = %q, want RP value", cfg.Networking.CNIVersion)
	}
	if cfg.Networking.DNSServiceIP != "10.42.0.10" {
		t.Fatalf("Networking.DNSServiceIP = %q, want RP value", cfg.Networking.DNSServiceIP)
	}
	if cfg.Node.Kubelet.ClusterFQDN != "rp.example.test" {
		t.Fatalf("Node.Kubelet.ClusterFQDN = %q, want RP value", cfg.Node.Kubelet.ClusterFQDN)
	}
	if cfg.APIServerURL() != "https://rp.example.test:443" {
		t.Fatalf("APIServerURL = %q", cfg.APIServerURL())
	}
}

func TestLoadConfigAdaptsLegacyConfigAliases(t *testing.T) {
	t.Parallel()

	configJSON := `{
		"azure": {
			"targetAgentPoolName": "pool1",
			"bootstrapToken": {
				"token": "abcdef.0123456789abcdef"
			},
			"targetCluster": {
				"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster"
			}
		},
		"kubernetes": {
			"version": "1.30.1"
		},
		"containerd": {
			"version": "2.1.0"
		},
		"runc": {
			"version": "1.3.0"
		},
		"cni": {
			"version": "1.6.0"
		},
		"node": {
			"kubelet": {
				"serverURL": "https://legacy.example.test:443",
				"dnsServiceIP": "10.0.0.10",
				"caCertData": "bGVnYWN5LWNh"
			}
		}
	}`

	configFile := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configFile, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := LoadConfig(configFile)
	if err != nil {
		t.Fatalf("LoadConfig() unexpected error: %v", err)
	}

	if cfg.Components.Kubernetes != "1.30.1" {
		t.Fatalf("Components.Kubernetes = %q, want legacy value", cfg.Components.Kubernetes)
	}
	if cfg.Components.Containerd != "2.1.0" {
		t.Fatalf("Components.Containerd = %q, want legacy value", cfg.Components.Containerd)
	}
	if cfg.Components.Runc != "1.3.0" {
		t.Fatalf("Components.Runc = %q, want legacy value", cfg.Components.Runc)
	}
	if cfg.Networking.CNIVersion != "1.6.0" {
		t.Fatalf("Networking.CNIVersion = %q, want legacy value", cfg.Networking.CNIVersion)
	}
	if cfg.Networking.DNSServiceIP != "10.0.0.10" {
		t.Fatalf("Networking.DNSServiceIP = %q, want legacy value", cfg.Networking.DNSServiceIP)
	}
	if cfg.Node.Kubelet.ClusterFQDN != "legacy.example.test:443" {
		t.Fatalf("Node.Kubelet.ClusterFQDN = %q, want legacy host", cfg.Node.Kubelet.ClusterFQDN)
	}
	if cfg.APIServerURL() != "https://legacy.example.test:443" {
		t.Fatalf("APIServerURL = %q", cfg.APIServerURL())
	}
}

func TestJSONDurationUnmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		want    time.Duration
		wantErr bool
	}{
		{
			name: "duration string",
			json: `{"machineReconcileInterval":"10m"}`,
			want: 10 * time.Minute,
		},
		{
			name: "numeric nanoseconds",
			json: `{"machineReconcileInterval":600000000000}`,
			want: 10 * time.Minute,
		},
		{
			name:    "invalid duration string",
			json:    `{"machineReconcileInterval":"ten minutes"}`,
			wantErr: true,
		},
		{
			name:    "invalid type",
			json:    `{"machineReconcileInterval":true}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg struct {
				MachineReconcileInterval JSONDuration `json:"machineReconcileInterval"`
			}
			err := json.Unmarshal([]byte(tt.json), &cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("json.Unmarshal expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			got := time.Duration(cfg.MachineReconcileInterval)
			if got != tt.want {
				t.Fatalf("MachineReconcileInterval=%s, want %s", got, tt.want)
			}
		})
	}
}

func TestJSONDurationMarshal(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(JSONDuration(10 * time.Minute))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if got, want := string(data), `"10m0s"`; got != want {
		t.Fatalf("json.Marshal=%s, want %s", got, want)
	}
}

func TestValidateAzureResourceID(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		wantErr    bool
	}{
		{
			name:       "valid AKS resource ID",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
			wantErr:    false,
		},
		{
			name:       "resource ID with hyphens and dots in names",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg.with.dots/providers/Microsoft.ContainerService/managedClusters/test-cluster-name",
			wantErr:    false,
		},
		{
			name:       "case insensitive - lowercase provider",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/microsoft.containerservice/managedClusters/test-cluster",
			wantErr:    false,
		},
		{
			name:       "case insensitive - uppercase provider",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/MICROSOFT.CONTAINERSERVICE/managedClusters/test-cluster",
			wantErr:    false,
		},
		{
			name:       "case insensitive - mixed case provider",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/microsoft.ContainerService/managedClusters/test-cluster",
			wantErr:    false,
		},
		{
			name:       "case insensitive - uppercase path segments",
			resourceID: "/SUBSCRIPTIONS/12345678-1234-1234-1234-123456789012/RESOURCEGROUPS/test-rg/PROVIDERS/Microsoft.ContainerService/MANAGEDCLUSTERS/test-cluster",
			wantErr:    false,
		},
		{
			name:       "case insensitive - mixed case path segments",
			resourceID: "/Subscriptions/12345678-1234-1234-1234-123456789012/ResourceGroups/test-rg/Providers/Microsoft.ContainerService/ManagedClusters/test-cluster",
			wantErr:    false,
		},
		{
			name:       "case insensitive - all lowercase",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourcegroups/test-rg/providers/microsoft.containerservice/managedclusters/test-cluster",
			wantErr:    false,
		},
		{
			name:       "invalid subscription ID format",
			resourceID: "/subscriptions/invalid-subscription-id/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
			wantErr:    true,
		},
		{
			name:       "wrong provider type",
			resourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.Compute/virtualMachines/test-vm",
			wantErr:    true,
		},
		{
			name:       "empty resource ID",
			resourceID: "",
			wantErr:    true,
		},
		{
			name:       "malformed resource ID",
			resourceID: "not-a-resource-id",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAzureResourceID(tt.resourceID)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateAzureResourceID() expected error but got none for %s", tt.resourceID)
				}
			} else {
				if err != nil {
					t.Errorf("validateAzureResourceID() unexpected error = %v for %s", err, tt.resourceID)
				}
			}
		})
	}
}

func TestPopulateTargetClusterInfoFromConfig(t *testing.T) {
	config := &Config{
		Azure: AzureConfig{
			TargetCluster: &TargetClusterConfig{
				ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
				Location:   "eastus",
			},
		},
	}

	populateTargetClusterInfoFromConfig(config)

	expected := TargetClusterConfig{
		ResourceID:        "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
		Location:          "eastus",
		Name:              "test-cluster",
		ResourceGroup:     "test-rg",
		SubscriptionID:    "12345678-1234-1234-1234-123456789012",
		NodeResourceGroup: "MC_test-rg_test-cluster_eastus",
	}

	if config.Azure.TargetCluster.Name != expected.Name {
		t.Errorf("Expected Name %s, got %s", expected.Name, config.Azure.TargetCluster.Name)
	}
	if config.Azure.TargetCluster.ResourceGroup != expected.ResourceGroup {
		t.Errorf("Expected ResourceGroup %s, got %s", expected.ResourceGroup, config.Azure.TargetCluster.ResourceGroup)
	}
	if config.Azure.TargetCluster.SubscriptionID != expected.SubscriptionID {
		t.Errorf("Expected SubscriptionID %s, got %s", expected.SubscriptionID, config.Azure.TargetCluster.SubscriptionID)
	}
	if config.Azure.SubscriptionID != expected.SubscriptionID {
		t.Errorf("Expected Azure SubscriptionID %s, got %s", expected.SubscriptionID, config.Azure.SubscriptionID)
	}
	if config.Azure.TargetCluster.NodeResourceGroup != expected.NodeResourceGroup {
		t.Errorf("Expected NodeResourceGroup %s, got %s", expected.NodeResourceGroup, config.Azure.TargetCluster.NodeResourceGroup)
	}
	if config.Azure.TargetCluster.Location != expected.Location {
		t.Errorf("Expected Location %s, got %s", expected.Location, config.Azure.TargetCluster.Location)
	}
	if config.Azure.TargetCluster.ResourceID != expected.ResourceID {
		t.Errorf("Expected ResourceID %s, got %s", expected.ResourceID, config.Azure.TargetCluster.ResourceID)
	}
}

func TestManagedIdentityConfiguration(t *testing.T) {
	// Create a temporary directory for test config files
	tempDir, err := os.MkdirTemp("", "aks-config-msi-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	tests := []struct {
		name              string
		configJSON        string
		wantMIConfigured  bool
		wantMIClientID    string
		wantValidationErr bool
	}{
		{
			name: "managedIdentity with empty object",
			configJSON: `{
				"azure": {
					"subscriptionId": "12345678-1234-1234-1234-123456789012",
					"tenantId": "12345678-1234-1234-1234-123456789012",
					"cloud": "AzurePublicCloud",
					"targetAgentPoolName": "aksflexnodes",
					"managedIdentity": {},
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						"location": "eastus"
					}
				}
			}`,
			wantMIConfigured:  true,
			wantMIClientID:    "",
			wantValidationErr: false,
		},
		{
			name: "managedIdentity with clientId",
			configJSON: `{
				"azure": {
					"subscriptionId": "12345678-1234-1234-1234-123456789012",
					"tenantId": "12345678-1234-1234-1234-123456789012",
					"cloud": "AzurePublicCloud",
					"targetAgentPoolName": "aksflexnodes",
					"managedIdentity": {
						"clientId": "87654321-4321-4321-4321-210987654321"
					},
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						"location": "eastus"
					}
				}
			}`,
			wantMIConfigured:  true,
			wantMIClientID:    "87654321-4321-4321-4321-210987654321",
			wantValidationErr: false,
		},
		{
			name: "managedIdentity with empty clientId string",
			configJSON: `{
				"azure": {
					"subscriptionId": "12345678-1234-1234-1234-123456789012",
					"tenantId": "12345678-1234-1234-1234-123456789012",
					"cloud": "AzurePublicCloud",
					"targetAgentPoolName": "aksflexnodes",
					"managedIdentity": {
						"clientId": ""
					},
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						"location": "eastus"
					}
				}
			}`,
			wantMIConfigured:  true,
			wantMIClientID:    "",
			wantValidationErr: false,
		},
		{
			name: "no managedIdentity field",
			configJSON: `{
				"azure": {
					"subscriptionId": "12345678-1234-1234-1234-123456789012",
					"tenantId": "12345678-1234-1234-1234-123456789012",
					"cloud": "AzurePublicCloud",
					"targetAgentPoolName": "aksflexnodes",
					"bootstrapToken": {
						"token": "abcdef.0123456789abcdef"
					},
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						"location": "eastus"
					}
				},
				"node": {
					"kubelet": {
						"serverURL": "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R"
					}
				}
			}`,
			wantMIConfigured:  false,
			wantMIClientID:    "",
			wantValidationErr: false,
		},
		{
			name: "arc and managedIdentity both configured should fail validation",
			configJSON: `{
				"azure": {
					"subscriptionId": "12345678-1234-1234-1234-123456789012",
					"tenantId": "12345678-1234-1234-1234-123456789012",
					"cloud": "AzurePublicCloud",
					"managedIdentity": {},
					"arc": {
						"enabled": true,
						"machineName": "test-node",
						"resourceGroup": "test-rg",
						"location": "eastus"
					},
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						"location": "eastus"
					}
				}
			}`,
			wantMIConfigured:  true,
			wantValidationErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary config file
			configFile := filepath.Join(tempDir, "config-"+tt.name+".json")
			if err := os.WriteFile(configFile, []byte(tt.configJSON), 0o644); err != nil {
				t.Fatalf("Failed to write test config file: %v", err)
			}

			// Test LoadConfig
			config, err := LoadConfig(configFile)
			if tt.wantValidationErr {
				if err == nil {
					t.Errorf("LoadConfig() expected validation error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("LoadConfig() unexpected error = %v", err)
			}

			// Verify IsMIConfigured
			if got := config.IsMIConfigured(); got != tt.wantMIConfigured {
				t.Errorf("IsMIConfigured() = %v, want %v", got, tt.wantMIConfigured)
			}

			// Verify ClientID if ManagedIdentity is configured
			if tt.wantMIConfigured {
				var gotClientID string
				if config.Azure.ManagedIdentity != nil {
					gotClientID = config.Azure.ManagedIdentity.ClientID
				}
				if gotClientID != tt.wantMIClientID {
					t.Errorf("ManagedIdentity.ClientID = %q, want %q", gotClientID, tt.wantMIClientID)
				}
			}
		})
	}
}

func TestValidateBootstrapToken(t *testing.T) {
	tests := []struct {
		name      string
		config    *Config
		wantErr   bool
		errString string
	}{
		{
			name: "valid bootstrap token",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid token format - uppercase",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "ABCDEF.0123456789ABCDEF",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr:   true,
			errString: "invalid bootstrap token format",
		},
		{
			name: "invalid token format - wrong token-id length",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcde.0123456789abcdef",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr:   true,
			errString: "invalid bootstrap token format",
		},
		{
			name: "invalid token format - wrong token-secret length",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcde",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr:   true,
			errString: "invalid bootstrap token format",
		},
		{
			name: "invalid token format - no separator",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef0123456789abcdef",
					},
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr:   true,
			errString: "invalid bootstrap token format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validateBootstrapToken()
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateBootstrapToken() expected error but got none")
				} else if tt.errString != "" && !strings.Contains(err.Error(), tt.errString) {
					t.Errorf("validateBootstrapToken() error = %v, want error containing %v", err, tt.errString)
				}
			} else {
				if err != nil {
					t.Errorf("validateBootstrapToken() unexpected error = %v", err)
				}
			}
		})
	}
}

func TestAuthenticationMethodValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  *Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "bootstrap token authentication enabled",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "service principal authentication enabled",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					ServicePrincipal: &ServicePrincipalConfig{
						TenantID:     "12345678-1234-1234-1234-123456789012",
						ClientID:     "12345678-1234-1234-1234-123456789012",
						ClientSecret: "test-secret",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: false,
		},
		{
			name: "partial service principal fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					ServicePrincipal: &ServicePrincipalConfig{
						TenantID: "12345678-1234-1234-1234-123456789012",
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: true,
			errMsg:  "azure.servicePrincipal.clientSecret is required when service principal is configured",
		},
		{
			name: "managed identity authentication enabled",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: false,
		},
		{
			name: "arc authentication enabled",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					Arc: &ArcConfig{
						Enabled:       true,
						ResourceGroup: "test-rg",
						MachineName:   "test-machine",
						Location:      "eastus",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: false,
		},
		{
			name: "partial arc config fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					Arc: &ArcConfig{
						Enabled:     true,
						MachineName: "test-machine",
						Location:    "eastus",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: true,
			errMsg:  "azure.arc.resourceGroup is required when Arc is enabled",
		},
		{
			name: "arc and managed identity together fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					Arc: &ArcConfig{
						Enabled:       true,
						ResourceGroup: "test-rg",
						MachineName:   "test-machine",
						Location:      "eastus",
					},
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: true,
			errMsg:  "only one Azure authentication method can be enabled at a time",
		},
		{
			name: "bootstrap token and managed identity together passes",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "bootstrap token and service principal together passes",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					ServicePrincipal: &ServicePrincipalConfig{
						TenantID:     "12345678-1234-1234-1234-123456789012",
						ClientID:     "12345678-1234-1234-1234-123456789012",
						ClientSecret: "test-secret",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData:  "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "service principal and managed identity together fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					ServicePrincipal: &ServicePrincipalConfig{
						TenantID:     "12345678-1234-1234-1234-123456789012",
						ClientID:     "12345678-1234-1234-1234-123456789012",
						ClientSecret: "test-secret",
					},
					ManagedIdentity: &ManagedIdentityConfig{
						ClientID: "12345678-1234-1234-1234-123456789012",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: true,
			errMsg:  "only one Azure authentication method can be enabled at a time",
		},
		{
			name: "arc and service principal together fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					Arc: &ArcConfig{
						Enabled:       true,
						ResourceGroup: "test-rg",
						MachineName:   "test-machine",
						Location:      "eastus",
					},
					ServicePrincipal: &ServicePrincipalConfig{
						TenantID:     "12345678-1234-1234-1234-123456789012",
						ClientID:     "12345678-1234-1234-1234-123456789012",
						ClientSecret: "test-secret",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: true,
			errMsg:  "only one Azure authentication method can be enabled at a time",
		},
		{
			name: "no authentication method configured fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
			},
			wantErr: true,
			errMsg:  "at least one authentication method must be configured",
		},
		{
			name: "bootstrap token without clusterFQDN fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						CACertData: "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0R",
					},
				},
			},
			wantErr: true,
			errMsg:  "node.kubelet.clusterFQDN is required when using bootstrap token authentication",
		},
		{
			name: "bootstrap token without caCertData fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ClusterFQDN: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
					},
				},
			},
			wantErr: true,
			errMsg:  "node.kubelet.caCertData is required when using bootstrap token authentication",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestTargetAgentPoolName(tt.config)

			err := tt.config.validate()
			if tt.wantErr {
				if err == nil {
					t.Errorf("Validate() expected error but got none")
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Validate() error = %v, want error containing %v", err, tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("Validate() unexpected error = %v", err)
				}
			}
		})
	}
}

// baseAzureJSON is the minimal valid azure config block shared across label tests.
const baseAzureJSON = `{
	"azure": {
		"subscriptionId": "12345678-1234-1234-1234-123456789012",
		"tenantId": "12345678-1234-1234-1234-123456789012",
		"cloud": "AzurePublicCloud",
		"targetAgentPoolName": "aksflexnodes",
		"bootstrapToken": { "token": "abcdef.0123456789abcdef" },
		"targetCluster": {
			"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
			"location": "eastus"
		}
	},
	"node": {
		"kubelet": {
			"serverURL": "https://test-cluster.hcp.eastus.azmk8s.io:443",
			"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0t"
		},
		"labels": %s
	}
}`

// TestLoadConfigNodeLabels verifies that node label keys are preserved exactly
// as written in the config file. This is the root motivation: Kubernetes label
// keys such as "cleanroom.azure.com/flexnode" or "topology.kubernetes.io/zone"
// contain dots, which Viper's default "." key-path delimiter misinterpreted as
// nested JSON keys, silently dropping the labels on load.
func TestLoadConfigNodeLabels(t *testing.T) {
	tests := []struct {
		name           string
		labelsJSON     string
		expectedLabels map[string]string // only the user-supplied labels; defaults are checked separately
	}{
		{
			name:       "plain labels without dots",
			labelsJSON: `{"env": "production", "team": "platform"}`,
			expectedLabels: map[string]string{
				"env":  "production",
				"team": "platform",
			},
		},
		{
			name:       "label key with a single dot segment (kubernetes.io prefix)",
			labelsJSON: `{"kubernetes.io/nodeReady": "true"}`,
			expectedLabels: map[string]string{
				"kubernetes.io/nodeReady": "true",
			},
		},
		{
			name:       "label key with multiple dot segments (topology prefix)",
			labelsJSON: `{"topology.kubernetes.io/zone": "eastus-1"}`,
			expectedLabels: map[string]string{
				"topology.kubernetes.io/zone": "eastus-1",
			},
		},
		{
			name:       "label key with three dot segments (org.example.com prefix)",
			labelsJSON: `{"org.example.com/myLabel": "true"}`,
			expectedLabels: map[string]string{
				"org.example.com/myLabel": "true",
			},
		},
		{
			name: "mixed dotted and plain labels all preserved",
			labelsJSON: `{
				"env": "staging",
				"topology.kubernetes.io/zone": "eastus-1",
				"cleanroom.azure.com/flexnode": "true",
				"disktype": "ssd"
			}`,
			expectedLabels: map[string]string{
				"env":                          "staging",
				"topology.kubernetes.io/zone":  "eastus-1",
				"cleanroom.azure.com/flexnode": "true",
				"disktype":                     "ssd",
			},
		},
		{
			name:       "label value containing dots is preserved",
			labelsJSON: `{"version": "1.2.3"}`,
			expectedLabels: map[string]string{
				"version": "1.2.3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			configJSON := fmt.Sprintf(baseAzureJSON, tt.labelsJSON)
			configFile := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(configFile, []byte(configJSON), 0o600); err != nil {
				t.Fatalf("os.WriteFile: %v", err)
			}

			config, err := LoadConfig(configFile)
			if err != nil {
				t.Fatalf("LoadConfig() unexpected error: %v", err)
			}

			for key, want := range tt.expectedLabels {
				got, ok := config.Node.Labels[key]
				if !ok {
					t.Errorf("label %q not found; got keys: %v", key, labelKeys(config.Node.Labels))
				} else if got != want {
					t.Errorf("label %q = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// labelKeys returns sorted keys of a map for use in test diagnostics.
func labelKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestIsBootstrapTokenConfigured(t *testing.T) {
	tests := []struct {
		name     string
		config   *Config
		expected bool
	}{
		{
			name: "bootstrap token configured with valid token",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "abcdef.0123456789abcdef",
					},
				},
			},
			expected: true,
		},
		{
			name: "bootstrap token not configured (nil)",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: nil,
				},
			},
			expected: false,
		},
		{
			name: "bootstrap token selected with empty token",
			config: &Config{
				Azure: AzureConfig{
					BootstrapToken: &BootstrapTokenConfig{
						Token: "",
					},
				},
			},
			expected: true,
		},
		{
			name: "service principal configured (not bootstrap token)",
			config: &Config{
				Azure: AzureConfig{
					ServicePrincipal: &ServicePrincipalConfig{
						TenantID:     "12345678-1234-1234-1234-123456789012",
						ClientID:     "12345678-1234-1234-1234-123456789012",
						ClientSecret: "test-secret",
					},
				},
			},
			expected: false,
		},
		{
			name: "arc enabled (not bootstrap token)",
			config: &Config{
				Azure: AzureConfig{
					Arc: &ArcConfig{
						Enabled:       true,
						ResourceGroup: "test-rg",
						MachineName:   "test-machine",
						Location:      "eastus",
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.config.IsBootstrapTokenConfigured()
			if result != tt.expected {
				t.Errorf("IsBootstrapTokenConfigured() = %v, want %v", result, tt.expected)
			}
		})
	}
}
