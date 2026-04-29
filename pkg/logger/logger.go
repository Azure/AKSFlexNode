package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// parseLogLevel converts a string log level to slog.Level with validation.
func parseLogLevel(level string) (slog.Level, error) {
	normalizedLevel := strings.ToLower(strings.TrimSpace(level))

	switch normalizedLevel {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level '%s'. Valid levels are: debug, info, warning, error", level)
	}
}

// CreateLogger creates and returns a *slog.Logger with the specified level
// and optional log directory for file output.
func CreateLogger(level, logDir string) *slog.Logger {
	logLevel, err := parseLogLevel(level)
	if err != nil {
		fmt.Printf("Warning: %v. Using 'info' level as default.\n", err)
		logLevel = slog.LevelInfo
	}

	levelVar := &slog.LevelVar{}
	levelVar.Set(logLevel)

	// Build the list of writers. Stdout is always included for journal/terminal.
	writers := []io.Writer{os.Stdout}
	if logDir != "" {
		if fileWriter, err := setupLogFileWriter(logDir); err != nil {
			fmt.Printf("Warning: Failed to setup log file in directory '%s': %v. Logging to stdout only.\n", logDir, err)
		} else {
			writers = append(writers, fileWriter)
		}
	}

	w := io.MultiWriter(writers...)

	// Detect if running under systemd (check for journal environment)
	isSystemdService := os.Getenv("JOURNAL_STREAM") != "" || isRunningUnderSystemd()

	var handler slog.Handler
	if isSystemdService {
		// For systemd services, omit timestamps (journald adds them).
		handler = slog.NewTextHandler(w, &slog.HandlerOptions{
			Level: levelVar,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					return slog.Attr{} // drop timestamp
				}
				return a
			},
		})
	} else {
		handler = slog.NewTextHandler(w, &slog.HandlerOptions{
			Level: levelVar,
		})
	}

	return slog.New(handler)
}

// isRunningUnderSystemd detects if the process is running under systemd
func isRunningUnderSystemd() bool {
	if data, err := os.ReadFile("/proc/1/comm"); err == nil {
		return strings.TrimSpace(string(data)) == "systemd"
	}
	return false
}

// setupLogFileWriter creates a file writer for the specified log directory
func setupLogFileWriter(logDir string) (io.Writer, error) {
	if err := ensureLogDirectoryExists(logDir); err != nil {
		return nil, fmt.Errorf("failed to create log directory '%s': %w", logDir, err)
	}

	logFilePath := filepath.Join(logDir, "aks-flex-node.log")
	file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) //#nosec G304 - logFilePath is from trusted agent config
	if err != nil {
		return nil, fmt.Errorf("failed to open log file '%s': %w", logFilePath, err)
	}

	return file, nil
}

// ensureLogDirectoryExists creates the log directory if it doesn't exist
func ensureLogDirectoryExists(logDir string) error {
	if _, err := os.Stat(logDir); err == nil {
		return nil
	}
	if err := os.MkdirAll(logDir, 0750); err != nil {
		return err
	}
	return nil
}
