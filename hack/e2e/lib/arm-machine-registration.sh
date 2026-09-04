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

_arm_machine_delete_url() {
  local cluster_id="$1"
  printf 'https://management.azure.com%s/agentPools/%s/deleteMachines?api-version=%s' \
    "${cluster_id}" "${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}" "${E2E_ARM_MACHINE_API_VERSION}"
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

_arm_machine_count() {
  local cluster_id="$1"
  local machine_name="$2"
  local response_file="${E2E_LOG_DIR}/arm-machines.json"

  az rest \
    --only-show-errors \
    --method get \
    --url "$(_arm_machine_collection_url "${cluster_id}")" \
    --output json > "${response_file}" || return 1

  jq -er --arg name "${machine_name}" \
    '[.value[]? | select((.name | ascii_downcase) == ($name | ascii_downcase))] | length' \
    "${response_file}"
}

_assert_arm_machine_absent() {
  local cluster_id="$1"
  local machine_name="$2"
  local count

  log_info "Verifying ARM Machine '${machine_name}' does not exist before bootstrap..."
  count="$(_arm_machine_count "${cluster_id}" "${machine_name}")" || return 1
  if [[ "${count}" != "0" ]]; then
    log_error "ARM Machine '${machine_name}' already exists; registration would not exercise the create path"
    return 1
  fi
}

_wait_for_arm_machine_delete() {
  local operation_url="$1"
  local machine_name="$2"
  local response_file="${E2E_LOG_DIR}/arm-machine-delete-operation.json"
  local deadline operation_status
  deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))

  while (( SECONDS < deadline )); do
    if ! az rest \
      --only-show-errors \
      --method get \
      --url "${operation_url}" \
      --output json > "${response_file}"; then
      log_warn "Failed to read ARM Machine delete operation; retrying..."
      sleep 10
      continue
    fi

    operation_status="$(jq -r '.status // empty' "${response_file}")"
    case "${operation_status,,}" in
      succeeded)
        return 0
        ;;
      failed|canceled)
        log_error "ARM Machine '${machine_name}' delete operation ${operation_status}"
        jq '{status, error}' "${response_file}" >&2 || true
        return 1
        ;;
      *)
        sleep 10
        ;;
    esac
  done

  log_error "Timed out waiting for ARM Machine '${machine_name}' delete operation"
  return 1
}

_delete_arm_machine() {
  local cluster_id="$1"
  local machine_name="$2"
  local count delete_body headers_file response_file token http_status header_name header_value
  local async_operation_url="" location_url=""

  count="$(_arm_machine_count "${cluster_id}" "${machine_name}")" || return 1
  if [[ "${count}" == "0" ]]; then
    log_info "ARM Machine '${machine_name}' is already absent"
    return 0
  fi

  # MachinesClient has no per-resource DELETE. AKS exposes deletion as the
  # AgentPools_DeleteMachines action on the preview API used by this scenario.
  log_info "Deleting ARM Machine '${machine_name}' through the agent pool action..."
  delete_body="$(jq -cn --arg name "${machine_name}" '{machineNames: [$name]}')"
  headers_file="${E2E_WORK_DIR}/arm-machine-delete-headers"
  response_file="${E2E_LOG_DIR}/arm-machine-delete-response.json"
  install -m 0600 /dev/null "${headers_file}"
  install -m 0600 /dev/null "${response_file}"
  token="$(az account get-access-token \
    --only-show-errors \
    --subscription "${AZURE_SUBSCRIPTION_ID}" \
    --resource-type arm \
    --query accessToken \
    --output tsv)" || return 1

  # Feed the authorization header through stdin so the token is not exposed in
  # the curl process arguments.
  if ! http_status="$(
    printf 'header = "Authorization: Bearer %s"\n' "${token}" |
      curl --silent --show-error \
        --config - \
        --request POST \
        --header 'Content-Type: application/json' \
        --data "${delete_body}" \
        --dump-header "${headers_file}" \
        --output "${response_file}" \
        --write-out '%{http_code}' \
        "$(_arm_machine_delete_url "${cluster_id}")"
  )"; then
    unset token
    log_error "Failed to submit ARM Machine '${machine_name}' delete operation"
    return 1
  fi
  unset token

  while IFS=':' read -r header_name header_value; do
    header_value="${header_value# }"
    header_value="${header_value%$'\r'}"
    case "${header_name,,}" in
      azure-asyncoperation) async_operation_url="${header_value}" ;;
      location) location_url="${header_value}" ;;
    esac
  done < "${headers_file}"
  : > "${headers_file}"

  case "${http_status}" in
    200|204) ;;
    202)
      if [[ -z "${async_operation_url}" ]]; then
        async_operation_url="${location_url}"
      fi
      if [[ -z "${async_operation_url}" ]]; then
        log_error "ARM Machine delete response did not include an operation URL"
        return 1
      fi
      _wait_for_arm_machine_delete "${async_operation_url}" "${machine_name}" || return 1
      ;;
    *)
      log_error "ARM Machine '${machine_name}' delete request returned HTTP ${http_status}"
      jq '{error}' "${response_file}" >&2 2>/dev/null || true
      return 1
      ;;
  esac

  count="$(_arm_machine_count "${cluster_id}" "${machine_name}")" || return 1
  if [[ "${count}" != "0" ]]; then
    log_error "ARM Machine '${machine_name}' still exists after deleteMachines completed"
    return 1
  fi
  log_success "ARM Machine '${machine_name}' was deleted"
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

  # Feed the authorization header through stdin so the token is not exposed in
  # the curl process arguments.
  status="$(
    printf 'header = "Authorization: Bearer %s"\n' "${token}" |
      curl --silent --show-error \
        --config - \
        --output "${response_file}" \
        --write-out '%{http_code}' \
        "${ARM_MACHINE_URL}" || true
  )"
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
  local cluster_id="$2"
  local machine_name="$3"

  log_info "Resetting the MSI host after ARM Machine registration validation..."
  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE' || return 1
set -euo pipefail

/usr/local/bin/aks-flex-node reset
systemctl stop aks-flex-node-msi-arm-registration.service 2>/dev/null || true
systemctl reset-failed aks-flex-node-msi-arm-registration.service 2>/dev/null || true
REMOTE

  _validate_rp_delete_cleanup "${vm_ip}" || return 1
  kubectl delete node "${machine_name}" --ignore-not-found --wait=false || return 1
  validate_node_absent "${machine_name}" || return 1
  _delete_arm_machine "${cluster_id}" "${machine_name}"
}

_reset_previous_arm_registration_host() {
  local vm_ip="$1"
  local cluster_id="$2"
  local machine_name="$3"

  log_info "Resetting the MSI host state from the previous ARM registration attempt..."
  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE' || return 1
set -euo pipefail

if command -v /usr/local/bin/aks-flex-node >/dev/null 2>&1; then
  /usr/local/bin/aks-flex-node reset
fi
systemctl stop aks-flex-node-msi-arm-registration.service 2>/dev/null || true
systemctl reset-failed aks-flex-node-msi-arm-registration.service 2>/dev/null || true
REMOTE

  _validate_rp_delete_cleanup "${vm_ip}" || return 1
  kubectl delete node "${machine_name}" --ignore-not-found --wait=false || return 1
  validate_node_absent "${machine_name}" || return 1
  _delete_arm_machine "${cluster_id}" "${machine_name}"
}

_cleanup_failed_arm_registration() {
  local vm_ip="$1"
  local cluster_id="$2"
  local machine_name="$3"

  log_warn "Resetting the MSI host after a failed ARM registration test..."
  if _reset_previous_arm_registration_host "${vm_ip}" "${cluster_id}" "${machine_name}"; then
    state_set "arm_registration_machine_name" ""
  else
    log_warn "Failed to fully reset the MSI host during failure cleanup"
  fi
}

arm_machine_registration_e2e() (
  log_section "ARM Machine Registration E2E (MSI)"
  local start
  start="$(timer_start)"

  local cluster_id vm_ip vm_name machine_name previous_machine_name
  cluster_id="$(state_get cluster_id)"
  vm_ip="$(state_get msi_vm_ip)"
  vm_name="$(state_get msi_vm_name)"
  previous_machine_name="$(state_get arm_registration_machine_name)"
  machine_name="${vm_name}-arm"

  _require_arm_machine_preview
  if [[ -n "${previous_machine_name}" ]]; then
    _reset_previous_arm_registration_host "${vm_ip}" "${cluster_id}" "${previous_machine_name}"
    state_set "arm_registration_machine_name" ""
  fi
  state_set "arm_registration_machine_name" "${machine_name}"
  _assert_arm_machine_absent "${cluster_id}" "${machine_name}"
  _wait_for_msi_arm_machine_access "${vm_ip}" "${cluster_id}" "${machine_name}"

  local cleanup_armed=1
  cleanup_on_exit() {
    local exit_code="$1"
    trap - EXIT
    if (( cleanup_armed == 1 )); then
      _cleanup_failed_arm_registration "${vm_ip}" "${cluster_id}" "${machine_name}"
    fi
    exit "${exit_code}"
  }
  trap 'cleanup_on_exit "$?"' EXIT

  node_join_msi_arm_registration "${machine_name}"
  _validate_arm_machine "${cluster_id}" "${machine_name}"
  _validate_arm_machine_create_log "${vm_ip}" "${machine_name}"
  validate_node_joined "${machine_name}"
  _reset_arm_registration_host "${vm_ip}" "${cluster_id}" "${machine_name}"
  state_set "arm_registration_machine_name" ""

  cleanup_armed=0
  trap - EXIT

  log_success "ARM Machine registration E2E passed in $(timer_elapsed "${start}")s"
)
