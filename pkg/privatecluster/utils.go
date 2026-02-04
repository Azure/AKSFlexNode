package privatecluster

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Color codes for terminal output
const (
	colorRed    = "\033[0;31m"
	colorGreen  = "\033[0;32m"
	colorYellow = "\033[1;33m"
	colorBlue   = "\033[0;34m"
	colorReset  = "\033[0m"
)

// Logger provides colored logging for the private cluster operations
type Logger struct {
	verbose bool
}

// NewLogger creates a new Logger instance
func NewLogger(verbose bool) *Logger {
	return &Logger{verbose: verbose}
}

// Info logs an info message
func (l *Logger) Info(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%sINFO:%s %s\n", colorBlue, colorReset, msg)
}

// Success logs a success message
func (l *Logger) Success(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%sSUCCESS:%s %s\n", colorGreen, colorReset, msg)
}

// Warning logs a warning message
func (l *Logger) Warning(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%sWARNING:%s %s\n", colorYellow, colorReset, msg)
}

// Error logs an error message
func (l *Logger) Error(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%sERROR:%s %s\n", colorRed, colorReset, msg)
}

// Verbose logs a verbose message (only if verbose mode is enabled)
func (l *Logger) Verbose(format string, args ...interface{}) {
	if l.verbose {
		msg := fmt.Sprintf(format, args...)
		fmt.Printf("%sVERBOSE:%s %s\n", colorBlue, colorReset, msg)
	}
}

// RunCommand executes a command and returns its output
func RunCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- commands are from trusted internal code
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("command '%s %s' failed: %w\nOutput: %s",
			name, strings.Join(args, " "), err, string(output))
	}
	return strings.TrimSpace(string(output)), nil
}

// RunCommandSilent executes a command and returns only whether it succeeded
func RunCommandSilent(ctx context.Context, name string, args ...string) bool {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- commands are from trusted internal code
	return cmd.Run() == nil
}

// RunCommandInteractive executes a command with stdout/stderr/stdin connected to the terminal
func RunCommandInteractive(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- commands are from trusted internal code
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// CommandExists checks if a command is available in PATH
func CommandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// GetRealHome returns the real user's home directory (handles sudo)
func GetRealHome() string {
	// Check if running with sudo
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			return u.HomeDir
		}
	}
	// Fallback to current user's home
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return "/root"
}

// GetSSHKeyPath returns the default SSH key path for the Gateway
func GetSSHKeyPath() string {
	return filepath.Join(GetRealHome(), ".ssh", "id_rsa_wg_gateway")
}

// EnsureDirectory creates a directory if it doesn't exist
func EnsureDirectory(path string) error {
	return os.MkdirAll(path, 0750)
}

// FileExists checks if a file exists
func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ReadFileContent reads a file and returns its content
func ReadFileContent(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is from trusted internal code
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteFileContent writes content to a file with specified permissions
func WriteFileContent(path, content string, perm os.FileMode) error {
	return os.WriteFile(path, []byte(content), perm) // #nosec G306 -- perm is from trusted internal code
}

// AddHostsEntry adds an entry to /etc/hosts if it doesn't exist
func AddHostsEntry(ip, hostname string) error {
	hostsPath := "/etc/hosts"

	// Check if entry already exists
	content, err := ReadFileContent(hostsPath)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	if strings.Contains(content, hostname) {
		return nil // Entry already exists
	}

	f, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0644) // #nosec G302,G304 -- /etc/hosts requires 0644
	if err != nil {
		return fmt.Errorf("failed to open hosts file: %w", err)
	}
	defer func() { _ = f.Close() }()

	entry := fmt.Sprintf("%s %s\n", ip, hostname)
	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("failed to write hosts entry: %w", err)
	}

	return nil
}

// RemoveHostsEntries removes entries matching a pattern from /etc/hosts
func RemoveHostsEntries(pattern string) error {
	hostsPath := "/etc/hosts"

	content, err := ReadFileContent(hostsPath)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	var newLines []string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, pattern) {
			newLines = append(newLines, line)
		}
	}

	newContent := strings.Join(newLines, "\n")
	if !strings.HasSuffix(newContent, "\n") {
		newContent += "\n"
	}

	return WriteFileContent(hostsPath, newContent, 0644)
}

// ParseResourceID parses an Azure resource ID and returns its components
func ParseResourceID(resourceID string) (subscriptionID, resourceGroup, resourceName string, err error) {
	// Normalize: Azure CLI sometimes returns lowercase 'resourcegroups'
	resourceID = strings.Replace(resourceID, "/resourcegroups/", "/resourceGroups/", 1)

	parts := strings.Split(resourceID, "/")
	if len(parts) < 9 {
		return "", "", "", fmt.Errorf("invalid resource ID format: %s", resourceID)
	}

	// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/{provider}/{type}/{name}
	subscriptionID = parts[2]
	resourceGroup = parts[4]
	resourceName = parts[8]

	if subscriptionID == "" || resourceGroup == "" || resourceName == "" {
		return "", "", "", fmt.Errorf("failed to parse resource ID components: %s", resourceID)
	}

	return subscriptionID, resourceGroup, resourceName, nil
}

// FixSSHKeyOwnership fixes SSH key ownership when running with sudo
func FixSSHKeyOwnership(keyPath string) error {
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		return nil // Not running with sudo
	}

	u, err := user.Lookup(sudoUser)
	if err != nil {
		return fmt.Errorf("failed to lookup user %s: %w", sudoUser, err)
	}

	// Change ownership of both private and public keys
	for _, path := range []string{keyPath, keyPath + ".pub"} {
		if FileExists(path) {
			cmd := exec.Command("chown", fmt.Sprintf("%s:%s", u.Uid, u.Gid), path) // #nosec G204 -- chown with uid/gid
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("failed to change ownership of %s: %w", path, err)
			}
		}
	}

	return nil
}

// GetHostname returns the lowercase hostname
func GetHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return strings.ToLower(hostname), nil
}

// IsRoot checks if the current process is running as root
func IsRoot() bool {
	return os.Getuid() == 0
}

// CleanKubeCache removes kube cache directories
func CleanKubeCache() error {
	paths := []string{
		"/root/.kube/cache",
		filepath.Join(GetRealHome(), ".kube", "cache"),
	}

	for _, path := range paths {
		if FileExists(path) {
			if err := os.RemoveAll(path); err != nil {
				// Log but don't fail
				continue
			}
		}
	}

	return nil
}
