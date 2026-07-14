#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/upgrade-drift.sh - Kubernetes version drift / repave E2E test
#
# Functions:
#   upgrade_drift_all - Trigger controller-machine-driven repave on all joined modes.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_UPGRADE_DRIFT_LOADED:-}" ]] && return 0
readonly _E2E_UPGRADE_DRIFT_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/controller.sh"

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

_remote_active_machine_snapshot() {
  local vm_ip="$1"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set +e
state_file="/etc/aks-flex-node/daemon-state.json"
machine="$(sudo sed -n 's/.*"activeMachine"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${state_file}" 2>/dev/null)"
applied_settings_version="$(sudo sed -n 's/.*"appliedSettingsVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${state_file}" 2>/dev/null)"
state=""
version=""
if [[ -n "${machine}" ]]; then
  state="$(machinectl show "${machine}" --property=State --value 2>/dev/null)"
  version="$(sudo systemd-run --machine="${machine}" --quiet --pipe /usr/local/bin/kubelet --version 2>/dev/null | awk '{print $2}' | sed 's/^v//')"
fi
printf '%s|%s|%s|%s\n' "${machine}" "${state}" "${version}" "${applied_settings_version}"
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

  log_info "Updating controller machine goal for ${mode} node to Kubernetes ${desired_version} (${settings_version})"
  machine_configmap_upsert "${vm_name}" "${desired_version}" "${settings_version}"
  remote_exec "${vm_ip}" 'sudo systemctl status aks-flex-node-agent.service --no-pager -l || true'

  log_info "Deleting Kubernetes Node ${vm_name} to trigger ${mode} repave"
  kubectl delete node "${vm_name}" --ignore-not-found --wait=false
}

_wait_for_mode_repave() {
  local mode="$1" desired_version="$2" settings_version="$3" old_active_machine="$4" old_node_uid="$5"
  local desired_major_minor
  desired_major_minor="$(_version_major_minor "${desired_version}")"

  local vm_ip vm_name timeout elapsed
  vm_ip="$(_mode_vm_ip "${mode}")"
  vm_name="$(_mode_vm_name "${mode}")"
  timeout="${E2E_DRIFT_UPGRADE_TIMEOUT:-900}"
  elapsed=0

  log_info "Waiting for ${mode} node repave to active side (timeout: ${timeout}s)..."
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local machine_snapshot active_machine state kubelet_version applied_settings_version kubelet_major_minor ready node_uid node_kubelet_version node_kubelet_major_minor
    machine_snapshot="$(_remote_active_machine_snapshot "${vm_ip}" || true)"
    IFS='|' read -r active_machine state kubelet_version applied_settings_version <<<"${machine_snapshot}"
    if [[ -n "${kubelet_version}" ]]; then
      kubelet_major_minor="$(_version_major_minor "${kubelet_version}")"
    else
      kubelet_major_minor=""
    fi

    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    node_uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    node_kubelet_version="$(kubectl get node "${vm_name}" -o jsonpath='{.status.nodeInfo.kubeletVersion}' 2>/dev/null | sed 's/^v//' || true)"
    if [[ -n "${node_kubelet_version}" ]]; then
      node_kubelet_major_minor="$(_version_major_minor "${node_kubelet_version}")"
    else
      node_kubelet_major_minor=""
    fi

    log_debug "${mode} repave poll: active_machine=${active_machine:-unknown} previous_machine=${old_active_machine} state=${state:-unknown} kubelet=${kubelet_version:-unknown} applied_settings=${applied_settings_version:-unknown} node_uid=${node_uid:-unknown} previous_node_uid=${old_node_uid} node_ready=${ready:-unknown} node_kubelet=${node_kubelet_version:-unknown}"
    if [[ -n "${active_machine}" && "${active_machine}" != "${old_active_machine}" && \
      "${state}" == "running" && "${kubelet_major_minor}" == "${desired_major_minor}" && \
      "${applied_settings_version}" == "${settings_version}" && -n "${node_uid}" && \
      "${node_uid}" != "${old_node_uid}" && "${ready}" == "True" && \
      "${node_kubelet_major_minor}" == "${desired_major_minor}" ]]; then
      log_success "Repaved ${mode} node to ${active_machine} with kubelet ${kubelet_version}; node reports kubelet ${node_kubelet_version} and applied settings ${applied_settings_version}"
      kubectl get nodes -o wide || true
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
  kubectl get nodes -o wide || true
  return 1
}

upgrade_drift_mode() {
  local mode="$1"
  log_section "Controller Machine Repave (${mode} node)"
  local start desired_version settings_version vm_name vm_ip old_machine_snapshot old_active_machine old_state old_version old_settings_version old_node_uid
  start="$(timer_start)"

  desired_version="$(_cluster_current_kubernetes_version)"
  settings_version="repave-${mode}-$(date +%s)"
  vm_name="$(_mode_vm_name "${mode}")"
  vm_ip="$(_mode_vm_ip "${mode}")"

  log_info "AKS desired Kubernetes version: ${desired_version}"
  _ensure_mode_joined "${mode}"
  validate_node_joined "${vm_name}"

  old_machine_snapshot="$(_remote_active_machine_snapshot "${vm_ip}")"
  IFS='|' read -r old_active_machine old_state old_version old_settings_version <<<"${old_machine_snapshot}"
  old_node_uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')"
  if [[ -z "${old_active_machine}" || -z "${old_node_uid}" ]]; then
    log_error "Failed to capture pre-repave state for ${mode}: active_machine=${old_active_machine:-unknown} node_uid=${old_node_uid:-unknown}"
    return 1
  fi
  log_debug "${mode} pre-repave state: active_machine=${old_active_machine} state=${old_state:-unknown} kubelet=${old_version:-unknown} applied_settings=${old_settings_version:-unknown} node_uid=${old_node_uid}"

  _trigger_mode_repave "${mode}" "${desired_version}" "${settings_version}"
  _wait_for_mode_repave "${mode}" "${desired_version}" "${settings_version}" "${old_active_machine}" "${old_node_uid}"
  smoke_test "${vm_name}" "${mode}-repave"

  log_success "${mode} controller machine repave passed in $(timer_elapsed "${start}")s"
}

upgrade_drift_all() {
  log_section "Controller Machine Repave (all modes)"
  upgrade_drift_mode msi
  upgrade_drift_mode token
  upgrade_drift_mode kubeadm
}

upgrade_drift_msi() { upgrade_drift_mode msi; }
