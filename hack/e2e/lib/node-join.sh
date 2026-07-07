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
  remote_copy "${REPO_ROOT}/scripts/install.sh" "${vm_ip}" "/tmp/aks-flex-node-install.sh"

  log_info "Installing and starting flex node agent on ${vm_ip}..."
  remote_exec "${vm_ip}" "UNIT_NAME=${unit_name} E2E_NODE_JOIN_TIMEOUT=${E2E_NODE_JOIN_TIMEOUT} E2E_KUBERNETES_VERSION=${E2E_KUBERNETES_VERSION} bash -s" <<'REMOTE'
set -euo pipefail

sudo AKS_FLEX_NODE_LOCAL_BINARY=/tmp/aks-flex-node-binary \
  AKS_FLEX_NODE_VERSION=e2e-local \
  SKIP_AZCLI=true \
  bash /tmp/aks-flex-node-install.sh --yes

aks-flex-node version

sudo cp /tmp/config.json /etc/aks-flex-node/
sudo mkdir -p /run/aks-flex-node
sudo tee /run/aks-flex-node/e2e-machine.json >/dev/null <<EOF
{
  "id": "local-test-machine",
  "goal": {
    "kubernetesVersion": "${E2E_KUBERNETES_VERSION}",
    "settingsVersion": "${E2E_KUBERNETES_VERSION}"
  }
}
EOF

if command -v apt-get >/dev/null 2>&1; then
  echo "Installing host packages required by preflight..."
  sudo DEBIAN_FRONTEND=noninteractive apt-get update
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y systemd-container curl nftables util-linux
fi

preflight_log="/tmp/aks-flex-node-preflight.log"
echo "Running preflight checks before bootstrap..."
set +e
{
  echo "=== preflight ${UNIT_NAME} $(date -Is) ==="
  sudo /usr/local/bin/aks-flex-node preflight --config /etc/aks-flex-node/config.json --output text
  preflight_rc=$?
  echo "=== preflight ${UNIT_NAME} exit ${preflight_rc} ==="
  exit "${preflight_rc}"
} 2>&1 | sudo tee -a "${preflight_log}"
preflight_rc=${PIPESTATUS[0]}
set -e
if (( preflight_rc != 0 )); then
  echo "Preflight checks failed with exit code ${preflight_rc}"
  exit "${preflight_rc}"
fi

# Clean up any leftover transient unit from a previous run
sudo systemctl stop "${UNIT_NAME}" 2>/dev/null || true
sudo systemctl reset-failed "${UNIT_NAME}" 2>/dev/null || true

sudo systemd-run \
  --unit="${UNIT_NAME}" \
  --description="AKS Flex Node E2E (${UNIT_NAME})" \
  --remain-after-exit \
  /usr/local/bin/aks-flex-node bootstrap --config /etc/aks-flex-node/config.json

echo "Waiting up to ${E2E_NODE_JOIN_TIMEOUT}s for aks-flex-node-agent.service to start..."
deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
while ! systemctl is-active --quiet aks-flex-node-agent.service; do
  if systemctl is-failed --quiet "${UNIT_NAME}"; then
    echo "Bootstrap unit failed:"
    sudo systemctl status "${UNIT_NAME}" --no-pager -l || true
    sudo journalctl -u "${UNIT_NAME}" -n 50 --no-pager || true
    sudo tail -n 50 /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null || true
    exit 1
  fi

  if (( SECONDS >= deadline )); then
    echo "Timed out waiting for aks-flex-node-agent.service to become active"
    sudo systemctl status "${UNIT_NAME}" --no-pager -l || true
    sudo systemctl status aks-flex-node-agent.service --no-pager -l || true
    sudo journalctl -u "${UNIT_NAME}" -n 50 --no-pager || true
    sudo journalctl -u aks-flex-node-agent.service -n 50 --no-pager || true
    sudo tail -n 50 /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null || true
    exit 1
  fi

  sleep 5
done

echo "aks-flex-node-agent.service became active"

echo "Validating aks-flex-node-agent.service..."
if ! systemctl list-unit-files aks-flex-node-agent.service --no-legend | grep -q '^aks-flex-node-agent.service'; then
  echo "aks-flex-node-agent.service unit file is not installed"
  sudo systemctl list-unit-files 'aks-flex-node*' --no-pager || true
  exit 1
fi

if ! systemctl is-enabled --quiet aks-flex-node-agent.service; then
  echo "aks-flex-node-agent.service is not enabled"
  sudo systemctl status aks-flex-node-agent.service --no-pager -l || true
  exit 1
fi

if ! systemctl is-active --quiet aks-flex-node-agent.service; then
  echo "aks-flex-node-agent.service is not active"
  sudo systemctl status aks-flex-node-agent.service --no-pager -l || true
  sudo journalctl -u aks-flex-node-agent.service -n 50 --no-pager || true
  sudo tail -n 50 /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null || true
  exit 1
fi

echo "aks-flex-node-agent.service is installed, enabled, and active"

sleep 10

# Dump nspawn machine status for debugging
echo "=== nspawn machines ==="
machinectl list --no-pager 2>&1 || true
for machine in kube1 kube2; do
  echo "=== nspawn machine $machine status ==="
  machinectl status "$machine" --no-pager 2>&1 || echo "($machine not found)"

  # Check kubelet inside each nspawn side when present.
  if machinectl show "$machine" &>/dev/null 2>&1; then
    echo "=== kubelet status inside $machine ==="
    sudo systemd-run --machine="$machine" --quiet --pipe systemctl status kubelet --no-pager -l 2>&1 || true
    echo "=== kubelet journal inside $machine (last 30 lines) ==="
    sudo systemd-run --machine="$machine" --quiet --pipe journalctl -u kubelet -n 30 --no-pager 2>&1 || true
  fi
done

# Dump agent logs for debugging
echo "=== agent logs (last 30 lines) ==="
sudo journalctl -u "${UNIT_NAME}" -n 30 --no-pager 2>&1 || true
echo "=== aks-flex-node-agent.service logs (last 30 lines) ==="
sudo journalctl -u aks-flex-node-agent.service -n 30 --no-pager 2>&1 || true
REMOTE

  log_success "Agent started on ${vm_ip}"
}

# ---------------------------------------------------------------------------
# Internal: simulate AKS RP machine deletion for a joined local_e2e node.
# ---------------------------------------------------------------------------
_rp_delete_unjoin_node() {
  local vm_ip="$1"
  local vm_name="$2"

  log_info "Deleting local Machine resource on ${vm_ip}..."
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
sudo rm -f /run/aks-flex-node/e2e-machine.json
REMOTE

  log_info "Tainting node '${vm_name}' with RP deletion signal..."
  if kubectl get node "${vm_name}" &>/dev/null; then
    kubectl taint node "${vm_name}" kubernetes.azure.com/flex-node-deleting=true:NoSchedule --overwrite
  else
    log_warn "Node '${vm_name}' is already absent; skipping deletion taint"
  fi

  _wait_for_node_not_ready_or_absent "${vm_name}"

  _validate_rp_delete_cleanup "${vm_ip}"

  log_info "Deleting any remaining node '${vm_name}' from cluster..."
  kubectl delete node "${vm_name}" --ignore-not-found --wait=false
}

_wait_for_node_not_ready_or_absent() {
  local vm_name="$1"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0

  log_info "Waiting for node '${vm_name}' to become NotReady or disappear (timeout: ${timeout}s)..."
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local ready
    ready="$(kubectl get node "${vm_name}" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)"
    if [[ -z "${ready}" ]]; then
      log_success "Node '${vm_name}' is absent"
      return 0
    fi
    if [[ "${ready}" != "True" ]]; then
      log_success "Node '${vm_name}' Ready condition is ${ready}"
      return 0
    fi

    sleep 5
    elapsed=$((elapsed + 5))
    log_debug "Waiting for ${vm_name} to become NotReady... (${elapsed}/${timeout}s)"
  done

  log_error "Node '${vm_name}' stayed Ready after ${timeout}s"
  kubectl get node "${vm_name}" -o wide 2>&1 || true
  kubectl describe node "${vm_name}" 2>&1 || true
  return 1
}

_validate_rp_delete_cleanup() {
  local vm_ip="$1"

  log_info "Validating RP delete cleanup on ${vm_ip}..."
  remote_exec "${vm_ip}" "E2E_NODE_JOIN_TIMEOUT=${E2E_NODE_JOIN_TIMEOUT} bash -s" <<'REMOTE'
set -euo pipefail

validate_no_wireguard_interfaces() {
  local interfaces
  local -a matches

  shopt -s nullglob
  matches=(/sys/class/net/wg*)
  shopt -u nullglob

  if ((${#matches[@]} == 0)); then
    return 0
  fi

  interfaces="$(printf '%s\n' "${matches[@]##*/}" | sort)"
  echo "WireGuard interfaces still exist after reset cleanup:"
  echo "${interfaces}"
  while IFS= read -r iface; do
    ip link show "${iface}" || true
  done <<<"${interfaces}"
  exit 1
}

validate_no_overlay_interfaces() {
  local iface

  for iface in geneve0 vxlan0 ipip0 unbounded0 cbr0; do
    if [[ -e "/sys/class/net/${iface}" ]]; then
      echo "reset cleanup interface ${iface} still exists"
      ip link show "${iface}" || true
      exit 1
    fi
  done
}

validate_no_wireguard_keys() {
  local path

  for path in /etc/wireguard/server.priv /etc/wireguard/server.pub; do
    if [[ -e "${path}" ]]; then
      echo "reset cleanup WireGuard key ${path} still exists"
      sudo ls -l "${path}" || true
      exit 1
    fi
  done
}

validate_no_policy_routing_state() {
  if ip rule show | grep -Eq 'lookup 51898|table 51898'; then
    echo "reset cleanup policy routing rule for table 51898 still exists"
    ip rule show
    exit 1
  fi

  if ip route show table 51898 | grep -q .; then
    echo "reset cleanup route table 51898 is not empty"
    ip route show table 51898
    exit 1
  fi
}

deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
while systemctl list-unit-files aks-flex-node-agent.service --no-legend | grep -q '^aks-flex-node-agent.service'; do
  if (( SECONDS >= deadline )); then
    echo "Timed out waiting for aks-flex-node-agent.service to be uninstalled"
    sudo systemctl status aks-flex-node-agent.service --no-pager -l || true
    sudo journalctl -u aks-flex-node-agent.service -n 50 --no-pager || true
    exit 1
  fi
  sleep 5
done

if [[ -e /run/aks-flex-node/e2e-machine.json ]]; then
  echo "local Machine file still exists after delete"
  exit 1
fi

for machine in kube1 kube2; do
  if machinectl show "${machine}" &>/dev/null; then
    echo "nspawn machine ${machine} still exists after delete"
    sudo machinectl status "${machine}" --no-pager || true
    exit 1
  fi
done

validate_no_wireguard_interfaces
validate_no_overlay_interfaces
validate_no_wireguard_keys
validate_no_policy_routing_state

for path in /etc/aks-flex-node /var/log/aks-flex-node; do
  if [[ -e "${path}" ]]; then
    echo "runtime path ${path} still exists after delete"
    sudo ls -la "${path}" || true
    exit 1
  fi
done

echo "RP delete cleanup validated"
REMOTE
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

  ensure_daemon_csr_approver

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

# ---------------------------------------------------------------------------
# node_unjoin_all - Unjoin all nodes in parallel
# ---------------------------------------------------------------------------
node_unjoin_all() {
  log_section "Unjoining All Nodes (parallel)"
  local start
  start=$(timer_start)

  local msi_pid token_pid kubeadm_pid
  local msi_exit=0 token_exit=0 kubeadm_exit=0

  node_unjoin_msi &
  msi_pid=$!

  node_unjoin_token &
  token_pid=$!

  node_unjoin_kubeadm &
  kubeadm_pid=$!

  wait "${msi_pid}" || msi_exit=$?
  wait "${token_pid}" || token_exit=$?
  wait "${kubeadm_pid}" || kubeadm_exit=$?

  local duration
  duration=$(timer_elapsed "${start}")

  if [[ "${msi_exit}" -ne 0 ]]; then
    log_error "MSI node unjoin failed (exit ${msi_exit})"
  fi
  if [[ "${token_exit}" -ne 0 ]]; then
    log_error "Token node unjoin failed (exit ${token_exit})"
  fi
  if [[ "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Kubeadm node unjoin failed (exit ${kubeadm_exit})"
  fi

  if [[ "${msi_exit}" -ne 0 || "${token_exit}" -ne 0 || "${kubeadm_exit}" -ne 0 ]]; then
    log_error "Node unjoins failed (${duration}s)"
    return 1
  fi

  log_success "All nodes unjoined in ${duration}s"
}
