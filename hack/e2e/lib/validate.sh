#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/validate.sh - Node join verification and smoke tests
#
# Functions:
#   validate_node_joined  <vm_name>  - Wait for a specific node to appear in kubectl
#   validate_all_nodes                - Verify MSI, token, offline, and kubeadm nodes joined
#   validate_npd_status   <vm_name> <vm_ip> - Verify node-problem-detector is active
#   validate_node_absent  <vm_name>  - Wait for a node to disappear from kubectl
#   validate_all_nodes_absent         - Verify all flex nodes are gone after unjoin
#   smoke_test            <vm_name> <label>  - Schedule an nginx pod on a node
#   smoke_test_all                    - Run smoke tests on all nodes
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_VALIDATE_LOADED:-}" ]] && return 0
readonly _E2E_VALIDATE_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# validate_node_joined - Wait for a node to appear in the cluster
# ---------------------------------------------------------------------------
validate_node_joined() {
  local vm_name="$1"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0

  log_info "Waiting for node '${vm_name}' to join cluster (timeout: ${timeout}s)..."

  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    if kubectl get nodes 2>/dev/null | grep -q "${vm_name}"; then
      log_success "Node '${vm_name}' joined the cluster"
      kubectl get node "${vm_name}" -o wide
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    log_debug "Waiting for ${vm_name}... (${elapsed}/${timeout}s)"
  done

  log_error "Node '${vm_name}' did not join cluster within ${timeout}s"
  log_error "Current nodes:"
  kubectl get nodes 2>&1 || true
  echo ""
  log_error "Certificate Signing Requests:"
  kubectl get csr 2>&1 || true
  return 1
}

# ---------------------------------------------------------------------------
# validate_node_ip - Ensure node InternalIP matches the expected provisioned IP
# ---------------------------------------------------------------------------
validate_node_ip() {
  local vm_name="$1"
  local expected_ip="$2"

  if [[ -z "${expected_ip}" ]] || ! is_valid_ipv4 "${expected_ip}"; then
    log_error "Expected node IP is empty or invalid for '${vm_name}': '${expected_ip}'"
    return 1
  fi

  local internal_ips
  internal_ips="$(kubectl get node "${vm_name}" -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}')"
  if grep -Fxq "${expected_ip}" <<<"${internal_ips}"; then
    log_success "Node '${vm_name}' InternalIP includes expected IP '${expected_ip}'"
    return 0
  fi

  log_error "Node '${vm_name}' InternalIP mismatch; expected '${expected_ip}'"
  log_error "Observed InternalIP values:"
  echo "${internal_ips}"
  return 1
}

# ---------------------------------------------------------------------------
# validate_npd_status - Ensure node-problem-detector is active and reporting
# ---------------------------------------------------------------------------
validate_npd_status() {
  local vm_name="$1"
  local vm_ip="$2"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0
  local npd_condition_jsonpath='{.status.conditions[?(@.type=="KernelDeadlock")].status}'
  local condition_error="${E2E_WORK_DIR}/npd-condition-${vm_name}.err"
  local quoted_timeout

  log_info "Validating node-problem-detector on '${vm_name}'..."

  if ! [[ "${timeout}" =~ ^[0-9]+$ ]]; then
    log_error "E2E_NODE_JOIN_TIMEOUT must be numeric, got '${timeout}'"
    return 1
  fi
  printf -v quoted_timeout "%q" "${timeout}"

  remote_exec "${vm_ip}" "E2E_NODE_JOIN_TIMEOUT=${quoted_timeout} bash -s" <<'REMOTE'
set -euo pipefail

deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
active_machine_error="/tmp/aks-flex-node-e2e-active-machine-$$.err"
status_error="/tmp/aks-flex-node-e2e-npd-status-$$.err"
while true; do
  if [[ ! -f /etc/aks-flex-node/daemon-state.json ]]; then
    active_machine=""
    echo "/etc/aks-flex-node/daemon-state.json is missing" > "${active_machine_error}"
  else
    active_machine="$(sudo python3 - <<'PY' 2>"${active_machine_error}" || true
import json
with open("/etc/aks-flex-node/daemon-state.json", encoding="utf-8") as state:
    print(json.load(state).get("activeMachine", ""))
PY
)"
  fi
  if [[ -n "${active_machine}" ]] && sudo machinectl show "${active_machine}" &>/dev/null; then
    status="$(sudo systemd-run --machine="${active_machine}" --quiet --pipe systemctl is-active node-problem-detector.service 2>"${status_error}" || true)"
    if [[ "${status}" == "active" ]]; then
      echo "node-problem-detector.service is active in ${active_machine}"
      exit 0
    fi
  fi

  if (( SECONDS >= deadline )); then
    echo "node-problem-detector.service did not become active"
    if [[ -s "${active_machine_error}" ]]; then
      cat "${active_machine_error}"
    fi
    if [[ -s "${status_error}" ]]; then
      cat "${status_error}"
    fi
    sudo machinectl list --no-pager || true
    if [[ -n "${active_machine:-}" ]]; then
      sudo systemd-run --machine="${active_machine}" --quiet --pipe systemctl status node-problem-detector.service --no-pager -l || true
      sudo systemd-run --machine="${active_machine}" --quiet --pipe journalctl -u node-problem-detector.service -n 50 --no-pager || true
    fi
    exit 1
  fi

  sleep 5
done
REMOTE

  local kernel_deadlock
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    kernel_deadlock="$(kubectl get node "${vm_name}" -o jsonpath="${npd_condition_jsonpath}" 2>"${condition_error}" || true)"
    if [[ "${kernel_deadlock}" == "False" ]]; then
      log_success "node-problem-detector is active and reporting on '${vm_name}'"
      return 0
    fi

    sleep 10
    elapsed=$((elapsed + 10))
    log_debug "Waiting for node-problem-detector condition on ${vm_name}... (${elapsed}/${timeout}s)"
  done

  log_error "node-problem-detector did not report KernelDeadlock=False on '${vm_name}' within ${timeout}s"
  if [[ -s "${condition_error}" ]]; then
    cat "${condition_error}" >&2
  fi
  kubectl describe node "${vm_name}" 2>&1 || true
  return 1
}

# ---------------------------------------------------------------------------
# validate_all_nodes - Check all MSI, token, offline, and kubeadm VMs joined
# ---------------------------------------------------------------------------
validate_all_nodes() {
  log_section "Validating Node Join"

  # Re-fetch kubeconfig to be safe
  local cluster_name resource_group
  cluster_name="$(state_get cluster_name)"
  resource_group="$(state_get resource_group)"

  az aks get-credentials \
    --resource-group "${resource_group}" \
    --name "${cluster_name}" \
    --overwrite-existing \
    --admin

  local msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name
  local msi_vm_ip token_vm_ip offline_vm_ip kubeadm_vm_ip
  local token_vm_private_ip offline_vm_private_ip
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"
  msi_vm_ip="$(state_get msi_vm_ip)"
  token_vm_ip="$(state_get token_vm_ip)"
  offline_vm_ip="$(state_get offline_vm_ip)"
  kubeadm_vm_ip="$(state_get kubeadm_vm_ip)"
  token_vm_private_ip="$(state_get token_vm_private_ip)"
  offline_vm_private_ip="$(state_get offline_vm_private_ip)"

  local failed=0
  validate_node_joined "${msi_vm_name}" || failed=1
  validate_node_joined "${token_vm_name}" || failed=1
  validate_node_joined "${offline_vm_name}" || failed=1
  validate_node_joined "${kubeadm_vm_name}" || failed=1
  validate_node_ip "${token_vm_name}" "${token_vm_private_ip}" || failed=1
  validate_node_ip "${offline_vm_name}" "${offline_vm_private_ip}" || failed=1
  validate_npd_status "${msi_vm_name}" "${msi_vm_ip}" || failed=1
  validate_npd_status "${token_vm_name}" "${token_vm_ip}" || failed=1
  # TODO: re-enable once NPD is included in the upstream Unbounded bootstrap
  # artifact bundle and resolver used by offline artifact mode.
  log_info "Skipping node-problem-detector validation on offline node '${offline_vm_name}'"
  validate_npd_status "${kubeadm_vm_name}" "${kubeadm_vm_ip}" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more nodes failed to join"
    return 1
  fi

  echo ""
  log_info "All cluster nodes:"
  kubectl get nodes -o wide
  log_success "All nodes verified in cluster"
}

# ---------------------------------------------------------------------------
# validate_node_absent - Wait for a node to disappear from the cluster
# ---------------------------------------------------------------------------
validate_node_absent() {
  local vm_name="$1"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0

  log_info "Waiting for node '${vm_name}' to leave cluster (timeout: ${timeout}s)..."

  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    if ! kubectl get node "${vm_name}" &>/dev/null; then
      log_success "Node '${vm_name}' is no longer in the cluster"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    log_debug "Waiting for ${vm_name} to disappear... (${elapsed}/${timeout}s)"
  done

  log_error "Node '${vm_name}' still present in cluster after ${timeout}s"
  log_error "Current nodes:"
  kubectl get nodes 2>&1 || true
  return 1
}

# ---------------------------------------------------------------------------
# validate_all_nodes_absent - Check all flex nodes are gone after unjoin
# ---------------------------------------------------------------------------
validate_all_nodes_absent() {
  log_section "Validating Nodes Absent After Unjoin"

  local msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"

  local failed=0
  # TODO: MSI validation skipped until credential plugin auth is supported
  log_info "Skipping MSI node absence validation (credential plugin auth not yet supported)"
  validate_node_absent "${token_vm_name}" || failed=1
  validate_node_absent "${offline_vm_name}" || failed=1
  validate_node_absent "${kubeadm_vm_name}" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more nodes still present after unjoin"
    return 1
  fi

  echo ""
  log_info "All cluster nodes:"
  kubectl get nodes -o wide
  log_success "All flex nodes confirmed absent"
}

# ---------------------------------------------------------------------------
# smoke_test - Schedule a pod on a specific node and wait for Ready
# ---------------------------------------------------------------------------
smoke_test() {
  local vm_name="$1"
  local label="$2"
  local pod_name="e2e-smoke-${label}"

  log_info "Smoke test: scheduling '${pod_name}' on node '${vm_name}'..."

  # Create pod manifest
  local manifest="${E2E_WORK_DIR}/${pod_name}.yaml"
  cat > "${manifest}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
spec:
  nodeSelector:
    kubernetes.io/hostname: ${vm_name}
  tolerations:
  - effect: NoSchedule
    operator: Exists
  containers:
  - name: nginx
    image: nginx:alpine
    resources:
      requests:
        memory: "64Mi"
        cpu: "100m"
      limits:
        memory: "128Mi"
        cpu: "200m"
EOF

  kubectl apply -f "${manifest}"

  if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${E2E_POD_READY_TIMEOUT}s"; then
    log_success "Smoke test PASSED for '${pod_name}' on '${vm_name}'"
    kubectl get pod "${pod_name}" -o wide
    kubectl delete pod "${pod_name}" --wait=false
    return 0
  else
    log_error "Smoke test FAILED for '${pod_name}' on '${vm_name}'"
    kubectl describe pod "${pod_name}" 2>&1 || true
    kubectl delete pod "${pod_name}" --wait=false 2>/dev/null || true
    return 1
  fi
}

# ---------------------------------------------------------------------------
# smoke_test_all - Run smoke tests on all nodes
# ---------------------------------------------------------------------------
smoke_test_all() {
  log_section "Running Smoke Tests"

  local msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"

  # A default bridge CNI config (99-bridge.conf) is written during bootstrap,
  # making nodes Ready without requiring unbounded-net-node DaemonSet.
  local failed=0
  smoke_test "${msi_vm_name}" "msi" || failed=1
  smoke_test "${token_vm_name}" "token" || failed=1
  smoke_test "${offline_vm_name}" "offline" || failed=1
  smoke_test "${kubeadm_vm_name}" "kubeadm" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more smoke tests failed"
    return 1
  fi

  log_success "All smoke tests passed"
}
