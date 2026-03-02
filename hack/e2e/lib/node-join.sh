#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join.sh - Bootstrap flex nodes into the AKS cluster
#
# Sources:
#   node-join-msi.sh     - MSI auth node join     (node_join_msi)
#   node-join-token.sh   - Bootstrap token join   (node_join_token)
#   node-join-kubeadm.sh - Kubeadm apply -f join  (node_join_kubeadm)
#
# Functions:
#   node_join_all  - Join all nodes (MSI, token, and kubeadm) in parallel
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

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/node-join-msi.sh"
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/node-join-token.sh"
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/node-join-kubeadm.sh"

# ---------------------------------------------------------------------------
# node_join_all - Join all nodes in parallel
# ---------------------------------------------------------------------------
node_join_all() {
  log_section "Joining All Nodes (parallel)"
  local start
  start=$(timer_start)

  local msi_pid token_pid kubeadm_pid
  local msi_exit=0 token_exit=0 kubeadm_exit=0

  node_join_msi &
  msi_pid=$!

  node_join_token &
  token_pid=$!

  node_join_kubeadm &
  kubeadm_pid=$!

  wait "${msi_pid}" || msi_exit=$?
  wait "${token_pid}" || token_exit=$?
  wait "${kubeadm_pid}" || kubeadm_exit=$?

  local duration
  duration=$(timer_elapsed "${start}")

  if [[ "${msi_exit}" -ne 0 ]]; then
    log_error "MSI node join failed (exit ${msi_exit})"
  fi
  if [[ "${token_exit}" -ne 0 ]]; then
    log_error "Token node join failed (exit ${token_exit})"
  fi
  if [[ "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Kubeadm node join failed (exit ${kubeadm_exit})"
  fi

  if [[ "${msi_exit}" -ne 0 || "${token_exit}" -ne 0 || "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Node joins failed (${duration}s)"
    return 1
  fi

  log_success "All nodes joined in ${duration}s"
}
