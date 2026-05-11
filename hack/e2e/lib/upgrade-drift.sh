#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/upgrade-drift.sh - Kubernetes version drift / repave E2E test
#
# Functions:
#   upgrade_drift_token - Join the token node with an older kubelet version and
#                         verify drift remediation repaves to the alternate side.
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

_wait_for_token_repave() {
  local desired_version="$1"
  local desired_major_minor
  desired_major_minor="$(_version_major_minor "${desired_version}")"

  local vm_ip vm_name timeout elapsed
  vm_ip="$(state_get token_vm_ip)"
  vm_name="$(state_get token_vm_name)"
  timeout="${E2E_DRIFT_UPGRADE_TIMEOUT:-900}"
  elapsed=0

  log_info "Waiting for token node drift remediation to repave to kube2 (timeout: ${timeout}s)..."
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local state_and_version state kubelet_version kubelet_major_minor ready
    state_and_version="$(_remote_kube2_state_and_version "${vm_ip}" || true)"
    state="${state_and_version%%|*}"
    kubelet_version="${state_and_version#*|}"
    if [[ "${kubelet_version}" != "${state_and_version}" && -n "${kubelet_version}" ]]; then
      kubelet_major_minor="$(_version_major_minor "${kubelet_version}")"
    else
      kubelet_major_minor=""
    fi

    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"

    log_debug "repave poll: kube2_state=${state:-unknown} kubelet=${kubelet_version:-unknown} node_ready=${ready:-unknown}"
    if [[ "${state}" == "running" && "${kubelet_major_minor}" == "${desired_major_minor}" && "${ready}" == "True" ]]; then
      log_success "Drift remediation repaved token node to kube2 with kubelet ${kubelet_version}"
      return 0
    fi

    sleep 15
    elapsed=$((elapsed + 15))
  done

  log_error "Timed out waiting for token node drift remediation"
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

upgrade_drift_token() {
  log_section "Kubernetes Version Drift Upgrade (Token Node)"
  local start desired_version initial_version original_version
  start="$(timer_start)"

  desired_version="$(_cluster_current_kubernetes_version)"
  initial_version="${E2E_DRIFT_INITIAL_KUBERNETES_VERSION:-$(_previous_minor_version "${desired_version}")}"
  original_version="${E2E_KUBERNETES_VERSION}"

  log_info "AKS desired Kubernetes version: ${desired_version}"
  log_info "Bootstrapping token node with older Kubernetes version: ${initial_version}"

  export E2E_KUBERNETES_VERSION="${initial_version}"
  node_join_token
  export E2E_KUBERNETES_VERSION="${original_version}"

  local token_vm_name
  token_vm_name="$(state_get token_vm_name)"
  validate_node_joined "${token_vm_name}"
  _wait_for_token_repave "${desired_version}"
  smoke_test "${token_vm_name}" "token-drift"

  log_success "Kubernetes version drift upgrade passed in $(timer_elapsed "${start}")s"
}
