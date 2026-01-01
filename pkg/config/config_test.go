package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
					c.Containerd.Version == "1.7.20" &&
					c.Kubernetes.Version == "1.32.7" &&
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
					"targetCluster": {
						"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
						"location": "eastus"
					}
				},
				"agent": {
					"logLevel": "debug"
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
			if err := os.WriteFile(configFile, []byte(tt.configJSON), 0644); err != nil {
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
