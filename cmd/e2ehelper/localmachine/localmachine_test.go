package localmachine

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandCreateStatusGetDelete(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "machine.json")

	out := runCommand(t, "--path", path, "create", "--kubernetes-version", "1.34.0", "--settings-version", "42")
	if !strings.Contains(out, `"settingsVersion": "42"`) {
		t.Fatalf("create output = %s", out)
	}

	out = runCommand(t, "--path", path, "status", "--provisioning-state", "Succeeded", "--observed-settings-version", "42", "--message", "ok")
	if !strings.Contains(out, `"provisioningState": "Succeeded"`) {
		t.Fatalf("status output = %s", out)
	}

	out = runCommand(t, "--path", path, "get")
	if !strings.Contains(out, `"message": "ok"`) {
		t.Fatalf("get output = %s", out)
	}

	out = runCommand(t, "--path", path, "delete")
	if strings.TrimSpace(out) != "deleted" {
		t.Fatalf("delete output = %s", out)
	}
}

func runCommand(t *testing.T, args ...string) string {
	t.Helper()

	cmd := Command
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%v): %v\n%s", args, err, buf.String())
	}
	return buf.String()
}
