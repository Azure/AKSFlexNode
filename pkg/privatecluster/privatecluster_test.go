package privatecluster

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
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
	logger := logrus.New()
	installer := NewInstaller(logger)
	if installer == nil {
		t.Fatal("NewInstaller() should not return nil")
	}
	if installer.logger != logger {
		t.Error("Installer.logger should match the provided logger")
	}
}

func TestInstallerGetName(t *testing.T) {
	installer := NewInstaller(logrus.New())
	if name := installer.GetName(); name != "PrivateClusterInstall" {
		t.Errorf("GetName() = %v, want PrivateClusterInstall", name)
	}
}

func TestInstallerIsCompletedNonPrivate(t *testing.T) {
	// When config is nil (non-private cluster), IsCompleted should return true
	installer := NewInstaller(logrus.New())
	installer.config = nil
	if !installer.IsCompleted(context.Background()) {
		t.Error("IsCompleted() should return true for non-private cluster")
	}
}

func TestUninstallerCreation(t *testing.T) {
	logger := logrus.New()
	uninstaller := NewUninstaller(logger)
	if uninstaller == nil {
		t.Fatal("NewUninstaller() should not return nil")
	}
	if uninstaller.logger != logger {
		t.Error("Uninstaller.logger should match the provided logger")
	}
}

func TestUninstallerGetName(t *testing.T) {
	uninstaller := NewUninstaller(logrus.New())
	if name := uninstaller.GetName(); name != "PrivateClusterUninstall" {
		t.Errorf("GetName() = %v, want PrivateClusterUninstall", name)
	}
}

func TestUninstallerIsCompletedNonPrivate(t *testing.T) {
	// When config is nil (non-private cluster), IsCompleted should return true
	uninstaller := NewUninstaller(logrus.New())
	uninstaller.config = nil
	if !uninstaller.IsCompleted(context.Background()) {
		t.Error("IsCompleted() should return true for non-private cluster")
	}
}
