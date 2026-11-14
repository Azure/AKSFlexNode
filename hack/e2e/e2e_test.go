package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestHarness(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		binary string
		script string
	}{
		{name: "runtime probes", binary: "python3", script: "runtime_test.py"},
		{name: "isolation lifecycle", binary: "bash", script: "isolation_modes_test.sh"},
		{name: "token isolation config", binary: "bash", script: "node_join_token_test.sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			binary, err := exec.LookPath(tt.binary)
			if err != nil {
				t.Skipf("E2E harness requires %s: %v", tt.binary, err)
			}
			script, err := filepath.Abs(tt.script)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, script)
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "E2E_WORK_DIR="+t.TempDir())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", tt.script, err, out)
			}
		})
	}
}

func TestShellSyntax(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("E2E scripts require bash: %v", err)
	}
	paths, err := filepath.Glob("lib/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, "run.sh", "isolation_modes_test.sh", "node_join_token_test.sh")
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, bash, "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("bash -n %s: %v\n%s", path, err, out)
			}
		})
	}
}
