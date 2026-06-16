#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-msi.sh - Join / unjoin an AKS flex node using MSI auth
#
# Functions:
#   node_join_msi   - Generate MSI config, deploy binary, run agent
#   node_unjoin_msi - Simulate RP delete and verify node cleanup
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_MSI_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_MSI_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# node_join_msi - Join the MSI VM
# ---------------------------------------------------------------------------
node_join_msi() {
  log_section "Joining MSI Node"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get msi_vm_ip)"
  local cluster_id
  cluster_id="$(state_get cluster_id)"
  local subscription_id
  subscription_id="$(state_get subscription_id)"
  local tenant_id
  tenant_id="$(state_get tenant_id)"
  local location
  location="$(state_get location)"
  local server_url
  server_url="$(state_get server_url)"
  local ca_cert_data
  ca_cert_data="$(state_get ca_cert_data)"

  # Step 1: Generate MSI config
  local config_file="${E2E_WORK_DIR}/config-msi.json"
  cat > "${config_file}" <<EOF
{
  "azure": {
    "subscriptionId": "${subscription_id}",
    "tenantId": "${tenant_id}",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "${E2E_TARGET_AGENT_POOL_NAME}",
    "managedIdentity": {},
    "targetCluster": {
      "resourceId": "${cluster_id}",
      "location": "${location}"
    }
  },
  "node": {
    "kubelet": {
      "serverURL": "${server_url}",
      "caCertData": "${ca_cert_data}"
    }
  },
  "agent": {
    "logLevel": "debug",
    "logDir": "/var/log/aks-flex-node",
    "e2eMode": true
  },
  "kubernetes": { "version": "${E2E_KUBERNETES_VERSION}" },
  "containerd": { "version": "${E2E_CONTAINERD_VERSION}" },
  "runc": { "version": "${E2E_RUNC_VERSION}" }
}
EOF

  # Step 2: Deploy and start
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-msi"

  log_success "MSI node joined in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# node_unjoin_msi - Simulate RP delete and verify node cleanup
# ---------------------------------------------------------------------------
node_unjoin_msi() {
  log_section "Unjoining MSI Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get msi_vm_ip)"
  vm_name="$(state_get msi_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  log_success "MSI node unjoined in $(timer_elapsed "${start}")s"
}
