package main

import (
	"testing"
)

// TestNewAgentCommand verifies that the agent command is created properly with all required fields.
// Test: Creates an agent command and validates its structure
// Expected: Command should be non-nil with Use="agent", non-empty descriptions, and RunE function set
func TestNewAgentCommand(t *testing.T) {
	cmd := NewAgentCommand()

	if cmd == nil {
		t.Fatal("NewAgentCommand should not return nil")
	}

	if cmd.Use != "agent" {
		t.Errorf("Expected Use to be 'agent', got '%s'", cmd.Use)
	}

	if cmd.Short == "" {
		t.Error("Short description should not be empty")
	}

	if cmd.Long == "" {
		t.Error("Long description should not be empty")
	}

	if cmd.RunE == nil {
		t.Error("RunE should be set")
	}
}

// TestNewUnbootstrapCommand verifies that the unbootstrap command is created properly with all required fields.
// Test: Creates an unbootstrap command and validates its structure
// Expected: Command should be non-nil with Use="unbootstrap", non-empty descriptions, and RunE function set
func TestNewUnbootstrapCommand(t *testing.T) {
	cmd := NewUnbootstrapCommand()

	if cmd == nil {
		t.Fatal("NewUnbootstrapCommand should not return nil")
	}

	if cmd.Use != "unbootstrap" {
		t.Errorf("Expected Use to be 'unbootstrap', got '%s'", cmd.Use)
	}

	if cmd.Short == "" {
		t.Error("Short description should not be empty")
	}

	if cmd.Long == "" {
		t.Error("Long description should not be empty")
	}

	if cmd.RunE == nil {
		t.Error("RunE should be set")
	}
}

// TestNewVersionCommand verifies that the version command is created properly with all required fields.
// Test: Creates a version command and validates its structure
// Expected: Command should be non-nil with Use="version", non-empty descriptions, and Run function set
func TestNewVersionCommand(t *testing.T) {
	cmd := NewVersionCommand()

	if cmd == nil {
		t.Fatal("NewVersionCommand should not return nil")
	}

	if cmd.Use != "version" {
		t.Errorf("Expected Use to be 'version', got '%s'", cmd.Use)
	}

	if cmd.Short == "" {
		t.Error("Short description should not be empty")
	}

	if cmd.Long == "" {
		t.Error("Long description should not be empty")
	}

	if cmd.Run == nil {
		t.Error("Run should be set")
	}
}

// TestVersionVariables verifies that version variables (set at build time via ldflags) can be modified.
// Test: Saves current values, sets test values, verifies they're set correctly, then restores originals
// Expected: All three variables (Version, GitCommit, BuildTime) should be settable and readable
func TestVersionVariables(t *testing.T) {
	// Test that version variables can be set
	oldVersion := Version
	oldGitCommit := GitCommit
	oldBuildTime := BuildTime

	Version = "test-version"
	GitCommit = "test-commit"
	BuildTime = "test-time"

	if Version != "test-version" {
		t.Error("Version should be settable")
	}

	if GitCommit != "test-commit" {
		t.Error("GitCommit should be settable")
	}

	if BuildTime != "test-time" {
		t.Error("BuildTime should be settable")
	}

	// Restore original values
	Version = oldVersion
	GitCommit = oldGitCommit
	BuildTime = oldBuildTime
}

// TestAllCommands is a table-driven test that verifies all CLI commands can be created without errors.
// Test: Iterates through all command types (agent, unbootstrap, version) and creates each one
// Expected: All commands should be created successfully and return non-nil objects
func TestAllCommands(t *testing.T) {
	// Verify all command constructors work properly
	tests := []struct {
		name string
		cmd  string
	}{
		{"agent command exists", "agent"},
		{"unbootstrap command exists", "unbootstrap"},
		{"version command exists", "version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cmd interface{}
			switch tt.cmd {
			case "agent":
				cmd = NewAgentCommand()
			case "unbootstrap":
				cmd = NewUnbootstrapCommand()
			case "version":
				cmd = NewVersionCommand()
			}

			if cmd == nil {
				t.Errorf("Command %s should not be nil", tt.cmd)
			}
		})
	}
}
