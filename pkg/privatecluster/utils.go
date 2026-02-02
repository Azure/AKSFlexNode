package privatecluster

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// CommandExists checks if a command is available in PATH.
func CommandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// GetRealHome returns the real user's home directory (handles sudo).
func GetRealHome() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			return u.HomeDir
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return "/root"
}

// GetSSHKeyPath returns the default SSH key path for the Gateway.
func GetSSHKeyPath() string {
	return filepath.Join(GetRealHome(), ".ssh", "id_rsa_wg_gateway")
}

// EnsureDirectory creates a directory if it doesn't exist.
func EnsureDirectory(path string) error {
	return os.MkdirAll(path, 0750)
}

// ReadFileContent reads a file and returns its content.
func ReadFileContent(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is from trusted internal code
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteFileContent writes content to a file with specified permissions.
func WriteFileContent(path, content string, perm os.FileMode) error {
	return os.WriteFile(path, []byte(content), perm) // #nosec G304 G306 G703 -- path, perm are from trusted internal code
}

// AddHostsEntry adds an entry to /etc/hosts if it doesn't exist.
func AddHostsEntry(ip, hostname string) error {
	hostsPath := "/etc/hosts"

	content, err := ReadFileContent(hostsPath)
	if err != nil {
		return fmt.Errorf("failed to read hosts file: %w", err)
	}

	if strings.Contains(content, hostname) {
		return nil // Entry already exists
	}

	f, err := os.OpenFile(hostsPath, os.O_APPEND|os.O_WRONLY, 0600) // #nosec G304 -- hostsPath is validated
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

// RemoveHostsEntries removes entries matching a pattern from /etc/hosts.
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

	return WriteFileContent(hostsPath, newContent, 0644) // #nosec G306 -- /etc/hosts requires 0644 for system readability
}

// FixSSHKeyOwnership fixes SSH key ownership when running with sudo.
func FixSSHKeyOwnership(keyPath string) error {
	sudoUser := os.Getenv("SUDO_USER")
	if sudoUser == "" {
		return nil // Not running with sudo
	}

	u, err := user.Lookup(sudoUser)
	if err != nil {
		return fmt.Errorf("failed to lookup user %s: %w", sudoUser, err)
	}

	for _, path := range []string{keyPath, keyPath + ".pub"} {
		if utils.FileExists(path) {
			cmd := exec.Command("chown", fmt.Sprintf("%s:%s", u.Uid, u.Gid), path) // #nosec G204 G702 -- chown with uid/gid from user.Lookup
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("failed to change ownership of %s: %w", path, err)
			}
		}
	}

	return nil
}

// GetHostname returns the lowercase hostname.
func GetHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", err
	}
	return strings.ToLower(hostname), nil
}

// CleanKubeCache removes kube cache directories.
func CleanKubeCache() error {
	paths := []string{
		"/root/.kube/cache",
		filepath.Join(GetRealHome(), ".kube", "cache"),
	}

	for _, path := range paths {
		if utils.FileExists(path) {
			if err := os.RemoveAll(path); err != nil {
				// Log but don't fail
				continue
			}
		}
	}

	return nil
}
