package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisabledNPDNspawnPreservesShellVariables(t *testing.T) {
	t.Parallel()

	for _, tool := range []string{"bash", "jq", "sh", "timeout"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required: %v", tool, err)
		}
	}
	source, err := os.ReadFile("lib/validate.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, function, ok := strings.Cut(string(source), "validate_npd_disabled() {")
	if !ok {
		t.Fatal("validate_npd_disabled function not found")
	}
	_, script, ok := strings.Cut(function, "<<'REMOTE'\n")
	if !ok {
		t.Fatal("remote validation script not found")
	}
	script, _, ok = strings.Cut(script, "\nREMOTE")
	if !ok {
		t.Fatal("remote validation script terminator not found")
	}

	for _, name := range []string{"absent", "unexpected binary"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o700); err != nil {
				t.Fatal(err)
			}
			for path, contents := range map[string]string{
				"config.json": `{}`,
				"state.json":  `{"isolation":"nspawn","activeMachine":"kube1"}`,
			} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mocks := map[string]string{
				"systemctl": "#!/bin/sh\nprintf 'not-found\\n'\n",
				"systemd-run": `#!/usr/bin/env bash
set -euo pipefail
while [[ "$1" == --* ]]; do shift; done
args=()
# systemd expands ExecStart arguments, but not stdin.
for argument in "$@"; do
  argument="${argument//\$\{load_state\}/}"
  argument="${argument//\$\{path\}/}"
  args+=("${argument}")
done
exec "${args[@]}"
`,
			}
			for path, contents := range mocks {
				if err := os.WriteFile(filepath.Join(bin, path), []byte(contents), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			binary := filepath.Join(root, "npd")
			if name == "unexpected binary" {
				if err := os.WriteFile(binary, []byte("unexpected NPD"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			replacer := strings.NewReplacer(
				"/etc/aks-flex-node/config.json", filepath.Join(root, "config.json"),
				"/etc/aks-flex-node/daemon-state.json", filepath.Join(root, "state.json"),
				"/etc/systemd/system/node-problem-detector.service", filepath.Join(root, "npd.service"),
				"/opt/aks-flex-node/bin/node-problem-detector", binary,
				"/etc/node-problem-detector/kernel-monitor.json", filepath.Join(root, "kernel-monitor.json"),
			)
			cmd := exec.CommandContext(t.Context(), "bash", "-s")
			cmd.Stdin = strings.NewReader(replacer.Replace(script) + "\n")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if name == "absent" {
				if err != nil {
					t.Fatalf("disabled NPD check failed: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), "Unexpected NPD artifact: "+binary) {
				t.Fatalf("unexpected binary was not rejected: err=%v output=%s", err, output)
			}
		})
	}
}
