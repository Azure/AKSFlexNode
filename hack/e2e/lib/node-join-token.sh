#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-token.sh - Join / unjoin an AKS flex node using
#                                    bootstrap token auth
#
# Functions:
#   node_join_token   - Create bootstrap token/RBAC, deploy binary, run agent
#   node_unjoin_token - Stop agent, run unbootstrap, delete node from cluster
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
# node_unjoin_token - Stop the agent, run unbootstrap, remove node from cluster
# ---------------------------------------------------------------------------
node_unjoin_token() {
  log_section "Unjoining Token Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get token_vm_ip)"
  vm_name="$(state_get token_vm_name)"

  # Step 1: Stop the agent service and run unbootstrap on the VM.
  # The unbootstrap command runs best-effort: ResetKubelet, ResetContainerdService,
  # and ArcUnbootstrap (no-op since Arc is disabled for token auth). It does not
  # delete the node object.
  log_info "Stopping agent and running unbootstrap on ${vm_ip}..."
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail

sudo systemctl stop aks-flex-node-token 2>/dev/null || true

sudo /usr/local/bin/aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json \
  2>&1 | sudo tee -a /var/log/aks-flex-node/aks-flex-node.log

echo "kubelet status after unbootstrap:"
systemctl is-active kubelet 2>&1 || true
echo "containerd status after unbootstrap:"
systemctl is-active containerd 2>&1 || true
REMOTE

  # Step 2: Delete the node object from the API server so validation passes
  # without waiting for the node controller to evict it.
  log_info "Deleting node '${vm_name}' from cluster..."
  kubectl delete node "${vm_name}" --ignore-not-found --wait=false

  log_success "Token node unjoined in $(timer_elapsed "${start}")s"
}
