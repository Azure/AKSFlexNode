package v20260301

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertFileAndBackup(t *testing.T, path, wantOutput, originalInput string, wantBackup bool) {
	t.Helper()

	got, err := os.ReadFile(path) // #nosec - test helper
	if err != nil {
		t.Fatalf("failed to read file after call: %v", err)
	}
	if string(got) != wantOutput {
		t.Errorf("file content mismatch\ngot:\n%s\nwant:\n%s", string(got), wantOutput)
	}

	backupPath := path + ".bak"
	_, backupErr := os.Stat(backupPath)
	backupExists := backupErr == nil

	if wantBackup && !backupExists {
		t.Errorf("expected backup file %s to exist, but it does not", backupPath)
	}
	if !wantBackup && backupExists {
		t.Errorf("expected no backup file, but %s exists", backupPath)
	}
	if wantBackup && backupExists {
		backup, err := os.ReadFile(backupPath) // #nosec - test helper
		if err != nil {
			t.Fatalf("failed to read backup file: %v", err)
		}
		if string(backup) != originalInput {
			t.Errorf("backup content mismatch\ngot:\n%s\nwant:\n%s", string(backup), originalInput)
		}
	}
}

func TestCommentOutSwapInFstab(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantOutput string
		wantBackup bool
	}{
		{
			name: "comments out a single swap line",
			input: `/dev/sda1 / ext4 defaults 0 1
/dev/sda2 none swap sw 0 0
`,
			wantOutput: `/dev/sda1 / ext4 defaults 0 1
# /dev/sda2 none swap sw 0 0
`,
			wantBackup: true,
		},
		{
			name: "comments out multiple swap lines",
			input: `/dev/sda1 / ext4 defaults 0 1
/dev/sda2 none swap sw 0 0
/dev/sda3 none swap sw 0 0
`,
			wantOutput: `/dev/sda1 / ext4 defaults 0 1
# /dev/sda2 none swap sw 0 0
# /dev/sda3 none swap sw 0 0
`,
			wantBackup: true,
		},
		{
			name: "no swap lines leaves file unchanged",
			input: `/dev/sda1 / ext4 defaults 0 1
/dev/sda3 /home ext4 defaults 0 2
`,
			wantOutput: `/dev/sda1 / ext4 defaults 0 1
/dev/sda3 /home ext4 defaults 0 2
`,
			wantBackup: false,
		},
		{
			name:       "empty file leaves file unchanged",
			input:      "",
			wantOutput: "",
			wantBackup: false,
		},
		{
			name: "already commented swap line is left alone",
			input: `/dev/sda1 / ext4 defaults 0 1
# /dev/sda2 none swap sw 0 0
`,
			wantOutput: `/dev/sda1 / ext4 defaults 0 1
# /dev/sda2 none swap sw 0 0
`,
			wantBackup: false,
		},
		{
			name: "mix of commented and uncommented swap lines",
			input: `# /dev/sda2 none swap sw 0 0
/dev/sda3 none swap sw 0 0
`,
			wantOutput: `# /dev/sda2 none swap sw 0 0
# /dev/sda3 none swap sw 0 0
`,
			wantBackup: true,
		},
		{
			name: "preserves leading whitespace when commenting",
			input: `/dev/sda1 / ext4 defaults 0 1
  /dev/sda2 none swap sw 0 0
`,
			wantOutput: `/dev/sda1 / ext4 defaults 0 1
#   /dev/sda2 none swap sw 0 0
`,
			wantBackup: true,
		},
		{
			name: "preserves blank lines and comments",
			input: `# this is a comment
/dev/sda1 / ext4 defaults 0 1

/dev/sda2 none swap sw 0 0
# another comment
`,
			wantOutput: `# this is a comment
/dev/sda1 / ext4 defaults 0 1

# /dev/sda2 none swap sw 0 0
# another comment
`,
			wantBackup: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			fstab := filepath.Join(dir, "fstab")

			if err := os.WriteFile(fstab, []byte(tt.input), 0600); err != nil {
				t.Fatalf("failed to write test fstab: %v", err)
			}

			a := &configureBaseOSAction{}
			if err := a.commentOutSwapInFstab(fstab); err != nil {
				t.Fatalf("commentOutSwapInFstab() returned error: %v", err)
			}

			assertFileAndBackup(t, fstab, tt.wantOutput, tt.input, tt.wantBackup)
		})
	}
}

func TestCommentOutSwapInFstab_FileNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fstab := filepath.Join(dir, "nonexistent")

	a := &configureBaseOSAction{}
	if err := a.commentOutSwapInFstab(fstab); err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
}

func TestCommentOutSwapInFstab_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fstab := filepath.Join(dir, "fstab")

	input := `/dev/sda1 / ext4 defaults 0 1
/dev/sda2 none swap sw 0 0
`
	want := `/dev/sda1 / ext4 defaults 0 1
# /dev/sda2 none swap sw 0 0
`

	if err := os.WriteFile(fstab, []byte(input), 0600); err != nil {
		t.Fatalf("failed to write test fstab: %v", err)
	}

	a := &configureBaseOSAction{}

	// first call should comment out the swap line
	if err := a.commentOutSwapInFstab(fstab); err != nil {
		t.Fatalf("first call returned error: %v", err)
	}
	got, err := os.ReadFile(fstab) // #nosec - path has been validated by caller
	if err != nil {
		t.Fatalf("failed to read fstab: %v", err)
	}
	if string(got) != want {
		t.Errorf("after first call: got:\n%s\nwant:\n%s", string(got), want)
	}

	// second call should be a no-op (swap line is already commented)
	if err := a.commentOutSwapInFstab(fstab); err != nil {
		t.Fatalf("second call returned error: %v", err)
	}
	got2, err := os.ReadFile(fstab) // #nosec - path has been validated by caller
	if err != nil {
		t.Fatalf("failed to read fstab: %v", err)
	}
	if string(got2) != want {
		t.Errorf("after second call: got:\n%s\nwant:\n%s", string(got2), want)
	}
}

func TestSanitizeSysctlConf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantOutput string
		wantBackup bool
	}{
		{
			name: "comments out conflicting rp_filter lines",
			input: `# sysctl settings
net.ipv4.conf.default.rp_filter=1
net.ipv4.conf.all.rp_filter=1
`,
			wantOutput: `# sysctl settings
# net.ipv4.conf.default.rp_filter=1
# net.ipv4.conf.all.rp_filter=1
`,
			wantBackup: true,
		},
		{
			name: "comments out conflicting ip_forward",
			input: `net.ipv4.ip_forward = 0
net.ipv6.conf.all.disable_ipv6 = 1
`,
			wantOutput: `# net.ipv4.ip_forward = 0
net.ipv6.conf.all.disable_ipv6 = 1
`,
			wantBackup: true,
		},
		{
			name: "leaves non-conflicting settings unchanged",
			input: `net.ipv6.conf.all.disable_ipv6 = 1
fs.file-max = 65535
`,
			wantOutput: `net.ipv6.conf.all.disable_ipv6 = 1
fs.file-max = 65535
`,
			wantBackup: false,
		},
		{
			name: "already commented lines are left alone",
			input: `# net.ipv4.conf.all.rp_filter=1
# net.ipv4.ip_forward = 0
`,
			wantOutput: `# net.ipv4.conf.all.rp_filter=1
# net.ipv4.ip_forward = 0
`,
			wantBackup: false,
		},
		{
			name:       "empty file leaves file unchanged",
			input:      "",
			wantOutput: "",
			wantBackup: false,
		},
		{
			name: "mix of commented and uncommented conflicting lines",
			input: `# net.ipv4.conf.all.rp_filter=2
net.ipv4.conf.default.rp_filter=1
net.ipv6.conf.all.disable_ipv6 = 1
`,
			wantOutput: `# net.ipv4.conf.all.rp_filter=2
# net.ipv4.conf.default.rp_filter=1
net.ipv6.conf.all.disable_ipv6 = 1
`,
			wantBackup: true,
		},
		{
			name: "handles spaces around equals sign",
			input: `net.ipv4.ip_forward = 0
net.ipv4.conf.all.rp_filter = 1
`,
			wantOutput: `# net.ipv4.ip_forward = 0
# net.ipv4.conf.all.rp_filter = 1
`,
			wantBackup: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			sysctlConf := filepath.Join(dir, "sysctl.conf")

			if err := os.WriteFile(sysctlConf, []byte(tt.input), 0600); err != nil {
				t.Fatalf("failed to write test sysctl.conf: %v", err)
			}

			a := &configureBaseOSAction{}
			if err := a.sanitizeSysctlConf(sysctlConf); err != nil {
				t.Fatalf("sanitizeSysctlConf() returned error: %v", err)
			}

			assertFileAndBackup(t, sysctlConf, tt.wantOutput, tt.input, tt.wantBackup)
		})
	}
}

func TestSanitizeSysctlConf_FileNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent")

	a := &configureBaseOSAction{}
	if err := a.sanitizeSysctlConf(path); err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
}

func TestSanitizeSysctlConf_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sysctlConf := filepath.Join(dir, "sysctl.conf")

	input := `net.ipv4.conf.all.rp_filter=1
net.ipv4.ip_forward = 0
`
	want := `# net.ipv4.conf.all.rp_filter=1
# net.ipv4.ip_forward = 0
`

	if err := os.WriteFile(sysctlConf, []byte(input), 0600); err != nil {
		t.Fatalf("failed to write test sysctl.conf: %v", err)
	}

	a := &configureBaseOSAction{}

	if err := a.sanitizeSysctlConf(sysctlConf); err != nil {
		t.Fatalf("first call returned error: %v", err)
	}
	got, err := os.ReadFile(sysctlConf) // #nosec - path has been validated by caller
	if err != nil {
		t.Fatalf("failed to read sysctl.conf: %v", err)
	}
	if string(got) != want {
		t.Errorf("after first call: got:\n%s\nwant:\n%s", string(got), want)
	}

	if err := a.sanitizeSysctlConf(sysctlConf); err != nil {
		t.Fatalf("second call returned error: %v", err)
	}
	got2, err := os.ReadFile(sysctlConf) // #nosec - path has been validated by caller
	if err != nil {
		t.Fatalf("failed to read sysctl.conf: %v", err)
	}
	if string(got2) != want {
		t.Errorf("after second call: got:\n%s\nwant:\n%s", string(got2), want)
	}
}

func TestCommentOutMatchingLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		shouldComment func(string) bool
		wantOutput    string
		wantBackup    bool
	}{
		{
			name:  "comments out lines matching predicate",
			input: "keep this\nremove this\nkeep that\n",
			shouldComment: func(line string) bool {
				return strings.Contains(line, "remove")
			},
			wantOutput: "keep this\n# remove this\nkeep that\n",
			wantBackup: true,
		},
		{
			name:  "no matches leaves file unchanged",
			input: "keep this\nkeep that\n",
			shouldComment: func(line string) bool {
				return strings.Contains(line, "remove")
			},
			wantOutput: "keep this\nkeep that\n",
			wantBackup: false,
		},
		{
			name:  "skips already-commented lines",
			input: "# remove this\nremove this\n",
			shouldComment: func(line string) bool {
				return strings.Contains(line, "remove")
			},
			wantOutput: "# remove this\n# remove this\n",
			wantBackup: true,
		},
		{
			name:  "preserves leading whitespace when commenting",
			input: "  remove this\n",
			shouldComment: func(line string) bool {
				return strings.Contains(line, "remove")
			},
			wantOutput: "#   remove this\n",
			wantBackup: true,
		},
		{
			name:          "empty file is a no-op",
			input:         "",
			shouldComment: func(string) bool { return true },
			wantOutput:    "",
			wantBackup:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "testfile")

			if err := os.WriteFile(path, []byte(tt.input), 0600); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			if err := commentOutMatchingLines(path, tt.shouldComment); err != nil {
				t.Fatalf("commentOutMatchingLines() returned error: %v", err)
			}

			assertFileAndBackup(t, path, tt.wantOutput, tt.input, tt.wantBackup)
		})
	}
}

func TestCommentOutMatchingLines_FileNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent")

	if err := commentOutMatchingLines(path, func(string) bool { return true }); err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
}
