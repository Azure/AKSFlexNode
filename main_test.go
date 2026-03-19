package main

import (
	"testing"
)

// TestCommandConstructors verifies that all command constructors used by main() function work correctly.
// Test: Creates agent, unbootstrap, and version commands to ensure main() dependencies are functional
// Expected: All command constructors should return non-nil cobra.Command objects
// Note: Cannot directly test main() execution as it handles signals and runs indefinitely
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

// TestConfigPath verifies that the global configPath variable can be set and retrieved.
// Test: Saves current value, sets a test path, verifies it's set correctly, then restores original
// Expected: configPath variable should be readable and writable (used for --config flag)
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
