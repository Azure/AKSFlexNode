#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-kubeadm.sh - Join an AKS flex node using the kubeadm join flow
#
# Functions:
#   node_join_kubeadm  - Create bootstrap token, generate action file,
#                        run aks-flex-node apply -f (KubeadmNodeJoin)
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_KUBEADM_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_KUBEADM_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# node_join_kubeadm - Join the Kubeadm VM using apply -f with KubeadmNodeJoin
# ---------------------------------------------------------------------------
node_join_kubeadm() {
  log_section "Joining Kubeadm Node (apply -f)"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get kubeadm_vm_ip)"
  local server_url
  server_url="$(state_get server_url)"
  local ca_cert_data
  ca_cert_data="$(state_get ca_cert_data)"

  # Step 1: Create bootstrap token & RBAC in the cluster
  log_info "Creating bootstrap token and RBAC resources for kubeadm join..."
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
  description: "AKS Flex Node E2E kubeadm bootstrap token"
  token-id: "${token_id}"
  token-secret: "${token_secret}"
  expiration: "${expiration}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:aks-flex-node"
EOF

  # Create RBAC bindings for TLS bootstrapping (idempotent).
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

  log_success "Bootstrap token and RBAC configured"
  state_set "kubeadm_bootstrap_token" "${bootstrap_token}"

  # Step 2: Generate the apply -f action file (JSON array of all bootstrap steps
  #         ending with the KubeadmNodeJoin action)
  local action_file="${E2E_WORK_DIR}/kubeadm-join.json"
  cat > "${action_file}" <<EOF
[
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.linux.ConfigureBaseOS",
      "name": "configure-os"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.cri.DownloadCRIBinaries",
      "name": "download-cri-binaries"
    },
    "spec": {
      "containerdVersion": "${E2E_CONTAINERD_VERSION}",
      "runcVersion": "${E2E_RUNC_VERSION}"
    }
  },
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.kubebins.DownloadKubeBinaries",
      "name": "download-kube-binaries"
    },
    "spec": {
      "kubernetesVersion": "${E2E_KUBERNETES_VERSION}"
    }
  },
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.cni.DownloadCNIBinaries",
      "name": "download-cni-binaries"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.cni.ConfigureCNI",
      "name": "configure-cni"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.cri.StartContainerdService",
      "name": "start-containerd"
    },
    "spec": {}
  },
  {
    "metadata": {
      "type": "type.googleapis.com/aks.flex.components.kubeadm.KubeadmNodeJoin",
      "name": "kubeadm-node-join"
    },
    "spec": {
      "controlPlane": {
        "server": "${server_url}",
        "certificateAuthorityData": "${ca_cert_data}"
      },
      "kubelet": {
        "bootstrapAuthInfo": {
          "token": "${bootstrap_token}"
        }
      }
    }
  }
]
EOF

  # Step 3: Upload binary and action file, then run apply -f directly
  log_info "Uploading binary and action file to ${vm_ip}..."
  remote_copy "${E2E_BINARY}" "${vm_ip}" "/tmp/aks-flex-node-binary"
  remote_copy "${action_file}" "${vm_ip}" "/tmp/kubeadm-join.json"

  log_info "Running kubeadm join via apply -f on ${vm_ip}..."
  remote_exec "${vm_ip}" 'bash -s' <<REMOTE
set -euo pipefail

sudo cp /tmp/aks-flex-node-binary /usr/local/bin/aks-flex-node
sudo chmod +x /usr/local/bin/aks-flex-node
aks-flex-node version

sudo mkdir -p /etc/aks-flex-node /var/log/aks-flex-node
sudo cp /tmp/kubeadm-join.json /etc/aks-flex-node/

sudo /usr/local/bin/aks-flex-node apply --no-prettyui -f /etc/aks-flex-node/kubeadm-join.json \
  2>&1 | sudo tee /var/log/aks-flex-node/aks-flex-node.log

if systemctl is-active --quiet kubelet; then
  echo "kubelet is running"
else
  echo "kubelet status:"
  systemctl status kubelet --no-pager -l 2>&1 || true
fi
REMOTE

  log_success "Kubeadm node joined via apply -f in $(timer_elapsed "${start}")s"
}
