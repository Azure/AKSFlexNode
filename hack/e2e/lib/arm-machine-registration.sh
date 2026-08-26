#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/arm-machine-registration.sh - Real ARM Machine registration E2E
#
# Reuses the managed-identity VM before the controller-backed lifecycle tests.
# The scenario proves that a missing ARM Machine is created by EnsureMachine,
# validates the resulting resource and Kubernetes Node, then resets the host so
# the regular MSI scenario can run unchanged.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_ARM_MACHINE_REGISTRATION_LOADED:-}" ]] && return 0
readonly _E2E_ARM_MACHINE_REGISTRATION_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

_arm_machine_collection_url() {
  local cluster_id="$1"
  printf 'https://management.azure.com%s/agentPools/%s/machines?api-version=%s' \
    "${cluster_id}" "${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}" "${E2E_ARM_MACHINE_API_VERSION}"
}

_arm_machine_url() {
  local cluster_id="$1"
  local machine_name="$2"
  printf 'https://management.azure.com%s/agentPools/%s/machines/%s?api-version=%s' \
    "${cluster_id}" "${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}" "${machine_name}" "${E2E_ARM_MACHINE_API_VERSION}"
}

_require_arm_machine_preview() {
  local state
  state="$(az feature show \
    --subscription "${AZURE_SUBSCRIPTION_ID}" \
    --namespace Microsoft.ContainerService \
    --name PutMachinePreview \
    --query properties.state \
    --output tsv 2>/dev/null || true)"
  if [[ "${state}" != "Registered" ]]; then
    log_error "Microsoft.ContainerService/PutMachinePreview must be Registered; current state: ${state:-unknown}"
    return 1
  fi
}

_assert_arm_machine_absent() {
  local cluster_id="$1"
  local machine_name="$2"
  local response_file="${E2E_LOG_DIR}/arm-machines-before-registration.json"

  log_info "Verifying ARM Machine '${machine_name}' does not exist before bootstrap..."
  az rest \
    --only-show-errors \
    --method get \
    --url "$(_arm_machine_collection_url "${cluster_id}")" \
    --output json > "${response_file}"

  if ! jq -e --arg name "${machine_name}" \
      '[.value[]? | select((.name | ascii_downcase) == ($name | ascii_downcase))] | length == 0' \
      "${response_file}" >/dev/null; then
    log_error "ARM Machine '${machine_name}' already exists; registration would not exercise the create path"
    return 1
  fi
}

_wait_for_msi_arm_machine_access() {
  local vm_ip="$1"
  local cluster_id="$2"
  local machine_name="$3"
  local machine_url
  machine_url="$(_arm_machine_url "${cluster_id}" "${machine_name}")"

  log_info "Waiting for the MSI role assignment to authorize ARM Machine requests..."
  remote_exec "${vm_ip}" \
    "env ARM_MACHINE_URL='${machine_url}' E2E_NODE_JOIN_TIMEOUT='${E2E_NODE_JOIN_TIMEOUT}' bash -s" <<'REMOTE'
set -euo pipefail

response_file=/tmp/aks-flex-arm-machine-access.json
deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
while (( SECONDS < deadline )); do
  token="$(curl -fsS \
    -H Metadata:true \
    'http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F' |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["access_token"])' 2>/dev/null || true)"
  if [[ -z "${token}" ]]; then
    echo "Managed identity token is not available yet; retrying..."
    sleep 10
    continue
  fi

  status="$(curl -sS -o "${response_file}" -w '%{http_code}' \
    -H "Authorization: Bearer ${token}" \
    "${ARM_MACHINE_URL}" || true)"
  unset token
  case "${status}" in
    404)
      echo "Managed identity is authorized and the ARM Machine is absent"
      exit 0
      ;;
    200)
      echo "ARM Machine unexpectedly exists before registration" >&2
      exit 1
      ;;
    *)
      echo "ARM Machine authorization returned HTTP ${status:-unknown}; retrying..."
      sleep 10
      ;;
  esac
done

echo "Managed identity was not authorized for ARM Machine requests before timeout" >&2
cat "${response_file}" >&2 2>/dev/null || true
exit 1
REMOTE
}

_validate_arm_machine() {
  local cluster_id="$1"
  local machine_name="$2"
  local response_file="${E2E_LOG_DIR}/arm-machine-registration.json"

  log_info "Reading the registered ARM Machine '${machine_name}'..."
  az rest \
    --only-show-errors \
    --method get \
    --url "$(_arm_machine_url "${cluster_id}" "${machine_name}")" \
    --output json > "${response_file}"

  if ! jq -e \
      --arg name "${machine_name}" \
      --arg version "${E2E_KUBERNETES_VERSION}" \
      '((.name | ascii_downcase) == ($name | ascii_downcase))
       and (.properties.kubernetes.orchestratorVersion == $version)
       and (.properties.provisioningState == "Succeeded")
       and ((.properties.eTag // "") | length > 0)' \
      "${response_file}" >/dev/null; then
    log_error "Registered ARM Machine did not match the requested goal or reach Succeeded"
    jq '{name, properties: {eTag: .properties.eTag, provisioningState: .properties.provisioningState, kubernetes: .properties.kubernetes}}' \
      "${response_file}" >&2
    return 1
  fi

  log_success "ARM Machine '${machine_name}' exists with the requested Kubernetes goal"
}

_validate_arm_machine_create_log() {
  local vm_ip="$1"
  local machine_name="$2"
  local log_file="${E2E_LOG_DIR}/arm-machine-registration-agent.log"

  log_info "Verifying bootstrap executed the ARM Machine create path..."
  remote_exec "${vm_ip}" "sudo bash -s" > "${log_file}" <<'REMOTE'
set -euo pipefail

journalctl -u aks-flex-node-msi-arm-registration.service --no-pager 2>/dev/null || true
tail -n 200 /var/log/aks-flex-node/aks-flex-node.log 2>/dev/null || true
REMOTE

  if grep -F 'creating or updating AKS machine' "${log_file}" |
      grep -F "machine=${machine_name}" >/dev/null; then
    return 0
  fi

  echo "ARM Machine creation log was not found for ${machine_name}" >&2
  tail -n 100 "${log_file}" >&2 || true
  return 1
}

_reset_arm_registration_host() {
  local vm_ip="$1"
  local machine_name="$2"

  log_info "Resetting the MSI host after ARM Machine registration validation..."
  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE'
set -euo pipefail

/usr/local/bin/aks-flex-node reset
systemctl stop aks-flex-node-msi-arm-registration.service 2>/dev/null || true
systemctl reset-failed aks-flex-node-msi-arm-registration.service 2>/dev/null || true
REMOTE

  _validate_rp_delete_cleanup "${vm_ip}"
  kubectl delete node "${machine_name}" --ignore-not-found --wait=false
  validate_node_absent "${machine_name}"
}

arm_machine_registration_e2e() {
  log_section "ARM Machine Registration E2E (MSI)"
  local start
  start="$(timer_start)"

  local cluster_id vm_ip vm_name machine_name
  cluster_id="$(state_get cluster_id)"
  vm_ip="$(state_get msi_vm_ip)"
  vm_name="$(state_get msi_vm_name)"
  machine_name="${vm_name}-arm"
  state_set "arm_registration_machine_name" "${machine_name}"

  _require_arm_machine_preview
  _assert_arm_machine_absent "${cluster_id}" "${machine_name}"
  _wait_for_msi_arm_machine_access "${vm_ip}" "${cluster_id}" "${machine_name}"
  node_join_msi_arm_registration "${machine_name}"
  _validate_arm_machine "${cluster_id}" "${machine_name}"
  _validate_arm_machine_create_log "${vm_ip}" "${machine_name}"
  validate_node_joined "${machine_name}"
  _reset_arm_registration_host "${vm_ip}" "${machine_name}"

  log_success "ARM Machine registration E2E passed in $(timer_elapsed "${start}")s"
}
