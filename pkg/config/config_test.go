package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func caFromBase64(t testing.TB, s string) []byte {
	data, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(s)
	if err != nil {
		t.Fatalf("failed to decode base64 string: %v", err)
	}
	return data
}

const testCAData = "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0tCk1JSUREekNDQWZlZ0F3SUJBZ0lSQU1kbzBZa0"

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
				return c.Azure.Cloud == "AzurePublicCloud" &&
					c.Agent.LogLevel == "info" &&
					c.Agent.LogDir == "/var/log/aks-flex-node" &&
					c.Paths.Kubernetes.ConfigDir == "/etc/kubernetes" &&
					c.Node.MaxPods == 110 &&
					c.Runc.Version == "1.1.12"
			},
		},
		{
			name: "existing values are preserved",
			config: &Config{
				Azure: AzureConfig{
					Cloud: "AzurePublicCloud",
				},
				Agent: AgentConfig{
					LogLevel: "debug",
					LogDir:   "/custom/log/dir",
				},
			},
			want: func(c *Config) bool {
				return c.Agent.LogLevel == "debug" &&
					c.Agent.LogDir == "/custom/log/dir"
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
					c.Node.Kubelet.ImageGCLowThreshold == 80 &&
					c.Node.Kubelet.KubeReserved != nil &&
					c.Node.Kubelet.EvictionHard != nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.config.SetDefaults()
			if !tt.want(tt.config) {
				t.Errorf("SetDefaults() failed validation for %s", tt.name)
			}
		})
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing subscription ID fails",
			config: &Config{
				Azure: AzureConfig{
					TenantID: "12345678-1234-1234-1234-123456789012",
					Cloud:    "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.subscriptionId is required",
		},
		{
			name: "missing tenant ID fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.tenantId is required",
		},
		{
			name: "missing target cluster location fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "AzurePublicCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
					},
				},
			},
			wantErr: true,
			errMsg:  "azure.targetCluster.location is required",
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
			name: "invalid azure cloud fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
					Cloud:          "InvalidCloud",
					TargetCluster: &TargetClusterConfig{
						ResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						Location:   "eastus",
					},
				},
			},
			wantErr: true,
			errMsg:  "invalid azure.cloud: InvalidCloud. Valid values are: AzurePublicCloud",
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
			errMsg:  "invalid agent.logLevel: invalid. Valid values are: debug, info, warning, error",
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
			err := tt.config.Validate()
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
						"serverURL": "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
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
			}
		})
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
					},
				},
			},
			wantErr:   true,
			errString: "invalid bootstrap token format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBootstrapToken(tt.config)
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
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
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
				isMIExplicitlySet: true,
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
				isMIExplicitlySet: true,
			},
			wantErr: true,
			errMsg:  "only one authentication method can be enabled at a time",
		},
		{
			name: "bootstrap token and service principal together fails",
			config: &Config{
				Azure: AzureConfig{
					SubscriptionID: "12345678-1234-1234-1234-123456789012",
					TenantID:       "12345678-1234-1234-1234-123456789012",
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
						Location:   "eastus",
					},
				},
				Agent: AgentConfig{
					LogLevel: "info",
				},
				Node: NodeConfig{
					Kubelet: KubeletConfig{
						ServerURL:  "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
						CACertData: caFromBase64(t, testCAData),
					},
				},
			},
			wantErr: true,
			errMsg:  "only one authentication method can be enabled at a time",
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
			errMsg:  "only one authentication method can be enabled at a time",
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
			name: "bootstrap token without serverURL fails",
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
						CACertData: caFromBase64(t, testCAData),
					},
				},
			},
			wantErr: true,
			errMsg:  "node.kubelet.serverURL is required when using bootstrap token authentication",
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
						ServerURL: "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443",
					},
				},
			},
			wantErr: true,
			errMsg:  "node.kubelet.caCertData is required when using bootstrap token authentication",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
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
