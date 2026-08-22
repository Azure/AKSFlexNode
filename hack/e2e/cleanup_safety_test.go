package e2e_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed lib/infra.sh
var infraScript string

func TestCleanupTargetNameValidation(t *testing.T) {
	t.Parallel()

	valid := []string{
		"test",
		"e2e-test",
		"aks-e2e-test",
		"MC_aksflex-e2e-test",
		"vm-e2e-msi-test",
		"vm-e2e-token-test",
		"vm-e2e-offline-test",
		"vm-e2e-kubeadm-test",
		"vm-e2e-arc-test",
		"vm-e2e-arc-test-connected",
		"vnet-e2e-test",
		"nsg-e2e-test",
	}

	tests := []struct {
		name        string
		argument    int
		value       string
		wantSuccess bool
		wantError   string
	}{
		{name: "matching targets", argument: -1, wantSuccess: true},
		{name: "missing optional target", argument: 4, value: "", wantSuccess: true},
		{name: "missing suffix", argument: 0, value: "", wantError: "without an E2E name suffix"},
		{name: "deployment", argument: 1, value: "production-deployment", wantError: "unexpected ARM deployment"},
		{name: "cluster", argument: 2, value: "production-cluster", wantError: "unexpected AKS cluster"},
		{name: "node resource group", argument: 3, value: "production-node-rg", wantError: "unexpected AKS node resource group"},
		{name: "MSI VM", argument: 4, value: "production-msi", wantError: "unexpected MSI VM"},
		{name: "token VM", argument: 5, value: "production-token", wantError: "unexpected token VM"},
		{name: "offline VM", argument: 6, value: "production-offline", wantError: "unexpected offline VM"},
		{name: "kubeadm VM", argument: 7, value: "production-kubeadm", wantError: "unexpected kubeadm VM"},
		{name: "Arc VM", argument: 8, value: "production-arc", wantError: "unexpected Arc VM"},
		{name: "Arc machine", argument: 9, value: "production-machine", wantError: "unexpected Arc machine"},
		{name: "virtual network", argument: 10, value: "production-vnet", wantError: "unexpected virtual network"},
		{name: "network security group", argument: 11, value: "production-nsg", wantError: "unexpected network security group"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			args := append([]string(nil), valid...)
			if test.argument >= 0 {
				args[test.argument] = test.value
			}
			cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
shift 2
if _validate_cleanup_target_names "$@"; then
  printf 'RESULT=success\n'
else
  printf 'RESULT=error\n'
fi
`
			output, err := runBash(t, script, append([]string{t.TempDir(), cleanupScript}, args...)...)
			if err != nil {
				t.Fatalf("target validation harness failed: %v\n%s", err, output)
			}
			if test.wantSuccess {
				if !strings.Contains(string(output), "RESULT=success") {
					t.Fatalf("matching cleanup targets were rejected:\n%s", output)
				}
				return
			}
			if !strings.Contains(string(output), "RESULT=error") ||
				!strings.Contains(string(output), test.wantError) {
				t.Fatalf("unsafe cleanup target was not rejected with %q:\n%s", test.wantError, output)
			}
		})
	}
}

func TestCleanupResourceOwnerCompatibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		state       string
		githubRunID string
		want        string
		wantError   string
	}{
		{
			name:        "new state uses attempt-scoped owner",
			state:       `{"resource_owner":"12345-2","run_id":"12345","name_suffix":"12345-2"}`,
			githubRunID: "12345",
			want:        "12345-2",
		},
		{
			name:        "legacy state falls back to run ID",
			state:       `{"run_id":"12345"}`,
			githubRunID: "different-run",
			want:        "12345",
		},
		{
			name:        "partial legacy state falls back to environment",
			state:       `{}`,
			githubRunID: "12345",
			want:        "12345",
		},
		{
			name:        "new state rejects another attempts owner",
			state:       `{"resource_owner":"12345-1","run_id":"12345","name_suffix":"12345-2"}`,
			githubRunID: "12345",
			wantError:   "does not match E2E name suffix",
		},
		{
			name:        "new partial state rejects unverifiable owner",
			state:       `{"resource_owner":"12345-2","run_id":"12345"}`,
			githubRunID: "12345",
			wantError:   "does not match E2E name suffix",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(workDir, "state.json"), []byte(test.state), 0o600); err != nil {
				t.Fatalf("write state: %v", err)
			}
			cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
GITHUB_RUN_ID="$2"
source "$3"
if owner="$(_cleanup_resource_owner)"; then
  printf 'OWNER=%s\n' "${owner}"
else
  printf 'OWNER=error\n'
fi
`
			output, err := runBash(t, script, workDir, test.githubRunID, cleanupScript)
			if err != nil {
				t.Fatalf("resolve cleanup owner: %v\n%s", err, output)
			}
			if test.wantError != "" {
				if !strings.Contains(string(output), "OWNER=error\n") ||
					!strings.Contains(string(output), test.wantError) {
					t.Fatalf("unsafe cleanup owner was not rejected with %q: %q", test.wantError, output)
				}
				return
			}
			if !strings.Contains(string(output), "OWNER="+test.want+"\n") {
				t.Fatalf("cleanup owner = %q, want %q", output, test.want)
			}
		})
	}
}

func TestInfrastructureUsesAttemptScopedOwnershipTag(t *testing.T) {
	t.Parallel()

	for _, required := range []string{
		`local resource_owner="${E2E_NAME_SUFFIX}"`,
		`--arg resource_owner "${resource_owner}"`,
		`run_id: $run_id, resource_owner: $resource_owner`,
		`--arg run "${resource_owner}"`,
		`'{"github-run": $run, "purpose": $purpose}'`,
	} {
		if !strings.Contains(infraScript, required) {
			t.Errorf("infrastructure ownership metadata is missing %q", required)
		}
	}
}

func TestLegacyRunTagCleanupIsScopedToNameSuffix(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	inventoryPath := filepath.Join(workDir, "inventory.json")
	inventory := `[
  {"type":"Microsoft.Compute/disks","name":"vm-e2e-token-12345-1_OsDisk_1_abcd","id":"/own-disk"},
  {"type":"Microsoft.Network/networkInterfaces","name":"vm-e2e-msi-12345-1-nic","id":"/own-nic"},
  {"type":"Microsoft.Compute/disks","name":"vm-e2e-token-12345-2_OsDisk_1_efgh","id":"/other-attempt-disk"},
  {"type":"Microsoft.Network/virtualNetworks","name":"vnet-e2e-12345-2","id":"/other-attempt-vnet"},
  {"type":"Microsoft.Compute/disks","name":"production-disk","id":"/unrelated"}
]`
	if err := os.WriteFile(inventoryPath, []byte(inventory), 0o600); err != nil {
		t.Fatalf("write resource inventory: %v", err)
	}

	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
INVENTORY_FILE="$2"
source "$3"
_resource_inventory() { cat "${INVENTORY_FILE}"; }
az() { printf 'AZ=%s\n' "$*"; }
printf '%s\n' 'IDS-BEGIN'
_tagged_resource_ids test-rg 12345 test-subscription 12345-1
printf '%s\n' 'IDS-END'
_delete_tagged_resources test-rg 12345 test-subscription 12345-1
`
	output, err := runBash(t, script, workDir, inventoryPath, cleanupScript)
	if err != nil {
		t.Fatalf("legacy tagged cleanup failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, ownID := range []string{"/own-disk", "/own-nic"} {
		if !strings.Contains(text, ownID) {
			t.Errorf("legacy tagged cleanup omitted owned resource %q:\n%s", ownID, text)
		}
	}
	for _, foreignID := range []string{"/other-attempt-disk", "/other-attempt-vnet", "/unrelated"} {
		if strings.Contains(text, foreignID) {
			t.Errorf("legacy tagged cleanup selected foreign resource %q:\n%s", foreignID, text)
		}
	}
}

func TestCleanupValidatesNamesBeforeAzureMutation(t *testing.T) {
	t.Parallel()

	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	contents, err := os.ReadFile(cleanupScript)
	if err != nil {
		t.Fatalf("read cleanup script: %v", err)
	}
	body := string(contents)
	cleanupStart := strings.Index(body, "cleanup() {")
	if cleanupStart < 0 {
		t.Fatal("cleanup function is absent")
	}
	body = body[cleanupStart:]
	validation := strings.Index(body, "if ! _validate_cleanup_target_names")
	if validation < 0 {
		t.Fatal("cleanup does not validate deterministic target names")
	}
	for _, mutation := range []string{
		`_delete_node_resource_group "${node_resource_group}"`,
		`az rest --method delete`,
		`az vm delete`,
		`az aks delete`,
		`az network vnet delete`,
		`_delete_tagged_resources`,
	} {
		mutationIndex := strings.Index(body, mutation)
		if mutationIndex < 0 {
			t.Fatalf("cleanup mutation %q is absent", mutation)
		}
		if validation >= mutationIndex {
			t.Errorf("cleanup validates names after mutation %q", mutation)
		}
	}
}
