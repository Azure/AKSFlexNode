package privatecluster

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
)

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
