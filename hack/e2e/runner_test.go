package e2e

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runShell(t *testing.T, env []string, script string) string {
	t.Helper()
	return runShellResult(t, env, script, true)
}

func TestRunnerScriptsParse(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"run.sh", "lib/isolation.sh", "lib/node-join-token.sh"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if out, err := exec.CommandContext(ctx, "bash", "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("parse %s: %v\n%s", path, err, out)
			}
		})
	}
}

func TestTokenJoinSelectsWorkerIsolation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nspawn", "none", "container"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			script, err := filepath.Abs("lib/node-join-token.sh")
			if err != nil {
				t.Fatal(err)
			}
			runShell(t, []string{"SCRIPT=" + script, "E2E_WORK_DIR=" + t.TempDir(), "MODE=" + mode}, `
source "$SCRIPT"
log_section() { :; }
log_info() { :; }
log_success() { :; }
state_get() { case "$1" in token_vm_private_ip) echo 10.0.0.4;; *) echo fixture;; esac; }
ensure_flex_controller() { :; }
mark_e2e_bootstrap_token_aks_managed() { :; }
with_cluster_lock() {
  if [[ "$1" == mark_e2e_bootstrap_token_aks_managed ]]; then return 0; fi
  case "$2" in
    setup-node-rbac) return 0;;
    generate-node-config)
      while (( $# )); do
        if [[ "$1" == --output ]]; then
          printf '%s\n' '{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}},"agent":{},"node":{"kubelet":{}}}' > "$2"
          return 0
        fi
        shift
      done
      return 1;;
    *) return 1;;
  esac
}
machine_configmap_upsert() { :; }
_deploy_and_start_agent() {
  jq -e --arg mode "$MODE" '
    .bootstrap.isolation == $mode
    and .bootstrap.experimental.containerIsolation == ($mode == "container")
    and .agent.requireMachineRegistration == true
    and .components.kubernetes == "1.35.0"
    and .node.maxPods == 58' "$2" >/dev/null
  if [[ "$MODE" != nspawn ]]; then jq -e '.agent.machineReconcileInterval == "5s"' "$2" >/dev/null; fi
}
E2E_KUBELET_MAX_PODS=58
E2E_TARGET_AGENT_POOL_NAME=pool
E2E_CONTROLLER_SERVICE_PROXY_PATH=/fixture
E2E_KUBERNETES_VERSION=1.35.0
E2E_CONTAINERD_VERSION=2.0.4
E2E_RUNC_VERSION=1.1.12
E2E_KUBELET_SYSTEM_RESERVED_CPU=50m
E2E_KUBELET_SYSTEM_RESERVED_MEMORY=100Mi
E2E_KUBELET_KUBE_RESERVED_CPU=200m
E2E_KUBELET_KUBE_RESERVED_MEMORY=650Mi
node_join_token "" "$MODE"
`)
		})
	}
}

func TestIsolationLifecycleDispatch(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("lib/isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	output := runShell(t, []string{"SCRIPT=" + script, "E2E_WORK_DIR=" + t.TempDir()}, `
source "$SCRIPT"
state_get() { echo "$1"; }
kubectl() { echo node/token; }
log_section() { :; }
_agent_upgrade_ensure_api() { echo api; }
node_unjoin_token() { echo reset; }
validate_node_absent() { echo absent; }
node_join_token() { echo "join:${2:-nspawn}"; }
_isolation_wait_runtime() { echo "runtime:$2"; }
validate_node_joined() { echo ready; }
validate_node_ip() { echo address; }
smoke_test() { echo "workload:$2"; }
_isolation_reapply_and_restart() { echo "restart:$3:$4"; }
_isolation_host_reboot() { echo "reboot:$3"; }
_isolation_repave() { echo "repave:$3"; }
_isolation_assert_clean() { echo "clean:$2"; }
isolation_all
`)
	want := []string{"api", "reset", "absent"}
	for _, mode := range []string{"none", "container"} {
		want = append(want,
			"join:"+mode, "runtime:"+mode, "ready", "address", "workload:"+mode+"-join",
			"restart:"+mode+":pre-repave", "reboot:"+mode, "repave:"+mode, "restart:"+mode+":post-repave",
			"reset", "clean:"+mode, "absent", "join:"+mode, "runtime:"+mode, "ready",
			"workload:"+mode+"-rejoin", "reset", "clean:"+mode, "absent",
		)
	}
	want = append(want, "join:nspawn", "ready")
	if strings.TrimSpace(output) != strings.Join(want, "\n") {
		t.Fatalf("lifecycle dispatch differs:\n%s", output)
	}
}
