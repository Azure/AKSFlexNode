package utils

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFileExists verifies the FileExists utility function for checking file existence.
// Test: Creates a temporary file and checks existence before and after creation
// Expected: Returns false for non-existent files, true for existing files
func TestFileExists(t *testing.T) {
	// Create temp file
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "test.txt")

	// File doesn't exist yet
	if FileExists(tempFile) {
		t.Error("FileExists should return false for non-existent file")
	}

	// Create file
	if err := os.WriteFile(tempFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// File exists now
	if !FileExists(tempFile) {
		t.Error("FileExists should return true for existing file")
	}
}

// TestFileExistsAndValid verifies file existence validation (non-zero size check).
// Test: Creates files with and without content, checks both existence and validity
// Expected: Returns true only for files that exist and have content (size > 0)
func TestFileExistsAndValid(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name     string
		content  []byte
		expected bool
	}{
		{
			name:     "valid file with content",
			content:  []byte("test content"),
			expected: true,
		},
		{
			name:     "empty file",
			content:  []byte(""),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempFile := filepath.Join(tempDir, tt.name+".txt")
			if err := os.WriteFile(tempFile, tt.content, 0644); err != nil {
				t.Fatalf("Failed to create test file: %v", err)
			}

			result := FileExistsAndValid(tempFile)
			if result != tt.expected {
				t.Errorf("FileExistsAndValid() = %v, want %v", result, tt.expected)
			}
		})
	}

	// Test non-existent file
	if FileExistsAndValid("/non/existent/file") {
		t.Error("FileExistsAndValid should return false for non-existent file")
	}
}

// TestDirectoryExists verifies directory existence checking functionality.
// Test: Checks existence of directory, file (not directory), and non-existent path
// Expected: Returns true only for actual directories, false for files and non-existent paths
func TestDirectoryExists(t *testing.T) {
	tempDir := t.TempDir()

	// Directory exists
	if !DirectoryExists(tempDir) {
		t.Error("DirectoryExists should return true for existing directory")
	}

	// Create a file (not directory)
	tempFile := filepath.Join(tempDir, "file.txt")
	if err := os.WriteFile(tempFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// File is not a directory
	if DirectoryExists(tempFile) {
		t.Error("DirectoryExists should return false for file")
	}

	// Non-existent path
	if DirectoryExists(filepath.Join(tempDir, "nonexistent")) {
		t.Error("DirectoryExists should return false for non-existent path")
	}
}

// TestRequiresSudoAccess verifies the sudo requirement detection logic for various commands and paths.
// Test: Tests commands that always need sudo (systemctl, apt), conditionally need sudo (mkdir, cp on system paths), and never need sudo (echo)
// Expected: Correctly identifies sudo requirements based on command name and file path arguments
func TestRequiresSudoAccess(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		args     []string
		expected bool
	}{
		{
			name:     "systemctl always needs sudo",
			command:  "systemctl",
			args:     []string{"start", "service"},
			expected: true,
		},
		{
			name:     "apt always needs sudo",
			command:  "apt",
			args:     []string{"install", "package"},
			expected: true,
		},
		{
			name:     "mkdir on system path needs sudo",
			command:  "mkdir",
			args:     []string{"/etc/test"},
			expected: true,
		},
		{
			name:     "mkdir on user path doesn't need sudo",
			command:  "mkdir",
			args:     []string{"/home/user/test"},
			expected: false,
		},
		{
			name:     "cp to /usr needs sudo",
			command:  "cp",
			args:     []string{"file.txt", "/usr/bin/file"},
			expected: true,
		},
		{
			name:     "cp in home doesn't need sudo",
			command:  "cp",
			args:     []string{"file1.txt", "/home/user/file2.txt"},
			expected: false,
		},
		{
			name:     "echo never needs sudo",
			command:  "echo",
			args:     []string{"hello"},
			expected: false,
		},
		{
			name:     "rm in /var needs sudo",
			command:  "rm",
			args:     []string{"-rf", "/var/lib/test"},
			expected: true,
		},
		{
			name:     "azcmagent always needs sudo",
			command:  "azcmagent",
			args:     []string{"connect"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := requiresSudoAccess(tt.command, tt.args)
			if result != tt.expected {
				t.Errorf("requiresSudoAccess(%s, %v) = %v, want %v", tt.command, tt.args, result, tt.expected)
			}
		})
	}
}

// TestShouldIgnoreCleanupError verifies error filtering for cleanup operations.
// Test: Tests various error types to determine which should be ignored during cleanup
// Expected: Errors matching cleanup patterns (not loaded, does not exist, No such file) should be ignored
func TestShouldIgnoreCleanupError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error should not be ignored",
			err:      nil,
			expected: false,
		},
		{
			name:     "os.ErrNotExist matches 'does not exist' pattern",
			err:      os.ErrNotExist,
			expected: true, // "file does not exist" matches "does not exist" pattern
		},
		{
			name:     "PathError with ErrNotExist should be ignored",
			err:      &os.PathError{Op: "remove", Path: "/test", Err: os.ErrNotExist},
			expected: true, // Error message contains "no such file or directory"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldIgnoreCleanupError(tt.err)
			if result != tt.expected {
				t.Errorf("shouldIgnoreCleanupError(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

// TestCreateTempFile verifies temporary file creation with specific content.
// Test: Creates a temp file with pattern and content, then verifies file exists and content matches
// Expected: File should be created with correct content and be readable
func TestCreateTempFile(t *testing.T) {
	content := []byte("test content")
	pattern := "test-*.txt"

	file, err := CreateTempFile(pattern, content)
	if err != nil {
		t.Fatalf("CreateTempFile failed: %v", err)
	}
	defer func() {
		_ = file.Close()
		CleanupTempFile(file.Name())
	}()

	// Verify file exists
	if !FileExists(file.Name()) {
		t.Error("Temp file should exist")
	}

	// Verify content
	readContent, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("Failed to read temp file: %v", err)
	}

	if string(readContent) != string(content) {
		t.Errorf("Content mismatch: got %q, want %q", readContent, content)
	}
}

// TestWriteFileAtomic verifies atomic file writing (write to temp, then rename).
// Test: Writes content atomically and verifies file existence, content, and permissions
// Expected: File should be created with correct content and permissions without partial writes
func TestWriteFileAtomic(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "test.txt")
	content := []byte("test content")
	perm := os.FileMode(0644)

	err := WriteFileAtomic(testFile, content, perm)
	if err != nil {
		t.Fatalf("WriteFileAtomic failed: %v", err)
	}

	// Verify file exists
	if !FileExists(testFile) {
		t.Error("File should exist after WriteFileAtomic")
	}

	// Verify content
	readContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	if string(readContent) != string(content) {
		t.Errorf("Content mismatch: got %q, want %q", readContent, content)
	}

	// Verify permissions
	stat, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("Failed to stat file: %v", err)
	}

	if stat.Mode().Perm() != perm {
		t.Errorf("Permission mismatch: got %v, want %v", stat.Mode().Perm(), perm)
	}
}

// TestGetArc verifies system architecture detection and normalization.
// Test: Calls GetArc to detect system architecture
// Expected: Returns one of the valid architectures (amd64, arm64, arm) based on system
func TestGetArc(t *testing.T) {
	arch, err := GetArc()
	if err != nil {
		t.Fatalf("GetArc failed: %v", err)
	}

	// Verify it returns a valid architecture string
	validArchs := []string{"amd64", "arm64", "arm"}
	valid := false
	for _, validArch := range validArchs {
		if arch == validArch {
			valid = true
			break
		}
	}

	if !valid {
		t.Errorf("GetArc returned unexpected architecture: %s", arch)
	}
}

// TestExtractClusterInfo verifies kubeconfig parsing for server URL and CA certificate extraction.
// Test: Tests valid kubeconfig, empty config, missing clusters, and missing server
// Expected: Successfully extracts server URL and CA data from valid kubeconfig, errors on invalid inputs
func TestExtractClusterInfo(t *testing.T) {
	tests := []struct {
		name       string
		kubeconfig string
		wantErr    bool
		wantServer string
	}{
		{
			name: "valid kubeconfig",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: dGVzdAo=
    server: https://test.example.com:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: test-token
`,
			wantErr:    false,
			wantServer: "https://test.example.com:6443",
		},
		{
			name:       "empty kubeconfig",
			kubeconfig: ``,
			wantErr:    true,
		},
		{
			name: "kubeconfig without clusters",
			kubeconfig: `apiVersion: v1
kind: Config
clusters: []
`,
			wantErr: true,
		},
		{
			name: "kubeconfig without server",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: dGVzdAo=
  name: test-cluster
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, caData, err := ExtractClusterInfo([]byte(tt.kubeconfig))

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if server != tt.wantServer {
				t.Errorf("Server mismatch: got %q, want %q", server, tt.wantServer)
			}

			if caData == "" {
				t.Error("CA data should not be empty")
			}
		})
	}
}

// TestCleanupTempFile verifies temporary file cleanup without panicking.
// Test: Creates a file, cleans it up, verifies it's removed, then tests cleanup of non-existent file
// Expected: File should be removed successfully, cleanup of non-existent file should not panic
func TestCleanupTempFile(t *testing.T) {
	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "test.txt")

	// Create a file
	if err := os.WriteFile(tempFile, []byte("test"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Cleanup should not panic
	CleanupTempFile(tempFile)

	// File should be removed
	if FileExists(tempFile) {
		t.Error("File should be removed after CleanupTempFile")
	}

	// Cleanup non-existent file should not panic
	CleanupTempFile("/non/existent/file")
}
