package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
)

func TestRestoreAgentBinary(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"direct", "blue", "green", "interrupted cleanup"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := setupResetBinary(t, name)
			for range 2 {
				if err := restoreAgentBinary(paths); err != nil {
					t.Fatalf("restoreAgentBinary: %v", err)
				}
				info, err := os.Lstat(paths.BinaryPath)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("expected regular executable: info=%v, err=%v", info, err)
				}
				output, err := exec.CommandContext(t.Context(), paths.BinaryPath).Output()
				if err != nil || string(output) != "active" {
					t.Fatalf("restored command: output=%q, err=%v", output, err)
				}
				if _, err := os.Lstat(filepath.Dir(paths.BluePath)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("managed layout remains: %v", err)
				}
			}
		})
	}
}

func TestRestoreAgentBinaryRejectsUnsafeLayouts(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"unexpected link", "dangling link", "external slot", "non-executable slot", "symlink directory"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := setupResetBinary(t, "blue")
			external := filepath.Join(t.TempDir(), "external")
			writeResetExecutable(t, external, "external")
			switch name {
			case "unexpected link":
				if err := replaceSymlink(paths.BinaryPath, external); err != nil {
					t.Fatal(err)
				}
			case "dangling link":
				if err := os.Remove(paths.BluePath); err != nil {
					t.Fatal(err)
				}
			case "external slot":
				if err := replaceSymlink(paths.BluePath, external); err != nil {
					t.Fatal(err)
				}
			case "non-executable slot":
				if err := os.Chmod(paths.BluePath, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink directory":
				dir := filepath.Dir(paths.BluePath)
				if err := os.Rename(dir, dir+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir+"-original", dir); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Readlink(paths.BinaryPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := restoreAgentBinary(paths); err == nil {
				t.Fatal("restoration accepted unsafe layout")
			}
			after, err := os.Readlink(paths.BinaryPath)
			if err != nil || after != before {
				t.Fatalf("public link changed: %q, %v", after, err)
			}
			for _, path := range []string{paths.CurrentPath, paths.LastGoodPath, paths.GreenPath, external} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("restoration removed %s: %v", path, err)
				}
			}
		})
	}
}

func TestRestoreAgentBinaryCleanupRetry(t *testing.T) {
	t.Parallel()

	paths := setupResetBinary(t, "green")
	unexpected := filepath.Join(filepath.Dir(paths.BluePath), "unexpected")
	writeResetExecutable(t, unexpected, "preserved")
	if err := restoreAgentBinary(paths); err == nil {
		t.Fatal("restoration accepted unexpected layout contents")
	}
	output, err := exec.CommandContext(t.Context(), paths.BinaryPath).Output()
	if err != nil || string(output) != "active" {
		t.Fatalf("cleanup failure left command unavailable: %q, %v", output, err)
	}
	if _, err := os.Stat(unexpected); err != nil {
		t.Fatalf("unexpected file was removed: %v", err)
	}
	if err := os.Remove(unexpected); err != nil {
		t.Fatal(err)
	}
	if err := restoreAgentBinary(paths); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
}

func TestRestoreAgentBinaryTask(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"stopped", "active", "recovery active", "unknown state", "activation locked"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := setupResetBinary(t, "blue")
			lockPath := filepath.Join(t.TempDir(), "activation.lock")
			if name == "activation locked" {
				lock, err := agentbinary.AcquireHostActivationLock(lockPath)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close() //nolint:errcheck // test cleanup
			}
			task := &restoreAgentBinaryTask{
				log:      slog.Default(),
				paths:    paths,
				lockPath: lockPath,
				inspectService: func(_ context.Context, _ *slog.Logger, unit string) (bool, error) {
					if name == "unknown state" {
						return false, errors.New("systemd unavailable")
					}
					return name == "active" || name == "recovery active" && unit == recoveryServiceUnitName, nil
				},
			}
			err := task.Do(t.Context())
			if name == "stopped" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("restoration ignored service or lock guard")
				}
				assertResolvedPath(t, paths.BinaryPath, paths.BluePath)
				if _, err := os.Stat(paths.GreenPath); err != nil {
					t.Fatalf("inactive slot removed: %v", err)
				}
			}
		})
	}
}

func TestInstallAfterRestoreAgentBinary(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("installer sets root ownership")
	}
	paths := setupResetBinary(t, "green")
	if err := restoreAgentBinary(paths); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement")
	writeResetExecutable(t, replacement, "replacement")
	script, err := filepath.Abs("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `
source "$1"
INSTALL_DIR="$2"
MANAGED_BINARY_DIR="$3"
AGENT_UPGRADE_LOCK_PATH="$4"
install_binary "$5"
`, "reset-install-test", script, filepath.Dir(paths.BinaryPath), filepath.Dir(paths.BluePath),
		filepath.Join(t.TempDir(), "activation.lock"), replacement)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install after reset: %v\n%s", err, output)
	}
	output, err := exec.CommandContext(t.Context(), paths.BinaryPath).Output()
	if err != nil || string(output) != "replacement" {
		t.Fatalf("reinstalled command: %q, %v", output, err)
	}
}

func setupResetBinary(t *testing.T, layout string) agentUpgradePaths {
	t.Helper()
	paths := testAgentUpgradePaths(t)
	writeResetExecutable(t, paths.BinaryPath, "active")
	if layout == "direct" {
		return paths
	}
	writeResetExecutable(t, paths.BluePath, "inactive")
	writeResetExecutable(t, paths.GreenPath, "inactive")
	active := paths.BluePath
	if layout == "green" {
		active = paths.GreenPath
	}
	writeResetExecutable(t, active, "active")
	for link, target := range map[string]string{
		paths.CurrentPath: active, paths.LastGoodPath: paths.BluePath,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if layout != "interrupted cleanup" {
		if err := replaceSymlink(paths.BinaryPath, paths.CurrentPath); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func writeResetExecutable(t *testing.T, path, output string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf "+output), 0o755); err != nil {
		t.Fatal(err)
	}
}
