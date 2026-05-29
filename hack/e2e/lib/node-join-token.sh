#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-token.sh - Join / unjoin an AKS flex node using
#                                    bootstrap token auth
#
# Functions:
#   node_join_token   - Create bootstrap token/RBAC, deploy binary, run agent
#   node_unjoin_token - Stop agent, run reset, delete node from cluster
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_TOKEN_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_TOKEN_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# node_join_token - Join the Token VM
# ---------------------------------------------------------------------------
node_join_token() {
  log_section "Joining Token Node"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get token_vm_ip)"
  local vm_private_ip
  vm_private_ip="$(state_get token_vm_private_ip)"
  local cluster_name
  cluster_name="$(state_get cluster_name)"
  local resource_group
  resource_group="$(state_get resource_group)"
  local subscription_id
  subscription_id="$(state_get subscription_id)"

  if [[ -z "${vm_private_ip}" ]] || ! is_valid_ipv4 "${vm_private_ip}"; then
    log_error "Invalid token VM private IP in state: '${vm_private_ip}'"
    return 1
  fi

  log_info "Setting up bootstrap token RBAC resources..."
  "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}"

  ensure_daemon_csr_approver

  log_info "Generating token config..."
  local config_file="${E2E_WORK_DIR}/config-token.json"
  "${REPO_ROOT}/scripts/aks-flex-config" generate-node-config \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" \
    --bootstrap-token \
    --output "${config_file}"

  jq \
    --arg nodeIP "${vm_private_ip}" \
    --arg kubernetesVersion "${E2E_KUBERNETES_VERSION}" \
    --arg containerdVersion "${E2E_CONTAINERD_VERSION}" \
    --arg runcVersion "${E2E_RUNC_VERSION}" \
    '.agent.logLevel = "debug"
      | .agent.e2eMode = true
      | .node.kubelet.nodeIP = $nodeIP
      | .kubernetes.version = $kubernetesVersion
      | .containerd.version = $containerdVersion
      | .runc.version = $runcVersion' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"

  # Step 3: Deploy and start
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-token"

  log_success "Token node joined in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# node_unjoin_token - Stop the agent, run reset, remove node from cluster
# ---------------------------------------------------------------------------
node_unjoin_token() {
  log_section "Unjoining Token Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get token_vm_ip)"
  vm_name="$(state_get token_vm_name)"

  # Step 1: Stop the agent service and run reset on the VM.
  # The public uninstall script still invokes the unbootstrap alias for backward
  # compatibility. The reset flow does not delete the node object.
  log_info "Running uninstall script on ${vm_ip}..."
  remote_copy "${REPO_ROOT}/scripts/uninstall.sh" "${vm_ip}" "/tmp/aks-flex-node-uninstall.sh"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail

sudo SKIP_AZCLI=true bash /tmp/aks-flex-node-uninstall.sh --force

if [[ -e /usr/local/bin/aks-flex-node ]]; then
  echo "aks-flex-node binary still exists after uninstall"
  exit 1
fi
if systemctl list-unit-files aks-flex-node-agent.service --no-legend | grep -q '^aks-flex-node-agent.service'; then
  echo "aks-flex-node-agent.service still exists after uninstall"
  exit 1
fi

echo "kubelet status after reset:"
systemctl is-active kubelet 2>&1 || true
echo "containerd status after reset:"
systemctl is-active containerd 2>&1 || true
REMOTE

  # Step 2: Delete the node object from the API server so validation passes
  # without waiting for the node controller to evict it.
  log_info "Deleting node '${vm_name}' from cluster..."
  kubectl delete node "${vm_name}" --ignore-not-found --wait=false

  log_success "Token node unjoined in $(timer_elapsed "${start}")s"
}
