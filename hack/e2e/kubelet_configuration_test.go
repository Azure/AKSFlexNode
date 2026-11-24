package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateKubeletExtraConfiguration(t *testing.T) {
	t.Parallel()
	for _, binary := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is required: %v", binary, err)
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	applied := "serializeImagePulls=false\nmaxParallelImagePulls=4\nlogging.verbosity=2\nlogging.format=text"
	effective := `{"kubeletconfig":{"serializeImagePulls":false,"maxParallelImagePulls":4,"logging":{"verbosity":2,"format":"text"}}}`
	tests := []struct {
		name      string
		applied   string
		effective string
		wantErr   string
	}{
		{name: "rendered and effective match", applied: applied, effective: effective},
		{name: "rendered boolean differs", applied: strings.ReplaceAll(applied, "serializeImagePulls=false", "serializeImagePulls=true"), effective: effective, wantErr: "Applied kubelet serializeImagePulls"},
		{name: "effective boolean differs", applied: applied, effective: strings.ReplaceAll(effective, `"serializeImagePulls":false`, `"serializeImagePulls":true`), wantErr: "Effective kubelet serializeImagePulls"},
		{name: "effective concurrency missing", applied: applied, effective: strings.ReplaceAll(effective, `"maxParallelImagePulls":4,`, ""), wantErr: "Effective kubelet maxParallelImagePulls"},
		{name: "typed verbosity differs", applied: applied, effective: strings.ReplaceAll(effective, `"verbosity":2`, `"verbosity":9`), wantErr: "Effective kubelet logging.verbosity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			command := exec.Command("bash", "-c", `
set -euo pipefail
source "$1"
_read_applied_kubelet_config() { printf '%s\n' "$TEST_APPLIED"; }
kubectl() { printf '%s\n' "$TEST_EFFECTIVE"; }
validate_kubelet_extra_configuration test-node test-ip \
  serializeImagePulls=false maxParallelImagePulls=4 logging.verbosity=2 logging.format=text
`, "_", filepath.Join(dir, "lib", "validate.sh"))
			command.Env = append(os.Environ(), "E2E_WORK_DIR="+t.TempDir(), "TEST_APPLIED="+tt.applied, "TEST_EFFECTIVE="+tt.effective)
			output, err := command.CombinedOutput()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), tt.wantErr) {
				t.Fatalf("validate error = %v, output = %s, want %q", err, output, tt.wantErr)
			}
		})
	}
}
