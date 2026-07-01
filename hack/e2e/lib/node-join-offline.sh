#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-offline.sh - Join / unjoin an AKS flex node using
#                                      bootstrap token auth with offline assets
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_OFFLINE_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_OFFLINE_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

readonly offlineArtifactsSource='oci://ghcr.io/azure/unbounded/bootstrap-artifacts:alpha-0cd4fe2-k8s-{{ .KubernetesVersion }}'
readonly offlineOCIImage='ghcr.io/azure/agent-ubuntu2404:v20260619'

node_join_offline() {
  log_section "Joining Offline Artifacts Node"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get offline_vm_ip)"
  local vm_private_ip
  vm_private_ip="$(state_get offline_vm_private_ip)"
  local cluster_name
  cluster_name="$(state_get cluster_name)"
  local resource_group
  resource_group="$(state_get resource_group)"
  local subscription_id
  subscription_id="$(state_get subscription_id)"

  if [[ -z "${vm_private_ip}" ]] || ! is_valid_ipv4 "${vm_private_ip}"; then
    log_error "Invalid offline VM private IP in state: '${vm_private_ip}'"
    return 1
  fi

  log_info "Setting up bootstrap token RBAC resources..."
  "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}"

  ensure_daemon_csr_approver

  log_info "Generating offline artifacts config..."
  local config_file="${E2E_WORK_DIR}/config-offline.json"
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
    --arg offlineArtifactsSource "${offlineArtifactsSource}" \
    --arg ociImage "${offlineOCIImage}" \
    '.agent.logLevel = "debug"
      | .agent.e2eMode = true
      | .node.kubelet.nodeIP = $nodeIP
      | .components = (.components // {})
      | .components.kubernetes = $kubernetesVersion
      | del(.components.containerd, .components.runc, .networking.cniVersion)
      | .bootstrap = (.bootstrap // {})
      | .bootstrap.ociImage = $ociImage
      | .bootstrap.offlineArtifacts.source = $offlineArtifactsSource
      | del(.kubernetes, .containerd, .runc)' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"

  jq -e \
    --arg offlineArtifactsSource "${offlineArtifactsSource}" \
    --arg ociImage "${offlineOCIImage}" \
    '.bootstrap.ociImage == $ociImage and .bootstrap.offlineArtifacts.source == $offlineArtifactsSource' \
    "${config_file}" >/dev/null
  log_info "Offline node artifact source: ${offlineArtifactsSource}"
  log_info "Offline node OCI image: ${offlineOCIImage}"

  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-offline"

  log_success "Offline artifacts node joined in $(timer_elapsed "${start}")s"
}

node_unjoin_offline() {
  log_section "Unjoining Offline Artifacts Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get offline_vm_ip)"
  vm_name="$(state_get offline_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  log_success "Offline artifacts node unjoined in $(timer_elapsed "${start}")s"
}
