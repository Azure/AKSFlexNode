#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-kubeadm.sh - Join / unjoin an AKS flex node via
#                                       bootstrap token (kubeadm VM)
#
# Functions:
#   node_join_kubeadm   - Create bootstrap token & RBAC, generate config,
#                         run aks-flex-node agent
#   node_unjoin_kubeadm - Stop agent, run unbootstrap, delete the node object
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_KUBEADM_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_KUBEADM_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# _kubeadm_ensure_rbac - Create / update RBAC and ConfigMaps (idempotent)
# ---------------------------------------------------------------------------
_kubeadm_ensure_rbac() {
  local server_url="$1"
  local ca_cert_data="$2"

  log_info "Ensuring bootstrap RBAC and ConfigMap resources..."

  # RBAC bindings for TLS bootstrapping (idempotent).
  # Mirrors the full set of resources that kubeadm init sets up:
  #  - ClusterRoleBindings for CSR creation and auto-approval
  #  - Roles/RoleBindings granting bootstrappers read access to kubeadm config
  #    and kubelet config (required by kubeadm join's preflight phase)
  #  - ClusterRole/ClusterRoleBinding for bootstrappers to GET nodes
  #  - ConfigMaps: cluster-info (kube-public), kubeadm-config and
  #    kubelet-config (kube-system) consumed by kubeadm join
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
  name: aks-flex-node-auto-approve-certificate-rotation
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:selfnodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:nodes
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
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: kube-system
  name: kubeadm:nodes-kubeadm-config
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["kubeadm-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: kube-system
  name: kubeadm:nodes-kubeadm-config
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:nodes-kubeadm-config
subjects:
- kind: Group
  apiGroup: rbac.authorization.k8s.io
  name: system:bootstrappers:aks-flex-node
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: kube-system
  name: kubeadm:kubelet-config
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["configmaps"]
  resourceNames: ["kubelet-config"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  namespace: kube-system
  name: kubeadm:kubelet-config
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kubeadm:kubelet-config
subjects:
- kind: Group
  apiGroup: rbac.authorization.k8s.io
  name: system:bootstrappers:aks-flex-node
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeadm:get-nodes
rules:
- verbs: ["get"]
  apiGroups: [""]
  resources: ["nodes"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeadm:get-nodes
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeadm:get-nodes
subjects:
- kind: Group
  apiGroup: rbac.authorization.k8s.io
  name: system:bootstrappers:aks-flex-node
EOF

  # Publish the ConfigMaps that kubeadm join reads during its preflight phase.
  # cluster-info goes into kube-public (publicly readable).
  # kubeadm-config and kubelet-config go into kube-system (bootstrapper-readable).
  kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-public
  name: cluster-info
data:
  kubeconfig: |
    apiVersion: v1
    kind: Config
    clusters:
    - cluster:
        certificate-authority-data: ${ca_cert_data}
        server: ${server_url}
      name: ""
    contexts: []
    current-context: ""
    preferences: {}
    users: []
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: kubeadm-config
data:
  ClusterConfiguration: |
    apiVersion: kubeadm.k8s.io/v1beta4
    kind: ClusterConfiguration
    kubernetesVersion: ${E2E_KUBERNETES_VERSION}
    networking:
      serviceSubnet: 10.0.0.0/16
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: kube-system
  name: kubelet-config
data:
  kubelet: |
    apiVersion: kubelet.config.k8s.io/v1beta1
    kind: KubeletConfiguration
EOF

  log_success "Bootstrap RBAC and ConfigMaps configured"
}

# ---------------------------------------------------------------------------
# _kubeadm_create_bootstrap_token - Create a token and print it to stdout
# ---------------------------------------------------------------------------
_kubeadm_create_bootstrap_token() {
  local token_id token_secret bootstrap_token expiration

  token_id="$(openssl rand -hex 3)"
  token_secret="$(openssl rand -hex 8)"
  bootstrap_token="${token_id}.${token_secret}"

  # Use a portable date command for expiration (24h from now)
  if date --version &>/dev/null; then
    # GNU date
    expiration="$(date -u -d "+24 hours" +"%Y-%m-%dT%H:%M:%SZ")"
  else
    # BSD/macOS date
    expiration="$(date -u -v+24H +"%Y-%m-%dT%H:%M:%SZ")"
  fi

  log_info "Token ID: ${token_id} | Expires: ${expiration}" >&2

  kubectl apply -f - >&2 <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${token_id}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  description: "AKS Flex Node E2E kubeadm bootstrap token"
  token-id: "${token_id}"
  token-secret: "${token_secret}"
  expiration: "${expiration}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:aks-flex-node"
EOF

  echo "${bootstrap_token}"
}

# ---------------------------------------------------------------------------
# node_join_kubeadm - Join the Kubeadm VM using bootstrap token config
# ---------------------------------------------------------------------------
node_join_kubeadm() {
  log_section "Joining Kubeadm Node (bootstrap token)"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get kubeadm_vm_ip)"
  local server_url
  server_url="$(state_get server_url)"
  local ca_cert_data
  ca_cert_data="$(state_get ca_cert_data)"
  local cluster_id
  cluster_id="$(state_get cluster_id)"
  local subscription_id
  subscription_id="$(state_get subscription_id)"
  local tenant_id
  tenant_id="$(state_get tenant_id)"
  local location
  location="$(state_get location)"

  # Step 1: Ensure RBAC / ConfigMaps and create a bootstrap token
  _kubeadm_ensure_rbac "${server_url}" "${ca_cert_data}"

  log_info "Creating bootstrap token..."
  local bootstrap_token
  bootstrap_token="$(_kubeadm_create_bootstrap_token)"
  state_set "kubeadm_bootstrap_token" "${bootstrap_token}"

  # Step 2: Generate the config file for aks-flex-node agent
  local config_file="${E2E_WORK_DIR}/config-kubeadm.json"
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
    "labels": {
      "kubernetes.azure.com/managed": "false"
    },
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

  # Step 3: Deploy and start the agent
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-kubeadm"

  log_success "Kubeadm node joined in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# node_unjoin_kubeadm - Stop agent, run unbootstrap, remove node from cluster
# ---------------------------------------------------------------------------
node_unjoin_kubeadm() {
  log_section "Unjoining Kubeadm Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get kubeadm_vm_ip)"
  vm_name="$(state_get kubeadm_vm_name)"

  # Step 1: Stop the agent service and run unbootstrap on the VM.
  log_info "Stopping agent and running unbootstrap on ${vm_ip}..."
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail

sudo systemctl stop aks-flex-node-kubeadm 2>/dev/null || true

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

  log_success "Kubeadm node unjoined in $(timer_elapsed "${start}")s"
}
