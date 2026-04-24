#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join.sh - Bootstrap flex nodes into the AKS cluster
#
# Sources:
#   node-join-msi.sh     - MSI auth node join/unjoin     (node_join_msi, node_unjoin_msi)
#   node-join-token.sh   - Bootstrap token join/unjoin   (node_join_token, node_unjoin_token)
#   node-join-kubeadm.sh - Kubeadm apply -f join/unjoin  (node_join_kubeadm, node_unjoin_kubeadm)
#
# Functions:
#   node_join_all   - Join all nodes (MSI, token, and kubeadm) in parallel
#   node_unjoin_all - Unjoin all nodes in parallel
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

# Clean up any leftover transient unit from a previous run
sudo systemctl stop ${unit_name} 2>/dev/null || true
sudo systemctl reset-failed ${unit_name} 2>/dev/null || true

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

# Dump nspawn machine status for debugging
echo "=== nspawn machines ==="
machinectl list --no-pager 2>&1 || true
echo "=== nspawn machine kube1 status ==="
machinectl status kube1 --no-pager 2>&1 || echo "(kube1 not found)"

# Check kubelet inside nspawn container
if machinectl show kube1 &>/dev/null 2>&1; then
  echo "=== kubelet status inside kube1 ==="
  sudo systemd-run --machine=kube1 --quiet --pipe systemctl status kubelet --no-pager -l 2>&1 || true
  echo "=== kubelet journal inside kube1 (last 30 lines) ==="
  sudo systemd-run --machine=kube1 --quiet --pipe journalctl -u kubelet -n 30 --no-pager 2>&1 || true
else
  echo "kube1 machine not running, checking host kubelet:"
  systemctl status kubelet --no-pager -l 2>&1 || true
fi

# Dump agent logs for debugging
echo "=== agent logs (last 30 lines) ==="
sudo journalctl -u ${unit_name} -n 30 --no-pager 2>&1 || true
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

  # TODO: MSI join is skipped until credential plugin auth is supported
  # in the shared agent library. Currently only bootstrap token auth works.
  local token_pid kubeadm_pid
  local token_exit=0 kubeadm_exit=0

  log_info "Skipping MSI node join (credential plugin auth not yet supported)"

  node_join_token &
  token_pid=$!

  node_join_kubeadm &
  kubeadm_pid=$!

  wait "${token_pid}" || token_exit=$?
  wait "${kubeadm_pid}" || kubeadm_exit=$?

  local duration
  duration=$(timer_elapsed "${start}")

  if [[ "${token_exit}" -ne 0 ]]; then
    log_error "Token node join failed (exit ${token_exit})"
  fi
  if [[ "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Kubeadm node join failed (exit ${kubeadm_exit})"
  fi

  if [[ "${token_exit}" -ne 0 || "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Node joins failed (${duration}s)"
    return 1
  fi

  log_success "All nodes joined in ${duration}s"
}

# ---------------------------------------------------------------------------
# node_unjoin_all - Unjoin all nodes in parallel
# ---------------------------------------------------------------------------
node_unjoin_all() {
  log_section "Unjoining All Nodes (parallel)"
  local start
  start=$(timer_start)

  # TODO: MSI unjoin skipped (MSI join is skipped)
  local token_pid kubeadm_pid
  local token_exit=0 kubeadm_exit=0

  log_info "Skipping MSI node unjoin (credential plugin auth not yet supported)"

  node_unjoin_token &
  token_pid=$!

  node_unjoin_kubeadm &
  kubeadm_pid=$!

  wait "${token_pid}" || token_exit=$?
  wait "${kubeadm_pid}" || kubeadm_exit=$?

  local duration
  duration=$(timer_elapsed "${start}")

  if [[ "${token_exit}" -ne 0 ]]; then
    log_error "Token node unjoin failed (exit ${token_exit})"
  fi
  if [[ "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Kubeadm node unjoin failed (exit ${kubeadm_exit})"
  fi

  if [[ "${token_exit}" -ne 0 || "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Node unjoins failed (${duration}s)"
    return 1
  fi

  log_success "All nodes unjoined in ${duration}s"
}
