package logger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestSetupLogger(t *testing.T) {
	// Create a temporary directory for testing
	tempDir := t.TempDir()

	tests := []struct {
		name    string
		level   string
		logDir  string
		wantErr bool
	}{
		{
			name:    "Valid info level with log directory",
			level:   "info",
			logDir:  tempDir,
			wantErr: false,
		},
		{
			name:    "Valid debug level without log directory",
			level:   "debug",
			logDir:  "",
			wantErr: false,
		},
		{
			name:    "Invalid level with log directory",
			level:   "invalid",
			logDir:  tempDir,
			wantErr: false, // Should fallback to info level
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = SetupLogger(ctx, tt.level, tt.logDir)

			logger := GetLoggerFromContext(ctx)
			if logger == nil {
				t.Error("Logger should not be nil")
				return
			}

			// Test that we can log messages
			logger.Info("Test info message")
			logger.Debug("Test debug message")
			logger.Warn("Test warning message")
			logger.Error("Test error message")

			// If log directory was specified, check that log file exists
			if tt.logDir != "" {
				logFile := filepath.Join(tt.logDir, "aks-flex-node.log")
				// Give some time for file operations to complete
				time.Sleep(100 * time.Millisecond)

				if _, err := os.Stat(logFile); os.IsNotExist(err) {
					t.Errorf("Log file should exist at %s", logFile)
				}
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		expected  logrus.Level
		expectErr bool
	}{
		{"debug level", "debug", logrus.DebugLevel, false},
		{"info level", "info", logrus.InfoLevel, false},
		{"warning level", "warning", logrus.WarnLevel, false},
		{"error level", "error", logrus.ErrorLevel, false},
		{"case insensitive", "DEBUG", logrus.DebugLevel, false},
		{"with spaces", "  info  ", logrus.InfoLevel, false},
		{"invalid level", "invalid", logrus.InfoLevel, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, err := ParseLogLevel(tt.level)

			if tt.expectErr && err == nil {
				t.Error("Expected error but got none")
			}

			if !tt.expectErr && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if level != tt.expected {
				t.Errorf("Expected level %v, got %v", tt.expected, level)
			}
		})
	}
}

func TestValidateLogLevel(t *testing.T) {
	validLevels := []string{"debug", "info", "warning", "error", "DEBUG", "INFO"}
	invalidLevels := []string{"invalid", "", "trace", "fatal"}

	for _, level := range validLevels {
		t.Run("valid_"+level, func(t *testing.T) {
			if err := ValidateLogLevel(level); err != nil {
				t.Errorf("Level %s should be valid, got error: %v", level, err)
			}
		})
	}

	for _, level := range invalidLevels {
		t.Run("invalid_"+level, func(t *testing.T) {
			if err := ValidateLogLevel(level); err == nil {
				t.Errorf("Level %s should be invalid", level)
			}
		})
	}
}

func TestLogLevelHelpers(t *testing.T) {
	tempDir := t.TempDir()
	ctx := SetupLogger(context.Background(), "debug", tempDir)

	helpers := NewLogLevelHelpers(ctx)

	// Test that all helper methods work without panicking
	helpers.Debug("Debug message")
	helpers.Debugf("Debug message %s", "formatted")
	helpers.Info("Info message")
	helpers.Infof("Info message %s", "formatted")
	helpers.Warning("Warning message")
	helpers.Warningf("Warning message %s", "formatted")
	helpers.Error("Error message")
	helpers.Errorf("Error message %s", "formatted")
}

func TestSystemdDetection(t *testing.T) {
	// Test systemd detection function
	// Note: This will depend on the test environment
	detected := isRunningUnderSystemd()

	// We can't assert a specific value since it depends on the environment
	// But we can verify the function doesn't panic
	t.Logf("Systemd detected: %v", detected)
}

func TestGetCurrentLogLevel(t *testing.T) {
	ctx := SetupLogger(context.Background(), "warning", "")
	level := GetCurrentLogLevel(ctx)

	if level != "warning" {
		t.Errorf("Expected warning level, got %s", level)
	}
}

func TestIsDebugEnabled(t *testing.T) {
	// Test with debug enabled
	ctx := SetupLogger(context.Background(), "debug", "")
	if !IsDebugEnabled(ctx) {
		t.Error("Debug should be enabled for debug level")
	}

	// Test with debug disabled
	ctx = SetupLogger(context.Background(), "error", "")
	if IsDebugEnabled(ctx) {
		t.Error("Debug should be disabled for error level")
	}
}
