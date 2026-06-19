#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-token.sh - Join / unjoin an AKS flex node using
#                                    bootstrap token auth
#
# Functions:
#   node_join_token   - Create bootstrap token/RBAC, deploy binary, run agent
#   node_unjoin_token - Simulate RP delete and verify node cleanup
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
    --agent-pool-name "${E2E_TARGET_AGENT_POOL_NAME}" \
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
      | .components = (.components // {})
      | .components.kubernetes = $kubernetesVersion
      | .components.containerd = $containerdVersion
      | .components.runc = $runcVersion
      | del(.kubernetes, .containerd, .runc)' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"

  # Step 3: Deploy and start
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-token"

  log_success "Token node joined in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# node_join_azlinux3 - Join the optional Azure Linux 3 host VM using the
# Azure Linux 3 nspawn OCI image.
# ---------------------------------------------------------------------------
node_join_azlinux3() {
  log_section "Joining Azure Linux 3 Node"
  local start
  start=$(timer_start)

  local vm_ip vm_private_ip cluster_name resource_group subscription_id
  vm_ip="$(state_get azlinux3_vm_ip)"
  vm_private_ip="$(state_get azlinux3_vm_private_ip)"
  cluster_name="$(state_get cluster_name)"
  resource_group="$(state_get resource_group)"
  subscription_id="$(state_get subscription_id)"

  if [[ -z "${vm_private_ip}" ]] || ! is_valid_ipv4 "${vm_private_ip}"; then
    log_error "Invalid Azure Linux 3 VM private IP in state: '${vm_private_ip}'"
    return 1
  fi

  log_info "Setting up bootstrap token RBAC resources..."
  "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}"

  ensure_daemon_csr_approver

  log_info "Generating Azure Linux 3 token config..."
  local config_file="${E2E_WORK_DIR}/config-azlinux3.json"
  "${REPO_ROOT}/scripts/aks-flex-config" generate-node-config \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" \
    --agent-pool-name "${E2E_TARGET_AGENT_POOL_NAME}" \
    --bootstrap-token \
    --output "${config_file}"

  jq \
    --arg nodeIP "${vm_private_ip}" \
    --arg kubernetesVersion "${E2E_KUBERNETES_VERSION}" \
    --arg containerdVersion "${E2E_CONTAINERD_VERSION}" \
    --arg runcVersion "${E2E_RUNC_VERSION}" \
    '.agent.logLevel = "debug"
      | .agent.e2eMode = true
      | .agent.ociImage = "ghcr.io/azure/agent-azlinux3:v20260619"
      | .node.kubelet.nodeIP = $nodeIP
      | .components = (.components // {})
      | .components.kubernetes = $kubernetesVersion
      | .components.containerd = $containerdVersion
      | .components.runc = $runcVersion
      | del(.kubernetes, .containerd, .runc)' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"

  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-azlinux3"

  log_success "Azure Linux 3 node joined in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# node_unjoin_token - Simulate RP delete and verify node cleanup
# ---------------------------------------------------------------------------
node_unjoin_token() {
  log_section "Unjoining Token Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get token_vm_ip)"
  vm_name="$(state_get token_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  log_success "Token node unjoined in $(timer_elapsed "${start}")s"
}

node_unjoin_azlinux3() {
  log_section "Unjoining Azure Linux 3 Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get azlinux3_vm_ip)"
  vm_name="$(state_get azlinux3_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  log_success "Azure Linux 3 node unjoined in $(timer_elapsed "${start}")s"
}
