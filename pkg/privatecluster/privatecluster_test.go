package privatecluster

import (
	"testing"
)

func TestParseResourceID(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		wantSubID  string
		wantRG     string
		wantName   string
		wantErr    bool
	}{
		{
			name:       "valid resource ID",
			resourceID: "/subscriptions/549c6279-3a6a-4412-b267-b4da1afbe002/resourceGroups/weiliu2testrg/providers/Microsoft.ContainerService/managedClusters/my-private-aks",
			wantSubID:  "549c6279-3a6a-4412-b267-b4da1afbe002",
			wantRG:     "weiliu2testrg",
			wantName:   "my-private-aks",
			wantErr:    false,
		},
		{
			name:       "lowercase resourcegroups",
			resourceID: "/subscriptions/549c6279-3a6a-4412-b267-b4da1afbe002/resourcegroups/weiliu2testrg/providers/Microsoft.ContainerService/managedClusters/my-private-aks",
			wantSubID:  "549c6279-3a6a-4412-b267-b4da1afbe002",
			wantRG:     "weiliu2testrg",
			wantName:   "my-private-aks",
			wantErr:    false,
		},
		{
			name:       "invalid resource ID - too short",
			resourceID: "/subscriptions/xxx",
			wantErr:    true,
		},
		{
			name:       "empty resource ID",
			resourceID: "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subID, rg, name, err := ParseResourceID(tt.resourceID)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseResourceID() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if subID != tt.wantSubID {
					t.Errorf("ParseResourceID() subID = %v, want %v", subID, tt.wantSubID)
				}
				if rg != tt.wantRG {
					t.Errorf("ParseResourceID() rg = %v, want %v", rg, tt.wantRG)
				}
				if name != tt.wantName {
					t.Errorf("ParseResourceID() name = %v, want %v", name, tt.wantName)
				}
			}
		})
	}
}

func TestDefaultConfigs(t *testing.T) {
	// Test DefaultGatewayConfig
	gw := DefaultGatewayConfig()
	if gw.Name != "wg-gateway" {
		t.Errorf("DefaultGatewayConfig().Name = %v, want wg-gateway", gw.Name)
	}
	if gw.Port != 51820 {
		t.Errorf("DefaultGatewayConfig().Port = %v, want 51820", gw.Port)
	}

	// Test DefaultVPNConfig
	vpn := DefaultVPNConfig()
	if vpn.NetworkInterface != "wg-aks" {
		t.Errorf("DefaultVPNConfig().NetworkInterface = %v, want wg-aks", vpn.NetworkInterface)
	}
	if vpn.GatewayVPNIP != "172.16.0.1" {
		t.Errorf("DefaultVPNConfig().GatewayVPNIP = %v, want 172.16.0.1", vpn.GatewayVPNIP)
	}
}

func TestLogger(t *testing.T) {
	// Just test that logger doesn't panic
	logger := NewLogger(false)
	logger.Info("test info")
	logger.Success("test success")
	logger.Warning("test warning")
	logger.Error("test error")
	logger.Verbose("should not print") // verbose=false

	loggerVerbose := NewLogger(true)
	loggerVerbose.Verbose("should print")
}

func TestFileExists(t *testing.T) {
	// Test with existing file
	if !FileExists("types.go") {
		t.Error("FileExists() should return true for types.go")
	}

	// Test with non-existing file
	if FileExists("nonexistent_file_12345.go") {
		t.Error("FileExists() should return false for non-existent file")
	}
}

func TestCommandExists(t *testing.T) {
	// Test with common command
	if !CommandExists("ls") {
		t.Error("CommandExists() should return true for 'ls'")
	}

	// Test with non-existing command
	if CommandExists("nonexistent_command_12345") {
		t.Error("CommandExists() should return false for non-existent command")
	}
}

func TestInstallerCreation(t *testing.T) {
	options := InstallOptions{
		AKSResourceID: "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		Verbose:       true,
	}

	// NewInstaller requires a credential; pass nil to test creation without Azure calls
	installer, err := NewInstaller(options, nil)
	// Expected to fail since nil credential can't create Azure clients
	if err != nil {
		t.Skipf("Skipping: NewInstaller requires valid Azure credential: %v", err)
	}
	if installer == nil {
		t.Fatal("NewInstaller() should not return nil")
	}
	if installer.logger == nil {
		t.Error("Installer.logger should not be nil")
	}
}

func TestUninstallerCreation(t *testing.T) {
	options := UninstallOptions{
		Mode:          CleanupModeLocal,
		AKSResourceID: "",
	}

	// NewUninstaller with empty resource ID and nil cred skips Azure client creation
	uninstaller, err := NewUninstaller(options, nil)
	if err != nil {
		t.Fatalf("NewUninstaller() returned error: %v", err)
	}
	if uninstaller == nil {
		t.Error("NewUninstaller() should not return nil")
	}
}
