#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/cleanup.sh - Resource cleanup and log collection
#
# Functions:
#   collect_logs  - SSH into VMs and download service/agent/kubelet logs
#   cleanup       - Delete all Azure resources created during the test
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_CLEANUP_LOADED:-}" ]] && return 0
readonly _E2E_CLEANUP_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# _collect_vm_logs - Collect logs from a single VM
# ---------------------------------------------------------------------------
_collect_vm_logs() {
  local vm_ip="$1"
  local prefix="$2"

  log_info "Collecting logs from ${vm_ip} (${prefix})..."

  remote_exec "${vm_ip}" \
    "sudo cat /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-aks-flex-node.log" 2>/dev/null || true

  remote_exec "${vm_ip}" \
    "sudo journalctl -u 'aks-flex-node-*' -n 500 --no-pager 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-agent-journal.log" 2>/dev/null || true

  remote_exec "${vm_ip}" \
    "sudo cat /tmp/aks-flex-node-preflight.log 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-preflight.log" 2>/dev/null || true

  # Kubelet and containerd run inside the nspawn machine (kube1 or kube2).
  # Use nsenter via the machine leader PID to run journalctl inside the container.
  # Fall back to host journal for non-nspawn setups.
  remote_exec "${vm_ip}" \
    "leader=\$(sudo machinectl show kube1 -p Leader --value 2>/dev/null); \
     if [ -n \"\$leader\" ]; then \
       sudo nsenter -t \$leader -m -p -- journalctl -u kubelet -n 500 --no-pager 2>/dev/null; \
     else \
       sudo journalctl -u kubelet -n 500 --no-pager 2>/dev/null; \
     fi" \
    > "${E2E_LOG_DIR}/${prefix}-kubelet.log" 2>/dev/null || true

  remote_exec "${vm_ip}" \
    "leader=\$(sudo machinectl show kube1 -p Leader --value 2>/dev/null); \
     if [ -n \"\$leader\" ]; then \
       sudo nsenter -t \$leader -m -p -- journalctl -u containerd -n 200 --no-pager 2>/dev/null; \
     else \
       sudo journalctl -u containerd -n 200 --no-pager 2>/dev/null; \
     fi" \
    > "${E2E_LOG_DIR}/${prefix}-containerd.log" 2>/dev/null || true

  remote_exec "${vm_ip}" "bash -s" <<'REMOTE' > "${E2E_LOG_DIR}/${prefix}-npd.log" 2>&1 || true
npd_service="node-problem-detector.service"
active_machine="$(sudo python3 - <<'PY'
import json
import sys

try:
    with open("/etc/aks-flex-node/daemon-state.json", encoding="utf-8") as state:
        active_machine = json.load(state).get("activeMachine", "")
    if active_machine:
        print(active_machine)
    else:
        print("daemon state does not include activeMachine", file=sys.stderr)
except FileNotFoundError as exc:
    print(f"daemon state not found: {exc}", file=sys.stderr)
except json.JSONDecodeError as exc:
    print(f"daemon state is not valid JSON: {exc}", file=sys.stderr)
except PermissionError as exc:
    print(f"daemon state permission denied: {exc}", file=sys.stderr)
PY
)"
if [ -n "${active_machine}" ]; then
  echo "=== ${npd_service} logs (${active_machine}) ==="
  # Match the agent and kubelet log depth; NPD entries are sparse but useful across node lifecycle phases.
  sudo systemd-run --machine="${active_machine}" --quiet --pipe journalctl -u "${npd_service}" -n 500 --no-pager || \
    echo "warning: failed to collect ${npd_service} logs from ${active_machine}"
else
  echo "warning: active machine unknown; falling back to host journal"
  sudo journalctl -u "${npd_service}" -n 500 --no-pager || \
    echo "warning: failed to collect ${npd_service} logs from host"
fi
REMOTE

  # Collect CNI config and nspawn machine state for networking diagnostics.
  # Read directly from the nspawn rootfs at /var/lib/machines/kube1/.
  local kube1_root="/var/lib/machines/kube1"
  remote_exec "${vm_ip}" \
    "echo '=== nspawn machines ===' && sudo machinectl list --no-pager 2>/dev/null; \
     echo '=== CNI config (kube1) ===' && sudo ls -la ${kube1_root}/etc/cni/net.d/ 2>/dev/null; \
     for f in ${kube1_root}/etc/cni/net.d/*.conflist ${kube1_root}/etc/cni/net.d/*.conf; do \
       [ -f \"\$f\" ] && echo \"--- \$f ---\" && sudo cat \"\$f\"; \
     done; \
     echo '=== CNI binaries (kube1) ===' && sudo ls -la ${kube1_root}/opt/cni/bin/ 2>/dev/null; \
     echo '=== containerd config ===' && sudo cat ${kube1_root}/etc/containerd/config.toml 2>/dev/null" \
    > "${E2E_LOG_DIR}/${prefix}-nspawn-diagnostics.log" 2>/dev/null || true

  log_info "Logs saved to ${E2E_LOG_DIR}/${prefix}-*.log"
}

# ---------------------------------------------------------------------------
# collect_logs - Collect logs from all VMs
# ---------------------------------------------------------------------------
collect_logs() {
  log_section "Collecting Logs"

  mkdir -p "${E2E_LOG_DIR}"

  local msi_vm_ip token_vm_ip offline_vm_ip kubeadm_vm_ip arc_vm_ip
  msi_vm_ip="$(state_get msi_vm_ip)"
  token_vm_ip="$(state_get token_vm_ip)"
  offline_vm_ip="$(state_get offline_vm_ip)"
  kubeadm_vm_ip="$(state_get kubeadm_vm_ip)"
  arc_vm_ip="$(state_get arc_vm_ip)"

  if [[ -n "${msi_vm_ip}" ]]; then
    _collect_vm_logs "${msi_vm_ip}" "msi" || true
  fi

  if [[ -n "${token_vm_ip}" ]]; then
    _collect_vm_logs "${token_vm_ip}" "token" || true
  fi

  if [[ -n "${offline_vm_ip}" ]]; then
    _collect_vm_logs "${offline_vm_ip}" "offline" || true
  fi

  if [[ -n "${kubeadm_vm_ip}" ]]; then
    _collect_vm_logs "${kubeadm_vm_ip}" "kubeadm" || true
  fi

  if [[ -n "${arc_vm_ip}" ]]; then
    _collect_vm_logs "${arc_vm_ip}" "arc" || true
  fi

  # Also capture cluster-side info
  {
    echo "=== Nodes ==="
    kubectl get nodes -o wide 2>/dev/null || true
    echo ""
    echo "=== Controller Pods ==="
    kubectl -n kube-system get pods -l app.kubernetes.io/name=aks-flex-controller -o wide 2>/dev/null || true
    echo ""
    echo "=== Machine ConfigMap Keys ==="
    kubectl -n kube-system get configmap aks-flex-machines -o json 2>/dev/null | jq -r '.data | keys[]?' || true
    echo ""
    echo "=== Unbounded-Net Pods ==="
    kubectl -n unbounded-net get pods -o wide 2>/dev/null || true
    echo ""
    echo "=== Unbounded-Net Resources ==="
    kubectl get sites,sitenodeslices,sitepeerings -o wide 2>/dev/null || true
    echo ""
    echo "=== CSRs ==="
    kubectl get csr 2>/dev/null || true
    echo ""
    echo "=== Events (last 50) ==="
    kubectl get events --sort-by='.lastTimestamp' -A 2>/dev/null | tail -50 || true
  } > "${E2E_LOG_DIR}/cluster-info.log" 2>&1

  kubectl -n kube-system logs "deployment/aks-flex-controller" --tail=500 \
    > "${E2E_LOG_DIR}/aks-flex-controller.log" 2>&1 || true
  kubectl -n unbounded-net logs "deployment/unbounded-net-controller" --tail=500 \
    > "${E2E_LOG_DIR}/unbounded-net-controller.log" 2>&1 || true
  kubectl -n unbounded-net logs -l app.kubernetes.io/name=unbounded-net-node --tail=500 --all-containers \
    > "${E2E_LOG_DIR}/unbounded-net-node.log" 2>&1 || true

  log_success "Logs collected in ${E2E_LOG_DIR}/"
  ls -la "${E2E_LOG_DIR}/"
}

# ---------------------------------------------------------------------------
# cleanup - Delete Azure resources
# ---------------------------------------------------------------------------
_deployment_state() {
  local resource_group="$1" deployment_name="$2" subscription_id="$3"
  local deployments

  deployments="$(az deployment group list \
    --resource-group "${resource_group}" \
    --subscription "${subscription_id}" \
    --output json 2>/dev/null)" || return 1
  jq -er --arg name "${deployment_name}" '
    if type != "array" then error("deployment list must be an array")
    else ([.[] | select(.name == $name)][0].properties.provisioningState // "")
    end
  ' <<<"${deployments}"
}

_wait_for_deployment_operations() {
  local resource_group="$1" deployment_name="$2" subscription_id="$3" deadline="$4"
  local operations

  while (( SECONDS < deadline )); do
    if ! operations="$(az deployment operation group list \
      --resource-group "${resource_group}" \
      --name "${deployment_name}" \
      --subscription "${subscription_id}" \
      --output json 2>/dev/null)"; then
      log_error "Failed to query operations for ARM deployment '${deployment_name}'"
      return 1
    fi
    if jq -e '
      type == "array" and all(.[].properties.provisioningState;
        . == "Succeeded" or . == "Failed" or . == "Canceled" or
        . == "Cancelled" or . == "Skipped")
    ' <<<"${operations}" >/dev/null; then
      return 0
    fi
    sleep "${E2E_CLEANUP_POLL_INTERVAL:-5}"
  done

  log_error "ARM deployment '${deployment_name}' still has active operations after the cleanup timeout"
  return 1
}

_cancel_active_deployment() {
  local resource_group="$1" deployment_name="$2" subscription_id="$3"
  local deadline="${4:-$((SECONDS + E2E_CLEANUP_TIMEOUT))}"
  local deployment_state

  [[ -n "${deployment_name}" ]] || return 0
  if ! deployment_state="$(_deployment_state \
    "${resource_group}" "${deployment_name}" "${subscription_id}")"; then
    log_error "Failed to query ARM deployment '${deployment_name}'; refusing to race resource deletion"
    return 1
  fi

  case "${deployment_state}" in
    ""|Succeeded|Failed|Canceled|Cancelled)
      [[ -z "${deployment_state}" ]] || _wait_for_deployment_operations \
        "${resource_group}" "${deployment_name}" "${subscription_id}" "${deadline}"
      return
      ;;
  esac

  log_warn "ARM deployment '${deployment_name}' is ${deployment_state}; canceling it before resource deletion"
  az deployment group cancel \
    --resource-group "${resource_group}" \
    --name "${deployment_name}" \
    --subscription "${subscription_id}" \
    --output none 2>/dev/null || true

  while (( SECONDS < deadline )); do
    if ! deployment_state="$(_deployment_state \
      "${resource_group}" "${deployment_name}" "${subscription_id}")"; then
      log_error "Failed to query ARM deployment '${deployment_name}' after requesting cancellation"
      return 1
    fi
    case "${deployment_state}" in
      ""|Succeeded|Failed|Canceled|Cancelled)
        log_info "ARM deployment is terminal: ${deployment_state:-not found}"
        [[ -z "${deployment_state}" ]] || _wait_for_deployment_operations \
          "${resource_group}" "${deployment_name}" "${subscription_id}" "${deadline}"
        return
        ;;
    esac
    sleep "${E2E_CLEANUP_POLL_INTERVAL:-5}"
  done

  log_error "ARM deployment '${deployment_name}' remained ${deployment_state} after ${E2E_CLEANUP_TIMEOUT}s"
  return 1
}

_remaining_cleanup_timeout() {
  local deadline="$1"
  local remaining=$((deadline - SECONDS))
  (( remaining > 0 )) || return 1
  printf '%s\n' "${remaining}"
}

_group_exists_with_retry() {
  local group_name="$1" subscription_id="$2"
  local exists attempt

  for attempt in 1 2 3 4 5; do
    if exists="$(az group exists \
      --name "${group_name}" \
      --subscription "${subscription_id}" \
      --output tsv 2>/dev/null)"; then
      case "${exists}" in
        true|false)
          printf '%s\n' "${exists}"
          return 0
          ;;
      esac
    fi
    if (( attempt < 5 )); then
      sleep "${E2E_CLEANUP_POLL_INTERVAL:-5}"
    fi
  done

  log_error "Failed to determine whether resource group '${group_name}' exists after ${attempt} attempts" >&2
  return 1
}

_resource_inventory() {
  local resource_group="$1" subscription_id="$2" run_id="${3:-}"
  local resource_json attempt
  local -a tag_args=()
  [[ -z "${run_id}" ]] || tag_args=(--tag "github-run=${run_id}")

  for attempt in 1 2 3 4 5; do
    if resource_json="$(az resource list \
      --resource-group "${resource_group}" \
      --subscription "${subscription_id}" \
      "${tag_args[@]}" \
      --output json 2>/dev/null)" && \
      jq -e 'type == "array"' <<<"${resource_json}" >/dev/null; then
      printf '%s\n' "${resource_json}"
      return 0
    fi
    if (( attempt < 5 )); then
      sleep "${E2E_CLEANUP_POLL_INTERVAL:-5}"
    fi
  done

  log_error "Failed to inventory resources in '${resource_group}' after ${attempt} attempts" >&2
  return 1
}

_cleanup_resource_owner() {
  local name_suffix="${1:-}" owner
  if [[ -z "${name_suffix}" ]]; then
    name_suffix="$(state_get name_suffix)"
  fi
  owner="$(state_get resource_owner)"
  if [[ -n "${owner}" ]]; then
    if [[ -z "${name_suffix}" || "${owner}" != "${name_suffix}" ]]; then
      log_error "Refusing tagged cleanup with resource owner '${owner}' that does not match E2E name suffix '${name_suffix}'"
      return 1
    fi
    printf '%s\n' "${owner}"
    return 0
  fi

  # State written before resource_owner was introduced used run_id as the
  # github-run tag value. Keep those deployments recoverable.
  state_get run_id "${GITHUB_RUN_ID:-}"
}

_is_expected_legacy_tagged_resource() {
  local resource_type="${1,,}" resource_name="$2" name_suffix="$3"
  local role vm_prefix

  [[ -n "${resource_name}" && -n "${name_suffix}" ]] || return 1
  case "${resource_type}" in
    microsoft.compute/disks)
      for role in msi token offline kubeadm arc; do
        vm_prefix="vm-e2e-${role}-${name_suffix}"
        if [[ "${resource_name}" == "${vm_prefix}-osdisk" || \
              "${resource_name}" == "${vm_prefix}"_OsDisk_* ]]; then
          return 0
        fi
      done
      ;;
    microsoft.network/networkinterfaces)
      for role in msi token offline kubeadm arc; do
        [[ "${resource_name}" == "vm-e2e-${role}-${name_suffix}-nic" ]] && return 0
      done
      ;;
    microsoft.network/publicipaddresses)
      for role in msi token offline kubeadm arc; do
        [[ "${resource_name}" == "vm-e2e-${role}-${name_suffix}-pip" ]] && return 0
      done
      ;;
    microsoft.network/virtualnetworks)
      [[ "${resource_name}" == "vnet-e2e-${name_suffix}" ]] && return 0
      ;;
    microsoft.network/networksecuritygroups)
      [[ "${resource_name}" == "nsg-e2e-${name_suffix}" ]] && return 0
      ;;
  esac
  return 1
}

_legacy_implicit_os_disk_ids_from_inventory() {
  local resource_json="$1"
  shift
  local resource_rows resource_type resource_name id vm_name

  if ! resource_rows="$(jq -r '
    if type != "array" then error("resource list must be an array")
    else .[] | [(.type // ""), (.name // ""), (.id // "")] |
      if all(.[]; type == "string") then @tsv
      else error("resource type, name, and ID must be strings")
      end
    end
  ' <<<"${resource_json}")"; then
    log_error "Azure returned an invalid resource inventory while locating legacy OS disks"
    return 1
  fi

  while IFS=$'\t' read -r resource_type resource_name id; do
    [[ "${resource_type,,}" == "microsoft.compute/disks" && -n "${id}" ]] || continue
    for vm_name in "$@"; do
      [[ -n "${vm_name}" ]] || continue
      if [[ "${resource_name}" == "${vm_name}"_OsDisk_* ]]; then
        printf '%s\n' "${id}"
        break
      fi
    done
  done <<<"${resource_rows}"
}

_legacy_implicit_os_disk_ids() {
  local resource_group="$1" subscription_id="$2" resource_json
  shift 2

  (( $# > 0 )) || return 0
  if ! resource_json="$(_resource_inventory "${resource_group}" "${subscription_id}")"; then
    return 1
  fi
  _legacy_implicit_os_disk_ids_from_inventory "${resource_json}" "$@"
}

_delete_legacy_implicit_os_disks() {
  local resource_group="$1" subscription_id="$2" disk_ids disk_id
  shift 2

  if ! disk_ids="$(_legacy_implicit_os_disk_ids "${resource_group}" "${subscription_id}" "$@")"; then
    return 1
  fi
  while IFS= read -r disk_id; do
    [[ -n "${disk_id}" ]] || continue
    log_info "Deleting residual legacy OS disk: ${disk_id##*/}"
    az resource delete --ids "${disk_id}" --subscription "${subscription_id}" --output none 2>/dev/null || true
  done <<<"${disk_ids}"
}

_validate_expected_cleanup_name() {
  local description="$1" actual="$2" expected="$3"

  [[ -n "${actual}" ]] || return 0
  if [[ "${actual}" != "${expected}" ]]; then
    log_error "Refusing to delete unexpected ${description} '${actual}'"
    log_error "Expected '${expected}' for this E2E deployment"
    return 1
  fi
}

_validate_cleanup_target_names() {
  local name_suffix="$1" deployment_name="$2" cluster_name="$3" node_resource_group="$4"
  local msi_vm_name="$5" token_vm_name="$6" offline_vm_name="$7" kubeadm_vm_name="$8"
  local arc_vm_name="$9" arc_machine_name="${10}" vnet_name="${11}" nsg_name="${12}"
  local resource_group="${13}" location="${14}"

  if [[ -z "${name_suffix}" ]]; then
    if [[ -n "${deployment_name}${cluster_name}${node_resource_group}${msi_vm_name}${token_vm_name}${offline_vm_name}${kubeadm_vm_name}${arc_vm_name}${arc_machine_name}${vnet_name}${nsg_name}" ]]; then
      log_error "Cannot validate persisted cleanup targets without an E2E name suffix"
      return 1
    fi
    return 0
  fi

  _validate_expected_cleanup_name "ARM deployment" "${deployment_name}" "e2e-${name_suffix}" || return 1
  _validate_expected_cleanup_name "AKS cluster" "${cluster_name}" "aks-e2e-${name_suffix}" || return 1
  _validate_expected_node_resource_group \
    "${node_resource_group}" "${name_suffix}" "${resource_group}" "${cluster_name}" "${location}" || return 1
  _validate_expected_cleanup_name "MSI VM" "${msi_vm_name}" "vm-e2e-msi-${name_suffix}" || return 1
  _validate_expected_cleanup_name "token VM" "${token_vm_name}" "vm-e2e-token-${name_suffix}" || return 1
  _validate_expected_cleanup_name "offline VM" "${offline_vm_name}" "vm-e2e-offline-${name_suffix}" || return 1
  _validate_expected_cleanup_name "kubeadm VM" "${kubeadm_vm_name}" "vm-e2e-kubeadm-${name_suffix}" || return 1
  _validate_expected_cleanup_name "Arc VM" "${arc_vm_name}" "vm-e2e-arc-${name_suffix}" || return 1
  _validate_expected_cleanup_name "Arc machine" "${arc_machine_name}" "vm-e2e-arc-${name_suffix}-connected" || return 1
  _validate_expected_cleanup_name "virtual network" "${vnet_name}" "vnet-e2e-${name_suffix}" || return 1
  _validate_expected_cleanup_name "network security group" "${nsg_name}" "nsg-e2e-${name_suffix}" || return 1
}

_validate_expected_node_resource_group() {
  local node_resource_group="$1" name_suffix="$2"
  local resource_group="${3:-}" cluster_name="${4:-}" location="${5:-}"

  [[ -n "${node_resource_group}" ]] || return 0
  if [[ -z "${name_suffix}" ]]; then
    log_error "Cannot validate AKS node resource group '${node_resource_group}' without an E2E name suffix"
    return 1
  fi

  local expected_node_resource_group="MC_aksflex-e2e-${name_suffix}"
  if [[ "${node_resource_group}" == "${expected_node_resource_group}" ]]; then
    return 0
  fi

  # E2E state written before the template assigned a deterministic node
  # resource group uses AKS's documented default naming convention.
  local legacy_node_resource_group=""
  if [[ -n "${resource_group}" && -n "${cluster_name}" && -n "${location}" ]]; then
    legacy_node_resource_group="MC_${resource_group}_${cluster_name}_${location}"
    if [[ "${node_resource_group}" == "${legacy_node_resource_group}" ]]; then
      return 0
    fi
  fi

  log_error "Refusing to delete unexpected AKS node resource group '${node_resource_group}'"
  if [[ -n "${legacy_node_resource_group}" ]]; then
    log_error "Expected '${expected_node_resource_group}' or legacy '${legacy_node_resource_group}' for this E2E deployment"
  else
    log_error "Expected '${expected_node_resource_group}' for this E2E deployment"
  fi
  return 1
}

_delete_node_resource_group() {
  local node_resource_group="$1" subscription_id="$2" deadline="$3"
  local exists wait_timeout

  [[ -n "${node_resource_group}" ]] || return 0
  if ! exists="$(_group_exists_with_retry "${node_resource_group}" "${subscription_id}")"; then
    log_error "Failed to determine whether AKS node resource group '${node_resource_group}' exists"
    return 1
  fi
  case "${exists}" in
    false)
      return 0
      ;;
    true)
      ;;
    *)
      log_error "Unexpected existence result for AKS node resource group '${node_resource_group}': ${exists}"
      return 1
      ;;
  esac

  az group delete --name "${node_resource_group}" --subscription "${subscription_id}" \
    --yes --no-wait --output none 2>/dev/null || true
  if ! wait_timeout="$(_remaining_cleanup_timeout "${deadline}")"; then
    log_error "Cleanup deadline reached before deleting AKS node resource group '${node_resource_group}'"
    return 1
  fi
  if ! az group wait --name "${node_resource_group}" --subscription "${subscription_id}" \
    --deleted --interval 10 --timeout "${wait_timeout}" 2>/dev/null; then
    log_error "AKS node resource group still exists after cleanup timeout: ${node_resource_group}"
    return 1
  fi
}

_delete_tagged_resources() {
  local resource_group="$1" run_id="$2" subscription_id="$3" legacy_name_suffix="${4:-}"
  local resource_json resource_type resource_name id
  local -a resource_types=(
    "Microsoft.Compute/disks"
    "Microsoft.Network/networkInterfaces"
    "Microsoft.Network/publicIPAddresses"
    "Microsoft.Network/virtualNetworks"
    "Microsoft.Network/networkSecurityGroups"
  )

  [[ -n "${run_id}" ]] || return 0
  if ! resource_json="$(_resource_inventory "${resource_group}" "${subscription_id}" "${run_id}")"; then
    log_error "Failed to list E2E resources tagged github-run=${run_id}"
    return 1
  fi

  for resource_type in "${resource_types[@]}"; do
    while IFS=$'\t' read -r resource_name id; do
      [[ -n "${id}" ]] || continue
      if [[ -n "${legacy_name_suffix}" ]] && \
        ! _is_expected_legacy_tagged_resource "${resource_type}" "${resource_name}" "${legacy_name_suffix}"; then
        log_warn "Ignoring legacy-tagged ${resource_type} outside this E2E attempt: ${resource_name}"
        continue
      fi
      log_info "Deleting residual ${resource_type}: ${id##*/}"
      az resource delete --ids "${id}" --subscription "${subscription_id}" --output none 2>/dev/null || true
    done < <(jq -r --arg resource_type "${resource_type}" \
      '.[] | select((.type | ascii_downcase) == ($resource_type | ascii_downcase)) | [.name, .id] | @tsv' \
      <<<"${resource_json}")
  done
}

_tagged_resource_ids() {
  local resource_group="$1" run_id="$2" subscription_id="$3" legacy_name_suffix="${4:-}"
  local resource_json resource_type resource_name id

  [[ -n "${run_id}" ]] || return 0
  resource_json="$(_resource_inventory "${resource_group}" "${subscription_id}" "${run_id}")" || return 1
  while IFS=$'\t' read -r resource_type resource_name id; do
    [[ -n "${id}" ]] || continue
    if [[ -n "${legacy_name_suffix}" ]] && \
      ! _is_expected_legacy_tagged_resource "${resource_type}" "${resource_name}" "${legacy_name_suffix}"; then
      continue
    fi
    printf '%s\n' "${id}"
  done < <(jq -r '.[] | [.type, .name, .id] | @tsv' <<<"${resource_json}")
}

cleanup() {
  log_section "Cleaning Up Resources"

  if [[ "${E2E_SKIP_CLEANUP}" == "1" ]]; then
    log_warn "Cleanup skipped (E2E_SKIP_CLEANUP=1)"
    log_info "Resources left for debugging:"
    state_dump
    return 0
  fi

  local resource_group cluster_name msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name arc_vm_name arc_machine_name arc_machine_id
  local subscription_id deployment_name resource_owner persisted_resource_owner legacy_tag_name_suffix
  local cleanup_failed vnet_name nsg_name name_suffix node_resource_group location cleanup_deadline
  resource_group="$(state_get resource_group)"
  cluster_name="$(state_get cluster_name)"
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"
  arc_vm_name="$(state_get arc_vm_name)"
  arc_machine_name="$(state_get arc_machine_name)"
  subscription_id="$(state_get subscription_id "${AZURE_SUBSCRIPTION_ID:-}")"
  arc_machine_id="$(state_get arc_machine_id)"
  deployment_name="$(state_get deployment_name)"
  persisted_resource_owner="$(state_get resource_owner)"
  name_suffix="$(state_get name_suffix)"
  vnet_name="$(state_get vnet_name)"
  nsg_name="$(state_get nsg_name)"
  node_resource_group="$(state_get node_resource_group)"
  location="$(state_get location)"
  if [[ -z "${name_suffix}" && "${cluster_name}" == aks-e2e-* ]]; then
    name_suffix="${cluster_name#aks-e2e-}"
  fi
  if [[ -z "${deployment_name}" && -n "${name_suffix}" ]]; then
    deployment_name="e2e-${name_suffix}"
  fi
  if [[ -z "${vnet_name}" && -n "${name_suffix}" ]]; then
    vnet_name="vnet-e2e-${name_suffix}"
  fi
  if [[ -z "${nsg_name}" && -n "${name_suffix}" ]]; then
    nsg_name="nsg-e2e-${name_suffix}"
  fi
  if [[ -z "${node_resource_group}" && -z "${persisted_resource_owner}" && \
        -n "${resource_group}" && -n "${cluster_name}" && -n "${location}" ]]; then
    node_resource_group="MC_${resource_group}_${cluster_name}_${location}"
  fi
  if ! resource_owner="$(_cleanup_resource_owner "${name_suffix}")"; then
    return 1
  fi
  legacy_tag_name_suffix=""
  if [[ -z "${persisted_resource_owner}" && -n "${resource_owner}" ]]; then
    if [[ -z "${name_suffix}" ]]; then
      log_error "Cannot safely clean resources with a legacy run tag without an E2E name suffix"
      return 1
    fi
    legacy_tag_name_suffix="${name_suffix}"
  fi
  cleanup_failed=0
  cleanup_deadline=$((SECONDS + E2E_CLEANUP_TIMEOUT))

  if [[ -z "${resource_group}" ]]; then
    if [[ ! -f "${E2E_STATE_FILE}" ]] || jq -e 'length == 0' "${E2E_STATE_FILE}" >/dev/null 2>&1; then
      log_warn "No resource group in state; nothing to clean up"
      return 0
    fi
    log_error "State is nonempty but has no resource_group; refusing to declare cleanup complete"
    return 1
  fi

  local resource_group_exists
  if ! resource_group_exists="$(_group_exists_with_retry "${resource_group}" "${subscription_id}")"; then
    log_error "Failed to determine whether resource group '${resource_group}' exists"
    return 1
  fi

  if ! _validate_cleanup_target_names \
    "${name_suffix}" "${deployment_name}" "${cluster_name}" "${node_resource_group}" \
    "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" "${kubeadm_vm_name}" \
    "${arc_vm_name}" "${arc_machine_name}" "${vnet_name}" "${nsg_name}" \
    "${resource_group}" "${location}"; then
    return 1
  fi

  if [[ -n "${arc_machine_name}" ]]; then
    local expected_arc_machine_id="/subscriptions/${subscription_id}/resourceGroups/${resource_group}/providers/Microsoft.HybridCompute/machines/${arc_machine_name}"
    if [[ -n "${arc_machine_id}" && "${arc_machine_id,,}" != "${expected_arc_machine_id,,}" ]]; then
      log_error "Refusing to delete Arc machine with an unexpected persisted resource ID: ${arc_machine_id}"
      return 1
    fi
    # The ID is completely determined by other state fields. Reconstructing it
    # avoids trusting a redundant deletion target from stale or damaged state.
    arc_machine_id="${expected_arc_machine_id}"
  elif [[ -n "${arc_machine_id}" ]]; then
    log_error "Cannot validate persisted Arc machine resource ID without arc_machine_name"
    return 1
  fi

  case "${resource_group_exists}" in
    false)
      if ! _validate_expected_node_resource_group \
        "${node_resource_group}" "${name_suffix}" "${resource_group}" "${cluster_name}" "${location}"; then
        return 1
      fi
      if ! _delete_node_resource_group "${node_resource_group}" "${subscription_id}" "${cleanup_deadline}"; then
        return 1
      fi
      state_set "lifecycle" "cleaned" || return 1
      state_set "cleanup_complete" "true" || return 1
      log_success "Resource group is absent; cleanup is complete"
      return 0
      ;;
    true)
      ;;
    *)
      log_error "Unexpected existence result for resource group '${resource_group}': ${resource_group_exists}"
      return 1
      ;;
  esac

  state_set "cleanup_complete" "false" || return 1
  state_set "lifecycle" "cleaning" || return 1

  if ! _cancel_active_deployment \
    "${resource_group}" "${deployment_name}" "${subscription_id}" "${cleanup_deadline}"; then
    # Deleting while ARM is still provisioning races new resource creation and
    # can report false success, so preserve state for a later cleanup attempt.
    state_set "cleanup_complete" "false" || true
    return 1
  fi

  # Snapshot IDs that are difficult to recover after deleting their parents.
  # This supports cleanup of deployments created before deterministic OS-disk
  # names and delete options were added to the Bicep module.
  local vm_inventory aks_inventory resource_inventory live_node_resource_group live_cluster_location
  local vm_name disk_id nic_id nic_output legacy_disk_output
  local -a managed_disk_ids=() legacy_implicit_os_disk_ids=() nic_ids=()
  if ! vm_inventory="$(az vm list \
    --resource-group "${resource_group}" \
    --subscription "${subscription_id}" \
    --output json 2>/dev/null)"; then
    log_error "Failed to inventory E2E VMs before cleanup"
    return 1
  fi
  if ! aks_inventory="$(az aks list \
    --resource-group "${resource_group}" \
    --subscription "${subscription_id}" \
    --output json 2>/dev/null)"; then
    log_error "Failed to inventory E2E AKS clusters before cleanup"
    return 1
  fi
  if ! jq -e 'type == "array"' <<<"${vm_inventory}" >/dev/null || \
    ! jq -e 'type == "array"' <<<"${aks_inventory}" >/dev/null; then
    log_error "Azure returned an invalid VM or AKS cleanup inventory"
    return 1
  fi
  if ! resource_inventory="$(_resource_inventory "${resource_group}" "${subscription_id}")"; then
    log_error "Failed to inventory E2E resources before cleanup"
    return 1
  fi
  if ! legacy_disk_output="$(_legacy_implicit_os_disk_ids_from_inventory \
    "${resource_inventory}" \
    "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" \
    "${kubeadm_vm_name}" "${arc_vm_name}")"; then
    return 1
  fi
  while IFS= read -r disk_id; do
    [[ -z "${disk_id}" ]] || legacy_implicit_os_disk_ids+=("${disk_id}")
  done <<<"${legacy_disk_output}"
  for vm_name in "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" "${kubeadm_vm_name}" "${arc_vm_name}"; do
    [[ -n "${vm_name}" ]] || continue
    if ! disk_id="$(jq -r --arg name "${vm_name}" \
      '.[] | select(.name == $name) | .storageProfile.osDisk.managedDisk.id // empty' \
      <<<"${vm_inventory}")"; then
      log_error "Failed to inspect OS disk for VM '${vm_name}'"
      return 1
    fi
    [[ -z "${disk_id}" ]] || managed_disk_ids+=("${disk_id}")
    if ! nic_output="$(jq -r --arg name "${vm_name}" \
      '.[] | select(.name == $name) | .networkProfile.networkInterfaces[]?.id // empty' \
      <<<"${vm_inventory}")"; then
      log_error "Failed to inspect network interfaces for VM '${vm_name}'"
      return 1
    fi
    while read -r nic_id; do
      [[ -z "${nic_id}" ]] || nic_ids+=("${nic_id}")
    done <<<"${nic_output}"
  done
  if ! live_node_resource_group="$(jq -r --arg name "${cluster_name}" \
    '.[] | select(.name == $name) | .nodeResourceGroup // empty' \
    <<<"${aks_inventory}")"; then
    log_error "Failed to inspect the AKS node resource group"
    return 1
  fi
  if ! live_cluster_location="$(jq -r --arg name "${cluster_name}" \
    '.[] | select(.name == $name) | .location // empty' \
    <<<"${aks_inventory}")"; then
    log_error "Failed to inspect the AKS cluster location"
    return 1
  fi
  if [[ -n "${live_cluster_location}" ]]; then
    if [[ -n "${location}" && "${location,,}" != "${live_cluster_location,,}" ]]; then
      log_error "Refusing cleanup because live AKS location '${live_cluster_location}' differs from state '${location}'"
      return 1
    fi
    location="${live_cluster_location}"
    state_set "location" "${location}" || return 1
  fi
  if [[ -n "${live_node_resource_group}" ]]; then
    if ! _validate_expected_node_resource_group \
      "${live_node_resource_group}" "${name_suffix}" "${resource_group}" "${cluster_name}" "${location}"; then
      return 1
    fi
    if [[ -n "${node_resource_group}" && "${node_resource_group}" != "${live_node_resource_group}" ]]; then
      log_warn "Live AKS node resource group differs from state; using the live cluster value '${live_node_resource_group}'"
    fi
    node_resource_group="${live_node_resource_group}"
    state_set "node_resource_group" "${node_resource_group}" || return 1
  elif ! _validate_expected_node_resource_group \
    "${node_resource_group}" "${name_suffix}" "${resource_group}" "${cluster_name}" "${location}"; then
    return 1
  fi
  if [[ -n "${node_resource_group}" && "${node_resource_group}" == "${resource_group}" ]]; then
    log_error "Refusing to delete parent resource group as an AKS node resource group"
    return 1
  fi

  # Arc lifecycle is external to Flex Node. Remove the E2E-owned Arc resource
  # explicitly before deleting its backing Azure evaluation VM.
  log_info "[1/8] Deleting Arc machine resource..."
  if [[ -n "${arc_machine_id}" ]]; then
    az rest --method delete --url "https://management.azure.com${arc_machine_id}?api-version=2024-07-10" --output none 2>/dev/null || true
  fi

  # Start independent VM and AKS deletes together, then wait for all of them
  # before removing their network dependencies.
  for vm_name in "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" "${kubeadm_vm_name}" "${arc_vm_name}"; do
    [[ -n "${vm_name}" ]] || continue
    log_info "Deleting VM: ${vm_name}..."
    az vm delete --resource-group "${resource_group}" --name "${vm_name}" \
      --subscription "${subscription_id}" --force-deletion yes --yes --no-wait 2>/dev/null || true
  done

  if [[ -n "${cluster_name}" ]]; then
    log_info "Deleting AKS cluster: ${cluster_name}..."
    az aks delete --resource-group "${resource_group}" --name "${cluster_name}" \
      --subscription "${subscription_id}" --yes --no-wait 2>/dev/null || true
  fi

  for vm_name in "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" "${kubeadm_vm_name}" "${arc_vm_name}"; do
    [[ -n "${vm_name}" ]] || continue
    local wait_timeout
    if ! wait_timeout="$(_remaining_cleanup_timeout "${cleanup_deadline}")"; then
      log_error "Cleanup deadline reached while waiting for VMs"
      cleanup_failed=1
      break
    fi
    if ! az vm wait --resource-group "${resource_group}" --name "${vm_name}" \
      --subscription "${subscription_id}" --deleted --interval 10 --timeout "${wait_timeout}" 2>/dev/null; then
      if az vm show --resource-group "${resource_group}" --name "${vm_name}" \
        --subscription "${subscription_id}" --output none 2>/dev/null; then
        log_error "VM still exists after cleanup timeout: ${vm_name}"
        cleanup_failed=1
      fi
    fi
    az disk delete --resource-group "${resource_group}" --name "${vm_name}-osdisk" \
      --subscription "${subscription_id}" --yes --output none 2>/dev/null || true
    az network nic delete --resource-group "${resource_group}" --name "${vm_name}-nic" \
      --subscription "${subscription_id}" --output none 2>/dev/null || true
    az network public-ip delete --resource-group "${resource_group}" --name "${vm_name}-pip" \
      --subscription "${subscription_id}" --output none 2>/dev/null || true
  done

  for disk_id in "${managed_disk_ids[@]}" "${legacy_implicit_os_disk_ids[@]}"; do
    az resource delete --ids "${disk_id}" --subscription "${subscription_id}" --output none 2>/dev/null || true
  done
  for nic_id in "${nic_ids[@]}"; do
    az resource delete --ids "${nic_id}" --subscription "${subscription_id}" --output none 2>/dev/null || true
  done

  if [[ -n "${cluster_name}" ]]; then
    local aks_wait_timeout
    if ! aks_wait_timeout="$(_remaining_cleanup_timeout "${cleanup_deadline}")"; then
      log_error "Cleanup deadline reached before AKS deletion completed"
      cleanup_failed=1
    elif ! az aks wait \
      --resource-group "${resource_group}" --name "${cluster_name}" \
      --subscription "${subscription_id}" --deleted --interval 10 \
      --timeout "${aks_wait_timeout}" 2>/dev/null; then
      if az aks show --resource-group "${resource_group}" --name "${cluster_name}" \
        --subscription "${subscription_id}" --output none 2>/dev/null; then
        log_error "AKS cluster still exists after cleanup timeout: ${cluster_name}"
        cleanup_failed=1
      fi
    fi
  fi

  if ! _delete_node_resource_group "${node_resource_group}" "${subscription_id}" "${cleanup_deadline}"; then
    cleanup_failed=1
  fi

  if [[ -n "${arc_machine_id}" ]]; then
    local arc_wait_timeout
    if ! arc_wait_timeout="$(_remaining_cleanup_timeout "${cleanup_deadline}")"; then
      log_error "Cleanup deadline reached before Arc machine deletion completed"
      cleanup_failed=1
    elif ! az resource wait --ids "${arc_machine_id}" --subscription "${subscription_id}" \
      --api-version 2024-07-10 --deleted --interval 10 --timeout "${arc_wait_timeout}" \
      --output none 2>/dev/null; then
      log_error "Arc machine still exists after cleanup timeout: ${arc_machine_id}"
      cleanup_failed=1
    fi
  fi

  [[ -z "${vnet_name}" ]] || az network vnet delete \
    --resource-group "${resource_group}" --name "${vnet_name}" \
    --subscription "${subscription_id}" --output none 2>/dev/null || true
  [[ -z "${nsg_name}" ]] || az network nsg delete \
    --resource-group "${resource_group}" --name "${nsg_name}" \
    --subscription "${subscription_id}" --output none 2>/dev/null || true

  log_info "[8/8] Cleaning up tagged network and disk resources..."
  local remaining_ids legacy_remaining_ids empty_inventories=0
  for _ in 1 2 3 4; do
    if ! _delete_tagged_resources "${resource_group}" "${resource_owner}" "${subscription_id}" "${legacy_tag_name_suffix}"; then
      cleanup_failed=1
      break
    fi
    # Old VM deployments let Azure choose names such as
    # <vm>_OsDisk_1_<hash>. Those disks can be detached, untagged, and absent
    # from `az vm list`, so rediscover and retry them from the full RG inventory.
    for disk_id in "${legacy_implicit_os_disk_ids[@]}"; do
      az resource delete --ids "${disk_id}" --subscription "${subscription_id}" --output none 2>/dev/null || true
    done
    if ! _delete_legacy_implicit_os_disks \
      "${resource_group}" "${subscription_id}" \
      "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" \
      "${kubeadm_vm_name}" "${arc_vm_name}"; then
      log_error "Failed to delete legacy implicit OS disks"
      cleanup_failed=1
      break
    fi
    if ! remaining_ids="$(_tagged_resource_ids "${resource_group}" "${resource_owner}" "${subscription_id}" "${legacy_tag_name_suffix}")"; then
      log_error "Failed to verify tagged-resource deletion"
      cleanup_failed=1
      break
    fi
    if ! legacy_remaining_ids="$(_legacy_implicit_os_disk_ids \
      "${resource_group}" "${subscription_id}" \
      "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" \
      "${kubeadm_vm_name}" "${arc_vm_name}")"; then
      log_error "Failed to verify legacy implicit OS-disk deletion"
      cleanup_failed=1
      break
    fi
    if [[ -z "${remaining_ids}" && -z "${legacy_remaining_ids}" ]]; then
      empty_inventories=$((empty_inventories + 1))
      if (( empty_inventories >= 2 )); then
        break
      fi
    else
      empty_inventories=0
    fi
    sleep "${E2E_CLEANUP_POLL_INTERVAL:-5}"
  done

  if (( empty_inventories < 2 )); then
    log_error "Residual-resource inventory did not remain empty for two consecutive checks"
    cleanup_failed=1
  fi

  if ! remaining_ids="$(_tagged_resource_ids "${resource_group}" "${resource_owner}" "${subscription_id}" "${legacy_tag_name_suffix}")"; then
    log_error "Failed final tagged-resource verification"
    cleanup_failed=1
  elif [[ -n "${remaining_ids}" ]]; then
    log_error "Tagged E2E resources remain after cleanup:"
    printf '%s\n' "${remaining_ids}" >&2
    cleanup_failed=1
  fi

  local final_inventory expected_names known_remaining captured_id resource_name
  local -a expected_name_args=()
  if ! final_inventory="$(_resource_inventory "${resource_group}" "${subscription_id}")"; then
    log_error "Failed final exact-name resource verification"
    cleanup_failed=1
    final_inventory='[]'
  fi
  for resource_name in "${cluster_name}" "${arc_machine_name}" "${vnet_name}" "${nsg_name}"; do
    [[ -z "${resource_name}" ]] || expected_name_args+=("${resource_name}")
  done
  for vm_name in "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" "${kubeadm_vm_name}" "${arc_vm_name}"; do
    [[ -n "${vm_name}" ]] || continue
    expected_name_args+=("${vm_name}" "${vm_name}-nic" "${vm_name}-pip" "${vm_name}-osdisk")
  done
  if ! expected_names="$(jq -cn --args '$ARGS.positional' -- "${expected_name_args[@]}")"; then
    log_error "Failed to build exact-name cleanup inventory"
    cleanup_failed=1
    expected_names='[]'
  fi
  if ! known_remaining="$(jq -er --argjson names "${expected_names}" '
    if type != "array" then error("resource list must be an array")
    else [.[] | select(.name as $name | $names | index($name)) | .id] | join("\n")
    end
  ' <<<"${final_inventory}")"; then
    log_error "Azure returned invalid final resource inventory"
    cleanup_failed=1
    known_remaining=""
  fi
  if ! legacy_remaining_ids="$(_legacy_implicit_os_disk_ids_from_inventory \
    "${final_inventory}" \
    "${msi_vm_name}" "${token_vm_name}" "${offline_vm_name}" \
    "${kubeadm_vm_name}" "${arc_vm_name}")"; then
    cleanup_failed=1
    legacy_remaining_ids=""
  elif [[ -n "${legacy_remaining_ids}" ]]; then
    known_remaining+=$'\n'"${legacy_remaining_ids}"
  fi
  for captured_id in "${managed_disk_ids[@]}" "${legacy_implicit_os_disk_ids[@]}" "${nic_ids[@]}" "${arc_machine_id}"; do
    [[ -n "${captured_id}" ]] || continue
    if jq -e --arg id "${captured_id}" '.[] | select(.id == $id)' <<<"${final_inventory}" >/dev/null; then
      known_remaining+=$'\n'"${captured_id}"
    fi
  done
  if [[ -n "${known_remaining}" ]]; then
    log_error "Known E2E resources remain after cleanup:"
    printf '%s\n' "${known_remaining}" >&2
    cleanup_failed=1
  fi

  if [[ "${cleanup_failed}" != "0" ]]; then
    return 1
  fi

  state_set "lifecycle" "cleaned" || return 1
  state_set "cleanup_complete" "true" || return 1
  log_success "Cleanup completed and no known E2E resources remain"
}
