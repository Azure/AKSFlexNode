#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join.sh - Bootstrap flex nodes into the AKS cluster
#
# Functions:
#   node_join_msi   - Install Azure CLI + MSI auth, deploy binary, run agent
#   node_join_token - Create bootstrap token/RBAC, deploy binary, run agent
#   node_join_all   - Join both nodes (MSI first, then token)
#
# Each function:
#   1. Generates the appropriate config.json
#   2. SCPs the binary + config onto the VM
#   3. Starts the agent via systemd-run
#   4. Waits for kubelet to report running
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# Internal: upload binary & config, start agent on a VM
# ---------------------------------------------------------------------------
_deploy_and_start_agent() {
  local vm_ip="$1"
  local config_file="$2"
  local unit_name="$3"

  log_info "Uploading binary and config to ${vm_ip}..."
  remote_copy "${E2E_BINARY}" "${vm_ip}" "/tmp/aks-flex-node-binary"
  remote_copy "${config_file}" "${vm_ip}" "/tmp/config.json"

  log_info "Starting flex node agent on ${vm_ip}..."
  remote_exec "${vm_ip}" 'bash -s' <<REMOTE
set -euo pipefail

sudo cp /tmp/aks-flex-node-binary /usr/local/bin/aks-flex-node
sudo chmod +x /usr/local/bin/aks-flex-node
aks-flex-node version

sudo mkdir -p /etc/aks-flex-node /var/log/aks-flex-node
sudo cp /tmp/config.json /etc/aks-flex-node/

sudo systemd-run \
  --unit=${unit_name} \
  --description="AKS Flex Node E2E (${unit_name})" \
  --remain-after-exit \
  /usr/local/bin/aks-flex-node agent --config /etc/aks-flex-node/config.json

echo "Waiting ${E2E_BOOTSTRAP_SETTLE_TIME}s for bootstrap to complete..."
sleep ${E2E_BOOTSTRAP_SETTLE_TIME}

if systemctl is-active --quiet ${unit_name}; then
  echo "Agent service is running"
else
  echo "Agent service failed:"
  sudo journalctl -u ${unit_name} -n 50 --no-pager || true
  sudo tail -n 50 /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null || true
  exit 1
fi

sleep 10
if systemctl is-active --quiet kubelet; then
  echo "kubelet is running"
else
  echo "kubelet status:"
  systemctl status kubelet --no-pager -l 2>&1 || true
fi
REMOTE

  log_success "Agent started on ${vm_ip}"
}

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

# ---------------------------------------------------------------------------
# node_join_token - Join the Token VM
# ---------------------------------------------------------------------------
node_join_token() {
  log_section "Joining Token Node"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get token_vm_ip)"
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

  # Step 1: Create bootstrap token & RBAC in the cluster
  log_info "Creating bootstrap token and RBAC resources..."
  local token_id token_secret bootstrap_token expiration

  token_id="$(openssl rand -hex 3)"
  token_secret="$(openssl rand -hex 8)"
  bootstrap_token="${token_id}.${token_secret}"

  # Use a portable date command for expiration (24h from now)
  if date --version &>/dev/null 2>&1; then
    # GNU date
    expiration="$(date -u -d "+24 hours" +"%Y-%m-%dT%H:%M:%SZ")"
  else
    # BSD/macOS date
    expiration="$(date -u -v+24H +"%Y-%m-%dT%H:%M:%SZ")"
  fi

  log_info "Token ID: ${token_id} | Expires: ${expiration}"

  # Create the bootstrap token secret
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${token_id}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  description: "AKS Flex Node E2E bootstrap token"
  token-id: "${token_id}"
  token-secret: "${token_secret}"
  expiration: "${expiration}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:aks-flex-node"
EOF

  # Create RBAC bindings for TLS bootstrapping
  kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-bootstrapper
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node-bootstrapper
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-auto-approve-csr
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-role
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
EOF

  log_success "Bootstrap token and RBAC configured"
  state_set "bootstrap_token" "${bootstrap_token}"

  # Step 2: Generate token config
  local config_file="${E2E_WORK_DIR}/config-token.json"
  cat > "${config_file}" <<EOF
{
  "azure": {
    "subscriptionId": "${subscription_id}",
    "tenantId": "${tenant_id}",
    "cloud": "AzurePublicCloud",
    "bootstrapToken": {
      "token": "${bootstrap_token}"
    },
    "arc": { "enabled": false },
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
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-token"

  log_success "Token node joined in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# node_join_all - Join both nodes
# ---------------------------------------------------------------------------
node_join_all() {
  node_join_msi
  node_join_token
}
