#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-msi.sh - Join an AKS flex node using MSI auth
#
# Functions:
#   node_join_msi  - Install Azure CLI + MSI auth, deploy binary, run agent
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

  # Step 1: Install Azure CLI on VM and log in with MSI
  log_info "Installing Azure CLI on MSI VM (${vm_ip})..."
  remote_exec "${vm_ip}" 'bash -s' <<'AZURECLI'
set -euo pipefail

MAX_RETRIES=5
RETRY_DELAY=15
for attempt in $(seq 1 $MAX_RETRIES); do
  while sudo fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1; do
    sleep 5
  done

  if sudo apt-get update -qq && curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash; then
    echo "Azure CLI installed"
    break
  fi

  if [ "$attempt" -lt "$MAX_RETRIES" ]; then
    sudo dpkg --configure -a 2>/dev/null || true
    sleep $RETRY_DELAY
  else
    echo "Azure CLI installation failed after ${MAX_RETRIES} attempts"
    exit 1
  fi
done

az login --identity --output none
sudo az login --identity --output none
echo "Azure CLI authenticated with managed identity"
AZURECLI

  # Step 2: Generate MSI config
  local config_file="${E2E_WORK_DIR}/config-msi.json"
  cat > "${config_file}" <<EOF
{
  "azure": {
    "subscriptionId": "${subscription_id}",
    "tenantId": "${tenant_id}",
    "cloud": "AzurePublicCloud",
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
    "logDir": "/var/log/aks-flex-node"
  },
  "kubernetes": { "version": "${E2E_KUBERNETES_VERSION}" },
  "containerd": { "version": "${E2E_CONTAINERD_VERSION}" },
  "runc": { "version": "${E2E_RUNC_VERSION}" }
}
EOF

  # Step 3: Deploy and start
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-msi"

  log_success "MSI node joined in $(timer_elapsed "${start}")s"
}
