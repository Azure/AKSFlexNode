#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/validate.sh - Node join verification and smoke tests
#
# Functions:
#   validate_node_joined  <vm_name>  - Wait for a specific node to appear in kubectl
#   validate_all_nodes                - Verify MSI, token, and kubeadm nodes joined
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
# validate_all_nodes - Check all MSI, token, and kubeadm VMs joined
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

  local msi_vm_name token_vm_name kubeadm_vm_name
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"

  local failed=0
  validate_node_joined "${msi_vm_name}" || failed=1
  validate_node_joined "${token_vm_name}" || failed=1
  validate_node_joined "${kubeadm_vm_name}" || failed=1

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

  local msi_vm_name token_vm_name kubeadm_vm_name
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"

  local failed=0
  smoke_test "${msi_vm_name}" "msi" || failed=1
  smoke_test "${token_vm_name}" "token" || failed=1
  smoke_test "${kubeadm_vm_name}" "kubeadm" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more smoke tests failed"
    return 1
  fi

  log_success "All smoke tests passed"
}
