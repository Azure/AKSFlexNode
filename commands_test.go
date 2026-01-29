package main

import (
	"testing"
)

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
