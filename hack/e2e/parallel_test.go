package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParallelLanesPropagateFailures(t *testing.T) {
	t.Parallel()
	for _, failedLane := range []string{"", "lifecycle", "isolation"} {
		name := failedLane
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			script, err := filepath.Abs("run.sh")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			runShellResult(t, []string{"SCRIPT=" + script, "E2E_WORK_DIR=" + root, "FAIL=" + failedLane}, `
source "$SCRIPT"
mkdir -p "$E2E_LOG_DIR"
SKIP_BUILD=1
E2E_BINARY="$SCRIPT"
E2E_CONTROLLER_IMAGE=fixture
E2E_SKIP_LOG_COLLECTION=1
infra_deploy() { :; }
ensure_cluster_dependencies() { :; }
log_section() { :; }
lane() {
  touch "$E2E_WORK_DIR/$1-started"
  for _ in {1..100}; do
    if [[ -f "$E2E_WORK_DIR/lifecycle-started" && -f "$E2E_WORK_DIR/isolation-started" ]]; then
      touch "$E2E_WORK_DIR/$1-finished"
      [[ "$FAIL" != "$1" ]]
      return
    fi
    sleep 0.01
  done
  return 1
}
cmd_lifecycle_e2e() { lane lifecycle; }
cmd_isolation_lane() { lane isolation; }
cleanup() { touch "$E2E_WORK_DIR/cleanup"; }
cmd_all
`, failedLane == "")
			for _, file := range []string{"lifecycle-finished", "isolation-finished", "cleanup"} {
				if _, err := os.Stat(filepath.Join(root, file)); err != nil {
					t.Fatalf("both lanes must finish before cleanup: %s: %v", file, err)
				}
			}
		})
	}
}

func TestIsolationLaneOwnsStateAndKubeconfig(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("run.sh")
	if err != nil {
		t.Fatal(err)
	}
	runShell(t, []string{"SCRIPT=" + script, "E2E_WORK_DIR=" + t.TempDir()}, `
source "$SCRIPT"
printf '%s' '{"token_vm_name":"original","isolation_vm_name":"isolated","isolation_vm_ip":"1.2.3.4","isolation_vm_private_ip":"10.0.0.4"}' > "$E2E_STATE_FILE"
printf kubeconfig > "$E2E_WORK_DIR/kubeconfig"
export KUBECONFIG="$E2E_WORK_DIR/kubeconfig"
export E2E_CLUSTER_LOCK_DIR="$E2E_WORK_DIR/cluster.lock"
original_state="$E2E_STATE_FILE"
original_kubeconfig="$KUBECONFIG"
node_join_token() {
  [[ "$(state_get token_vm_name)" == isolated ]]
  [[ "$(state_get token_vm_ip)" == 1.2.3.4 ]]
  [[ "$(state_get token_vm_private_ip)" == 10.0.0.4 ]]
  [[ "$E2E_STATE_FILE" != "$original_state" && "$KUBECONFIG" != "$original_kubeconfig" ]]
  [[ "$E2E_CLUSTER_LOCK_DIR" == "${E2E_WORK_DIR%/isolation}/cluster.lock" ]]
  cmp "$original_kubeconfig" "$KUBECONFIG"
  state_set lane isolated
}
validate_node_joined() { [[ "$1" == isolated ]]; }
isolation_all() { [[ "$(state_get lane)" == isolated ]]; }
cmd_isolation_lane
[[ "$(state_get token_vm_name)" == original ]]
[[ -z "$(state_get lane)" ]]
[[ "$KUBECONFIG" == "$original_kubeconfig" ]]
`)
}

func TestParallelRepavesPropagateFailures(t *testing.T) {
	t.Parallel()
	for _, failedMode := range []string{"", "msi", "token", "kubeadm", "localdns"} {
		name := failedMode
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			script, err := filepath.Abs("lib/upgrade-drift.sh")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			runShellResult(t, []string{"SCRIPT=" + script, "E2E_WORK_DIR=" + root, "FAIL=" + failedMode}, `
source "$SCRIPT"
log_section() { :; }
upgrade_drift_mode() {
  touch "$E2E_WORK_DIR/$1-started"
  for _ in {1..100}; do
    if [[ -f "$E2E_WORK_DIR/msi-started" && -f "$E2E_WORK_DIR/token-started" && -f "$E2E_WORK_DIR/kubeadm-started" ]]; then
      touch "$E2E_WORK_DIR/$1-finished"
      [[ "$FAIL" != "$1" ]]
      return
    fi
    sleep 0.01
  done
  return 1
}
localdns_disable_repave_msi() {
  [[ -f "$E2E_WORK_DIR/msi-finished" ]]
  touch "$E2E_WORK_DIR/localdns"
  [[ "$FAIL" != localdns ]]
}
upgrade_drift_all
`, failedMode == "")
			for _, mode := range []string{"msi", "token", "kubeadm"} {
				if _, err := os.Stat(filepath.Join(root, mode+"-finished")); err != nil {
					t.Fatal(err)
				}
			}
			_, err = os.Stat(filepath.Join(root, "localdns"))
			if (err == nil) == (failedMode == "msi") {
				t.Fatalf("LocalDNS must run only after successful MSI repave: %v", err)
			}
		})
	}
}
