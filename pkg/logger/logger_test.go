package logger

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateLogger(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name   string
		level  string
		logDir string
	}{
		{
			name:   "Valid info level with log directory",
			level:  "info",
			logDir: tempDir,
		},
		{
			name:   "Valid debug level without log directory",
			level:  "debug",
			logDir: "",
		},
		{
			name:   "Invalid level with log directory",
			level:  "invalid",
			logDir: tempDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			CreateLogger(tt.level, tt.logDir)

			log := slog.Default()
			if log == nil {
				t.Error("slog.Default() should not be nil")
				return
			}

			log.Info("Test info message")
			log.Debug("Test debug message")
			log.Warn("Test warning message")
			log.Error("Test error message")

			if tt.logDir != "" {
				logFile := filepath.Join(tt.logDir, "aks-flex-node.log")
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
		expected  slog.Level
		expectErr bool
	}{
		{"debug level", "debug", slog.LevelDebug, false},
		{"info level", "info", slog.LevelInfo, false},
		{"warning level", "warning", slog.LevelWarn, false},
		{"error level", "error", slog.LevelError, false},
		{"case insensitive", "DEBUG", slog.LevelDebug, false},
		{"with spaces", "  info  ", slog.LevelInfo, false},
		{"invalid level", "invalid", slog.LevelInfo, true},
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

func TestSystemdDetection(t *testing.T) {
	detected := isRunningUnderSystemd()
	t.Logf("Systemd detected: %v", detected)
}
