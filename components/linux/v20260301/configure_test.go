package v20260301

import (
	"os"
	"path/filepath"
	"testing"
)

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

			if err := os.WriteFile(fstab, []byte(tt.input), 0644); err != nil {
				t.Fatalf("failed to write test fstab: %v", err)
			}

			a := &configureBaseOSAction{}
			if err := a.commentOutSwapInFstab(fstab); err != nil {
				t.Fatalf("commentOutSwapInFstab() returned error: %v", err)
			}

			got, err := os.ReadFile(fstab)
			if err != nil {
				t.Fatalf("failed to read fstab after call: %v", err)
			}
			if string(got) != tt.wantOutput {
				t.Errorf("fstab content mismatch\ngot:\n%s\nwant:\n%s", string(got), tt.wantOutput)
			}

			backupPath := fstab + ".bak"
			_, backupErr := os.Stat(backupPath)
			backupExists := backupErr == nil

			if tt.wantBackup && !backupExists {
				t.Errorf("expected backup file %s to exist, but it does not", backupPath)
			}
			if !tt.wantBackup && backupExists {
				t.Errorf("expected no backup file, but %s exists", backupPath)
			}
			if tt.wantBackup && backupExists {
				backup, err := os.ReadFile(backupPath) // #nosec - path has been validated by caller
				if err != nil {
					t.Fatalf("failed to read backup file: %v", err)
				}
				if string(backup) != tt.input {
					t.Errorf("backup content mismatch\ngot:\n%s\nwant:\n%s", string(backup), tt.input)
				}
			}
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
	got, err := os.ReadFile(fstab)
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
	got2, err := os.ReadFile(fstab)
	if err != nil {
		t.Fatalf("failed to read fstab: %v", err)
	}
	if string(got2) != want {
		t.Errorf("after second call: got:\n%s\nwant:\n%s", string(got2), want)
	}
}
