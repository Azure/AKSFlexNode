#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/upgrade-drift.sh - Kubernetes version drift / repave E2E test
#
# Functions:
#   upgrade_drift_all - Trigger local-machine-driven repave on all joined modes.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_UPGRADE_DRIFT_LOADED:-}" ]] && return 0
readonly _E2E_UPGRADE_DRIFT_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

_version_major_minor() {
  local version="${1#v}"
  local major minor
  IFS='.' read -r major minor _ <<<"${version}"
  echo "${major}.${minor}"
}

_previous_minor_version() {
  local version="${1#v}"
  local major minor
  IFS='.' read -r major minor _ <<<"${version}"
  if [[ -z "${major}" || -z "${minor}" || "${minor}" -le 0 ]]; then
    log_error "Cannot derive previous minor version from ${version}"
    return 1
  fi
  echo "${major}.$((minor - 1)).0"
}

_cluster_current_kubernetes_version() {
  local cluster_name resource_group version
  cluster_name="$(state_get cluster_name)"
  resource_group="$(state_get resource_group)"

  version="$(az aks show \
    --resource-group "${resource_group}" \
    --name "${cluster_name}" \
    --query 'currentKubernetesVersion || kubernetesVersion' \
    -o tsv)"

  if [[ -z "${version}" ]]; then
    log_error "Failed to resolve AKS current Kubernetes version"
    return 1
  fi
  echo "${version#v}"
}

_remote_kube2_state_and_version() {
  local vm_ip="$1"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set +e
state="$(machinectl show kube2 --property=State --value 2>/dev/null)"
version="$(sudo systemd-run --machine=kube2 --quiet --pipe /usr/local/bin/kubelet --version 2>/dev/null | awk '{print $2}' | sed 's/^v//')"
printf '%s|%s\n' "${state}" "${version}"
REMOTE
}

_mode_vm_ip() {
  local mode="$1"
  state_get "${mode}_vm_ip"
}

_mode_vm_name() {
  local mode="$1"
  state_get "${mode}_vm_name"
}

_join_mode() {
  local mode="$1"
  case "${mode}" in
    msi) node_join_msi ;;
    token) node_join_token ;;
    kubeadm) node_join_kubeadm ;;
    *) log_error "unknown repave mode ${mode}" ;;
  esac
}

_ensure_mode_joined() {
  local mode="$1"
  local vm_name
  vm_name="$(_mode_vm_name "${mode}")"
  if kubectl get node "${vm_name}" &>/dev/null; then
    return 0
  fi
  log_info "Node ${vm_name} is not joined; joining ${mode} node before repave validation"
  _join_mode "${mode}"
  validate_node_joined "${vm_name}"
}

_trigger_mode_repave() {
  local mode="$1" desired_version="$2" settings_version="$3"
  local vm_ip vm_name
  vm_ip="$(_mode_vm_ip "${mode}")"
  vm_name="$(_mode_vm_name "${mode}")"

  log_info "Updating local machine goal on ${mode} node to Kubernetes ${desired_version} (${settings_version})"
  remote_exec "${vm_ip}" "DESIRED_VERSION=${desired_version} SETTINGS_VERSION=${settings_version} bash -s" <<'REMOTE'
set -euo pipefail
sudo mkdir -p /run/aks-flex-node
sudo tee /run/aks-flex-node/e2e-machine.json >/dev/null <<EOF
{
  "id": "local-test-machine",
  "goal": {
    "kubernetesVersion": "${DESIRED_VERSION}",
    "settingsVersion": "${SETTINGS_VERSION}"
  }
}
EOF
sudo systemctl status aks-flex-node-agent.service --no-pager -l || true
REMOTE

  log_info "Deleting Kubernetes Node ${vm_name} to trigger ${mode} repave"
  kubectl delete node "${vm_name}" --ignore-not-found --wait=false
}

_wait_for_mode_repave() {
  local mode="$1" desired_version="$2"
  local desired_major_minor
  desired_major_minor="$(_version_major_minor "${desired_version}")"

  local vm_ip vm_name timeout elapsed
  vm_ip="$(_mode_vm_ip "${mode}")"
  vm_name="$(_mode_vm_name "${mode}")"
  timeout="${E2E_DRIFT_UPGRADE_TIMEOUT:-900}"
  elapsed=0

  log_info "Waiting for ${mode} node repave to kube2 (timeout: ${timeout}s)..."
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local state_and_version state kubelet_version kubelet_major_minor ready node_kubelet_version node_kubelet_major_minor
    state_and_version="$(_remote_kube2_state_and_version "${vm_ip}" || true)"
    state="${state_and_version%%|*}"
    kubelet_version="${state_and_version#*|}"
    if [[ "${kubelet_version}" != "${state_and_version}" && -n "${kubelet_version}" ]]; then
      kubelet_major_minor="$(_version_major_minor "${kubelet_version}")"
    else
      kubelet_major_minor=""
    fi

    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    node_kubelet_version="$(kubectl get node "${vm_name}" -o jsonpath='{.status.nodeInfo.kubeletVersion}' 2>/dev/null | sed 's/^v//' || true)"
    if [[ -n "${node_kubelet_version}" ]]; then
      node_kubelet_major_minor="$(_version_major_minor "${node_kubelet_version}")"
    else
      node_kubelet_major_minor=""
    fi

    log_debug "${mode} repave poll: kube2_state=${state:-unknown} kube2_kubelet=${kubelet_version:-unknown} node_ready=${ready:-unknown} node_kubelet=${node_kubelet_version:-unknown}"
    if [[ "${state}" == "running" && "${kubelet_major_minor}" == "${desired_major_minor}" && "${ready}" == "True" && "${node_kubelet_major_minor}" == "${desired_major_minor}" ]]; then
      log_success "Repaved ${mode} node to kube2 with kubelet ${kubelet_version}; node reports kubelet ${node_kubelet_version}"
      return 0
    fi

    sleep 15
    elapsed=$((elapsed + 15))
  done

  log_error "Timed out waiting for ${mode} node repave"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE' || true
set +e
echo "=== nspawn machines ==="
machinectl list --no-pager
for machine in kube1 kube2; do
  echo "=== ${machine} status ==="
  machinectl status "${machine}" --no-pager || true
  echo "=== ${machine} kubelet version ==="
  sudo systemd-run --machine="${machine}" --quiet --pipe /usr/local/bin/kubelet --version || true
done
echo "=== aks-flex-node-agent logs ==="
sudo journalctl -u aks-flex-node-agent.service -n 100 --no-pager || true
REMOTE
  kubectl get node "${vm_name}" -o wide || true
  return 1
}

upgrade_drift_mode() {
  local mode="$1"
  log_section "Local Machine Repave (${mode} node)"
  local start desired_version settings_version vm_name
  start="$(timer_start)"

  desired_version="$(_cluster_current_kubernetes_version)"
  settings_version="repave-${mode}-$(date +%s)"
  vm_name="$(_mode_vm_name "${mode}")"

  log_info "AKS desired Kubernetes version: ${desired_version}"
  _ensure_mode_joined "${mode}"
  validate_node_joined "${vm_name}"
  _trigger_mode_repave "${mode}" "${desired_version}" "${settings_version}"
  _wait_for_mode_repave "${mode}" "${desired_version}"
  smoke_test "${vm_name}" "${mode}-repave"

  log_success "${mode} local machine repave passed in $(timer_elapsed "${start}")s"
}

upgrade_drift_all() {
  log_section "Local Machine Repave (all modes)"
  upgrade_drift_mode msi
  upgrade_drift_mode token
  upgrade_drift_mode kubeadm
}

upgrade_drift_msi() { upgrade_drift_mode msi; }
