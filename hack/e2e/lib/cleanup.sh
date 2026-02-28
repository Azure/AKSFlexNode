#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/cleanup.sh - Resource cleanup and log collection
#
# Functions:
#   collect_logs  - SSH into VMs and download service/agent/kubelet logs
#   cleanup       - Delete all Azure resources created during the test
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_CLEANUP_LOADED:-}" ]] && return 0
readonly _E2E_CLEANUP_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# _collect_vm_logs - Collect logs from a single VM
# ---------------------------------------------------------------------------
_collect_vm_logs() {
  local vm_ip="$1"
  local prefix="$2"

  log_info "Collecting logs from ${vm_ip} (${prefix})..."

  remote_exec "${vm_ip}" \
    "sudo cat /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-aks-flex-node.log" 2>/dev/null || true

  remote_exec "${vm_ip}" \
    "sudo journalctl -u 'aks-flex-node-*' -n 500 --no-pager 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-agent-journal.log" 2>/dev/null || true

  remote_exec "${vm_ip}" \
    "sudo journalctl -u kubelet -n 500 --no-pager 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-kubelet.log" 2>/dev/null || true

  remote_exec "${vm_ip}" \
    "sudo journalctl -u containerd -n 200 --no-pager 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-containerd.log" 2>/dev/null || true

  log_info "Logs saved to ${E2E_LOG_DIR}/${prefix}-*.log"
}

# ---------------------------------------------------------------------------
# collect_logs - Collect logs from all VMs
# ---------------------------------------------------------------------------
collect_logs() {
  log_section "Collecting Logs"

  mkdir -p "${E2E_LOG_DIR}"

  local msi_vm_ip token_vm_ip kubeadm_vm_ip
  msi_vm_ip="$(state_get msi_vm_ip)"
  token_vm_ip="$(state_get token_vm_ip)"
  kubeadm_vm_ip="$(state_get kubeadm_vm_ip)"

  if [[ -n "${msi_vm_ip}" ]]; then
    _collect_vm_logs "${msi_vm_ip}" "msi" || true
  fi

  if [[ -n "${token_vm_ip}" ]]; then
    _collect_vm_logs "${token_vm_ip}" "token" || true
  fi

  if [[ -n "${kubeadm_vm_ip}" ]]; then
    _collect_vm_logs "${kubeadm_vm_ip}" "kubeadm" || true
  fi

  # Also capture cluster-side info
  {
    echo "=== Nodes ==="
    kubectl get nodes -o wide 2>/dev/null || true
    echo ""
    echo "=== CSRs ==="
    kubectl get csr 2>/dev/null || true
    echo ""
    echo "=== Events (last 50) ==="
    kubectl get events --sort-by='.lastTimestamp' -A 2>/dev/null | tail -50 || true
  } > "${E2E_LOG_DIR}/cluster-info.log" 2>&1

  log_success "Logs collected in ${E2E_LOG_DIR}/"
  ls -la "${E2E_LOG_DIR}/"
}

# ---------------------------------------------------------------------------
# cleanup - Delete Azure resources
# ---------------------------------------------------------------------------
cleanup() {
  log_section "Cleaning Up Resources"

  if [[ "${E2E_SKIP_CLEANUP}" == "1" ]]; then
    log_warn "Cleanup skipped (E2E_SKIP_CLEANUP=1)"
    log_info "Resources left for debugging:"
    state_dump
    return 0
  fi

  local resource_group cluster_name msi_vm_name token_vm_name kubeadm_vm_name
  resource_group="$(state_get resource_group)"
  cluster_name="$(state_get cluster_name)"
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"
  local deployment_name
  deployment_name="$(state_get deployment_name)"

  if [[ -z "${resource_group}" ]]; then
    log_warn "No resource group in state; nothing to clean up"
    return 0
  fi

  # Delete VMs first (faster than waiting for full RG delete)
  log_info "[1/5] Deleting MSI VM: ${msi_vm_name}..."
  az vm delete --resource-group "${resource_group}" --name "${msi_vm_name}" \
    --force-deletion yes --yes --no-wait 2>/dev/null || true

  log_info "[2/5] Deleting Token VM: ${token_vm_name}..."
  az vm delete --resource-group "${resource_group}" --name "${token_vm_name}" \
    --force-deletion yes --yes --no-wait 2>/dev/null || true

  log_info "[3/5] Deleting Kubeadm VM: ${kubeadm_vm_name}..."
  az vm delete --resource-group "${resource_group}" --name "${kubeadm_vm_name}" \
    --force-deletion yes --yes --no-wait 2>/dev/null || true

  # Clean up leftover networking resources tied to our deployment
  log_info "[4/5] Cleaning up networking resources..."
  local run_id="${GITHUB_RUN_ID:-}"
  if [[ -n "${run_id}" ]]; then
    for res_type in networkInterfaces publicIPAddresses networkSecurityGroups disks; do
      az resource list --resource-group "${resource_group}" \
        --query "[?tags.\"github-run\"=='${run_id}' && contains(type, '${res_type}')].id" \
        -o tsv 2>/dev/null | while read -r id; do
          az resource delete --ids "${id}" --no-wait 2>/dev/null || true
        done
    done
  fi

  log_info "[5/5] Deleting AKS cluster: ${cluster_name}..."
  az aks delete --resource-group "${resource_group}" --name "${cluster_name}" \
    --yes --no-wait 2>/dev/null || true

  # If we created the VNet/NSG via Bicep, they'll be cleaned up when no
  # resources reference them, or on next deployment.  We don't delete the
  # resource group itself since it may be shared.

  log_success "Cleanup initiated (async deletes in progress)"
}
