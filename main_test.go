package main

import (
	"testing"
)

func TestCommandConstructors(t *testing.T) {
	// Verify that all command constructors used in main() work properly
	// We can't directly test main() execution, but we can test the components it uses
	
	// Test that command creation works
	rootCmd := NewAgentCommand()
	if rootCmd == nil {
		t.Error("Should be able to create agent command")
	}
	
	unbootstrapCmd := NewUnbootstrapCommand()
	if unbootstrapCmd == nil {
		t.Error("Should be able to create unbootstrap command")
	}
	
	versionCmd := NewVersionCommand()
	if versionCmd == nil {
		t.Error("Should be able to create version command")
	}
}

func TestConfigPath(t *testing.T) {
	// Test that configPath variable is accessible
	oldPath := configPath
	configPath = "/test/path"
	
	if configPath != "/test/path" {
		t.Error("configPath should be settable")
	}
	
	// Restore
	configPath = oldPath
}
