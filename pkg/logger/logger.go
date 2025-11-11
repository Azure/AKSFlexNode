package logger

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Context key for storing logger
type contextKey string

const loggerContextKey contextKey = "aks-flex-node-logger"

// LogLevel represents supported logging levels
type LogLevel string

const (
	// LogLevelDebug enables debug, info, warning, and error messages
	LogLevelDebug LogLevel = "debug"
	// LogLevelInfo enables info, warning, and error messages
	LogLevelInfo LogLevel = "info"
	// LogLevelWarning enables warning and error messages
	LogLevelWarning LogLevel = "warning"
	// LogLevelError enables only error messages
	LogLevelError LogLevel = "error"
)

// ValidLogLevels contains all supported log levels
var ValidLogLevels = map[string]LogLevel{
	"debug":   LogLevelDebug,
	"info":    LogLevelInfo,
	"warning": LogLevelWarning,
	"error":   LogLevelError,
}

// ValidateLogLevel validates if the provided log level is supported
func ValidateLogLevel(level string) error {
	normalizedLevel := strings.ToLower(strings.TrimSpace(level))
	if _, valid := ValidLogLevels[normalizedLevel]; !valid {
		return fmt.Errorf("invalid log level '%s'. Valid levels are: debug, info, warning, error", level)
	}
	return nil
}

// ParseLogLevel converts string log level to logrus.Level with validation
func ParseLogLevel(level string) (logrus.Level, error) {
	normalizedLevel := strings.ToLower(strings.TrimSpace(level))

	switch normalizedLevel {
	case "debug":
		return logrus.DebugLevel, nil
	case "info":
		return logrus.InfoLevel, nil
	case "warning":
		return logrus.WarnLevel, nil
	case "error":
		return logrus.ErrorLevel, nil
	default:
		return logrus.InfoLevel, fmt.Errorf("invalid log level '%s'. Valid levels are: debug, info, warning, error", level)
	}
}

// SetupLogger creates a logger with specified level and optional log directory
// For systemd services, it supports dual output to both journal (stdout) and file
func SetupLogger(ctx context.Context, level, logDir string) context.Context {
	logger := logrus.New()

	// Set log level with proper validation
	logLevel, err := ParseLogLevel(level)
	if err != nil {
		// Log the error but continue with default level
		fmt.Printf("Warning: %v. Using 'info' level as default.\n", err)
		logLevel = logrus.InfoLevel
	}
	logger.SetLevel(logLevel)

	// Configure log formatter for systemd compatibility
	logger.SetReportCaller(true)

	// Detect if running under systemd (check for journal environment)
	isSystemdService := os.Getenv("JOURNAL_STREAM") != "" || isRunningUnderSystemd()

	if isSystemdService {
		// For systemd services, use a simpler formatter optimized for journald
		logger.SetFormatter(&logrus.TextFormatter{
			DisableTimestamp: true, // systemd journal adds timestamps
			DisableColors:    false,
			CallerPrettyfier: func(f *runtime.Frame) (string, string) {
				filename := filepath.Base(f.File)
				return fmt.Sprintf("[%s:%d]", filename, f.Line), ""
			},
		})

		// Set up dual output: journal (stdout) and optional log file
		writers := []io.Writer{os.Stdout}

		if logDir != "" {
			if fileWriter, err := setupLogFileWriter(logDir); err != nil {
				fmt.Printf("Warning: Failed to setup log file in directory '%s': %v. Logging to journal only.\n", logDir, err)
			} else {
				writers = append(writers, fileWriter)
			}
		}

		logger.SetOutput(io.MultiWriter(writers...))
	} else {
		// For non-systemd environments, use the original formatting with colors enabled
		logger.SetFormatter(&logrus.TextFormatter{
			TimestampFormat: "2006-01-02 15:04:05",
			FullTimestamp:   true,
			ForceColors:     true, // Enable colors for terminal output
			CallerPrettyfier: func(f *runtime.Frame) (string, string) {
				filename := filepath.Base(f.File)
				return fmt.Sprintf("[%s:%d]", filename, f.Line), ""
			},
		})

		// Set up log file if directory is specified
		if logDir != "" {
			if err := setupLogFile(logger, logDir); err != nil {
				fmt.Printf("Warning: Failed to setup log file in directory '%s': %v. Logging to console.\n", logDir, err)
			}
		}
	}

	return context.WithValue(ctx, loggerContextKey, logger)
}

// isRunningUnderSystemd detects if the process is running under systemd
func isRunningUnderSystemd() bool {
	// Check if systemd is the init system (PID 1)
	if data, err := os.ReadFile("/proc/1/comm"); err == nil {
		return strings.TrimSpace(string(data)) == "systemd"
	}
	return false
}

// setupLogFileWriter creates a file writer for the specified log directory
// This is used for systemd dual-output logging (journal + file)
func setupLogFileWriter(logDir string) (io.Writer, error) {
	// Ensure the log directory exists first
	if err := ensureLogDirectoryExists(logDir); err != nil {
		return nil, fmt.Errorf("failed to create log directory '%s': %w", logDir, err)
	}

	logFilePath := filepath.Join(logDir, "aks-flex-node.log")

	// Create the log file if it doesn't exist
	if err := createLogFileIfNotExists(logFilePath); err != nil {
		return nil, fmt.Errorf("failed to create log file '%s': %w", logFilePath, err)
	}

	// Try to open log file for writing, handle permission issues
	file, err := os.OpenFile(logFilePath, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		// If it's a permission error and we're not running as root, try to fix permissions
		if os.IsPermission(err) {
			// Try to fix permissions using system command
			if fixErr := utils.RunSystemCommand("chmod", "666", logFilePath); fixErr == nil {
				// Retry opening the file after fixing permissions
				file, err = os.OpenFile(logFilePath, os.O_WRONLY|os.O_APPEND, 0666)
				if err == nil {
					return file, nil
				}
			}
		}
		return nil, fmt.Errorf("failed to open log file '%s': %w", logFilePath, err)
	}

	return file, nil
}

// ensureLogDirectoryExists creates the log directory if it doesn't exist
func ensureLogDirectoryExists(logDir string) error {
	// Check if directory already exists
	if _, err := os.Stat(logDir); err == nil {
		return nil // Directory already exists
	}

	// Try to create directory with appropriate permissions
	if err := os.MkdirAll(logDir, 0755); err == nil {
		return nil // Successfully created directory
	}

	// If direct creation fails, try using system command for privileged paths
	if err := utils.RunSystemCommand("mkdir", "-p", logDir); err != nil {
		return fmt.Errorf("failed to create directory using system command: %w", err)
	}

	// Set appropriate permissions on the created directory
	if err := utils.RunSystemCommand("chmod", "755", logDir); err != nil {
		fmt.Printf("Warning: Failed to set permissions on directory %s: %v\n", logDir, err)
	}

	return nil
}

// setupLogFile creates log file in the specified directory (legacy method for non-systemd)
func setupLogFile(logger *logrus.Logger, logDir string) error {
	// Ensure the log directory exists first
	if err := ensureLogDirectoryExists(logDir); err != nil {
		return fmt.Errorf("failed to create log directory '%s': %w", logDir, err)
	}

	writer, err := setupLogFileWriter(logDir)
	if err != nil {
		return err
	}
	logger.SetOutput(writer)
	return nil
}

// createLogFileIfNotExists creates a log file using appropriate method based on path privileges
func createLogFileIfNotExists(logFilePath string) error {
	// Check if file already exists
	if utils.FileExists(logFilePath) {
		return nil
	}

	// For systemd services, try direct file creation first since the service
	// should have the correct user/group and the log directory should already exist
	if isRunningUnderSystemd() {
		// Try direct file creation with appropriate permissions
		file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			file.Close()
			return nil
		}
		// If direct creation fails, fall through to the system method
		fmt.Printf("Warning: Direct log file creation failed (%v), trying system method...\n", err)
	}

	// Use WriteFileAtomicSystem to create an empty log file with proper permissions
	// This handles all the sudo logic and system path handling automatically
	if err := utils.WriteFileAtomicSystem(logFilePath, []byte{}, 0644); err != nil {
		return err
	}

	// Ensure proper ownership for the current user after file creation
	// Skip this for systemd services as they should already have correct ownership
	if !isRunningUnderSystemd() {
		currentUser := os.Getenv("USER")
		if currentUser != "" {
			if err := utils.RunSystemCommand("chown", currentUser+":"+currentUser, logFilePath); err != nil {
				fmt.Printf("Warning: Failed to change ownership of %s to %s: %v\n", logFilePath, currentUser, err)
			}
		}
	}

	return nil
}

// GetLoggerFromContext retrieves the logger from context
func GetLoggerFromContext(ctx context.Context) *logrus.Logger {
	if logger, ok := ctx.Value(loggerContextKey).(*logrus.Logger); ok {
		return logger
	}
	// Fallback to default logger if not found in context
	return logrus.New()
}

// GetCurrentLogLevel returns the current log level as a string
func GetCurrentLogLevel(ctx context.Context) string {
	logger := GetLoggerFromContext(ctx)
	switch logger.GetLevel() {
	case logrus.DebugLevel:
		return "debug"
	case logrus.InfoLevel:
		return "info"
	case logrus.WarnLevel:
		return "warning"
	case logrus.ErrorLevel:
		return "error"
	default:
		return "unknown"
	}
}

// IsDebugEnabled checks if debug logging is enabled
func IsDebugEnabled(ctx context.Context) bool {
	logger := GetLoggerFromContext(ctx)
	return logger.IsLevelEnabled(logrus.DebugLevel)
}

// LogLevelHelpers provides convenient logging functions with proper context
type LogLevelHelpers struct {
	ctx context.Context
}

// NewLogLevelHelpers creates a new helper instance with context
func NewLogLevelHelpers(ctx context.Context) *LogLevelHelpers {
	return &LogLevelHelpers{ctx: ctx}
}

// Debug logs a debug message if debug level is enabled
func (h *LogLevelHelpers) Debug(args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Debug(args...)
}

// Debugf logs a formatted debug message if debug level is enabled
func (h *LogLevelHelpers) Debugf(format string, args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Debugf(format, args...)
}

// Info logs an info message
func (h *LogLevelHelpers) Info(args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Info(args...)
}

// Infof logs a formatted info message
func (h *LogLevelHelpers) Infof(format string, args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Infof(format, args...)
}

// Warning logs a warning message
func (h *LogLevelHelpers) Warning(args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Warn(args...)
}

// Warningf logs a formatted warning message
func (h *LogLevelHelpers) Warningf(format string, args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Warnf(format, args...)
}

// Error logs an error message
func (h *LogLevelHelpers) Error(args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Error(args...)
}

// Errorf logs a formatted error message
func (h *LogLevelHelpers) Errorf(format string, args ...interface{}) {
	logger := GetLoggerFromContext(h.ctx)
	logger.Errorf(format, args...)
}
