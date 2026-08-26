#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-arc.sh - Join / unjoin an AKS Flex Node by using the
#                                  identity of an externally managed Arc server
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_ARC_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_ARC_LOADED=1
readonly arcHybridComputeAPIVersion="2024-07-10"
readonly aksContributorRoleDefinitionID="ed7f3fbd-7b88-4dd4-9017-9adb7ce333f8"
readonly arcBootstrapDataAction="Microsoft.ContainerService/managedClusters/agentPools/listBootstrapData/action"
readonly arcRoleAssignmentWaitAttempts=12
readonly arcRoleAssignmentPollInterval=5
readonly arcBootstrapFetchAttempts=60
readonly arcBootstrapFetchPollInterval=15

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/controller.sh"

_validate_arc_resource_providers() {
  local namespace state
  for namespace in Microsoft.HybridCompute Microsoft.HybridConnectivity Microsoft.GuestConfiguration; do
    state="$(az provider show --namespace "${namespace}" --query registrationState -o tsv)"
    if [[ "${state}" != "Registered" ]]; then
      log_error "Azure resource provider ${namespace} must be registered before E2E"
      return 1
    fi
  done
}

_remove_arc_vm_extensions() {
  local resource_group="$1" vm_name="$2"
  local extension

  while IFS= read -r extension; do
    [[ -n "${extension}" ]] || continue
    log_info "Removing Azure VM extension ${extension} before Arc evaluation setup"
    az vm extension delete --resource-group "${resource_group}" --vm-name "${vm_name}" --name "${extension}" --output none
  done < <(az vm extension list --resource-group "${resource_group}" --vm-name "${vm_name}" --query '[].name' -o tsv)
}

_prepare_arc_evaluation_vm() {
  local vm_ip="$1"

  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail

sudo install -d -m 0755 /etc/systemd/system.conf.d
printf '[Manager]\nDefaultEnvironment=MSFT_ARC_TEST=true\n' |
  sudo tee /etc/systemd/system.conf.d/99-arc-test.conf >/dev/null
sudo systemctl set-environment MSFT_ARC_TEST=true
sudo systemctl stop walinuxagent.service 2>/dev/null || true
sudo systemctl disable walinuxagent.service 2>/dev/null || true

sudo tee /usr/local/sbin/block-azure-imds.sh >/dev/null <<'SCRIPT'
#!/bin/sh
set -eu
for ip in 169.254.169.254 169.254.169.253; do
  iptables -C OUTPUT -d "${ip}/32" -j REJECT 2>/dev/null ||
    iptables -I OUTPUT 1 -d "${ip}/32" -j REJECT
done
SCRIPT
sudo chmod 0755 /usr/local/sbin/block-azure-imds.sh
sudo tee /etc/systemd/system/block-azure-imds.service >/dev/null <<'UNIT'
[Unit]
Description=Block Azure IMDS for Arc E2E evaluation VM
After=network-online.target nftables.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/block-azure-imds.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
UNIT
sudo systemctl daemon-reload
sudo systemctl enable block-azure-imds.service >/dev/null
sudo systemctl restart block-azure-imds.service

if ! command -v azcmagent >/dev/null 2>&1; then
  curl -fsSL https://gbl.his.arc.azure.com/azcmagent-linux |
    sudo env MSFT_ARC_TEST=true bash
fi

# Package installation can invoke needrestart after the initial disable.
sudo systemctl stop walinuxagent.service 2>/dev/null || true
sudo systemctl disable walinuxagent.service 2>/dev/null || true
REMOTE
}

_connect_arc_machine() {
  local vm_ip="$1" arc_machine_name="$2" resource_group="$3" location="$4" subscription_id="$5" tenant_id="$6"

  if remote_exec "${vm_ip}" "sudo azcmagent show 2>/dev/null | grep -Eq 'Agent Status[[:space:]]*:[[:space:]]*Connected'"; then
    log_info "Arc machine ${arc_machine_name} is already connected"
    return 0
  fi

  local arm_token
  # Reuse azure/login's cached ARM token. Requesting a new audience here can
  # require refreshing the short-lived GitHub OIDC client assertion after the
  # infrastructure deployment has already consumed several minutes.
  arm_token="$(az account get-access-token --query accessToken -o tsv)"
  printf '%s\n' "${arm_token}" | remote_exec "${vm_ip}" \
    "read -r ARM_TOKEN; sudo env MSFT_ARC_TEST=true azcmagent connect --subscription-id '${subscription_id}' --resource-group '${resource_group}' --tenant-id '${tenant_id}' --location '${location}' --resource-name '${arc_machine_name}' --tags purpose=e2e-test --access-token \"\${ARM_TOKEN}\""
  unset arm_token
}

_wait_for_arc_identity() {
  local arc_resource_id="$1"
  local deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
  local principal_id=""

  while ((SECONDS < deadline)); do
    principal_id="$(az resource show --ids "${arc_resource_id}" --api-version "${arcHybridComputeAPIVersion}" --query identity.principalId -o tsv 2>/dev/null || true)"
    if [[ -n "${principal_id}" ]]; then
      echo "${principal_id}"
      return 0
    fi
    sleep 5
  done

  log_error "Timed out waiting for Arc identity on ${arc_resource_id}"
  return 1
}

_arc_role_records_match() {
  local records="$1" principal_id="$2" cluster_id="$3"

  jq -e \
    --arg principalID "${principal_id}" \
    --arg roleDefinitionID "${aksContributorRoleDefinitionID}" \
    --arg scope "${cluster_id}" '
      def records: if type == "array" then . else [.] end;
      [
        records[]
        | select(
            ((.principalId // "") | ascii_downcase) == ($principalID | ascii_downcase)
            and (((.roleDefinitionId // "") | split("/") | last | ascii_downcase) == ($roleDefinitionID | ascii_downcase))
            and (((.scope // "") | ascii_downcase) == ($scope | ascii_downcase))
          )
      ]
      | length == 1
    ' <<<"${records}" >/dev/null
}

_arc_role_assignment_visible() {
  local principal_id="$1" cluster_id="$2" subscription_id="$3"
  local assignments

  # Keep lookup failure distinct from a valid empty result so callers never
  # create a duplicate assignment when Azure CLI or ARM is transiently down.
  if ! assignments="$(az role assignment list \
    --assignee-object-id "${principal_id}" \
    --role "${aksContributorRoleDefinitionID}" \
    --scope "${cluster_id}" \
    --fill-principal-name false \
    --fill-role-definition-name false \
    --subscription "${subscription_id}" \
    --output json)"; then
    return 2
  fi
  _arc_role_records_match "${assignments}" "${principal_id}" "${cluster_id}"
}

_aks_contributor_allows_bootstrap_data() {
  local subscription_id="$1" definition

  if ! definition="$(az role definition show \
    --id "/subscriptions/${subscription_id}/providers/Microsoft.Authorization/roleDefinitions/${aksContributorRoleDefinitionID}" \
    --subscription "${subscription_id}" \
    --output json)"; then
    return 1
  fi
  jq -e --arg action "${arcBootstrapDataAction}" '
    def covers($grant; $requested):
      ($grant | ascii_downcase) as $normalizedGrant
      | ($requested | ascii_downcase) as $normalizedRequested
      | $normalizedGrant == "*"
        or $normalizedGrant == $normalizedRequested
        or (($normalizedGrant | endswith("/*")) and ($normalizedRequested | startswith($normalizedGrant[0:-1])));
    [
      .permissions[]?
      | select(
          any(.actions[]?; covers(.; $action))
          and all(.notActions[]?; covers(.; $action) | not)
        )
    ]
    | length > 0
  ' <<<"${definition}" >/dev/null
}

_ensure_arc_role_assignment() {
  local principal_id="$1" cluster_id="$2" subscription_id="$3"
  local created_assignment assignment_status=0

  if ! state_set "arc_role_assigned" "false"; then
    return 1
  fi
  _arc_role_assignment_visible "${principal_id}" "${cluster_id}" "${subscription_id}" || assignment_status=$?
  case "${assignment_status}" in
    0)
      if ! state_set "arc_role_assigned" "true"; then
        return 1
      fi
      log_info "Verified the Arc AKS Contributor role assignment"
      return 0
      ;;
    1) ;;
    *)
      log_error "Failed to query the Arc AKS Contributor role assignment; refusing to create a possibly duplicate assignment"
      return 1
      ;;
  esac

  if ! created_assignment="$(az role assignment create \
    --assignee-object-id "${principal_id}" \
    --assignee-principal-type ServicePrincipal \
    --role "${aksContributorRoleDefinitionID}" \
    --scope "${cluster_id}" \
    --subscription "${subscription_id}" \
    --output json)"; then
    log_error "Failed to create the Arc AKS Contributor role assignment"
    return 1
  fi
  if ! _arc_role_records_match "${created_assignment}" "${principal_id}" "${cluster_id}"; then
    log_error "Azure returned an Arc role assignment that did not match the requested principal, role, and cluster scope"
    return 1
  fi

  for attempt in $(seq 1 "${arcRoleAssignmentWaitAttempts}"); do
    assignment_status=0
    _arc_role_assignment_visible "${principal_id}" "${cluster_id}" "${subscription_id}" || assignment_status=$?
    case "${assignment_status}" in
      0)
        if ! state_set "arc_role_assigned" "true"; then
          return 1
        fi
        log_info "Verified the Arc AKS Contributor role assignment"
        return 0
        ;;
      1) ;;
      *)
        log_warn "Failed to query the newly created Arc role assignment; retrying verification (${attempt}/${arcRoleAssignmentWaitAttempts})"
        ;;
    esac
    if [[ "${attempt}" -lt "${arcRoleAssignmentWaitAttempts}" ]]; then
      sleep "${arcRoleAssignmentPollInterval}"
    fi
  done

  if [[ "${assignment_status}" -eq 1 ]]; then
    log_error "Arc AKS Contributor role assignment was not visible at the expected principal, role, and cluster scope"
  else
    log_error "Arc AKS Contributor role assignment could not be verified because Azure CLI queries kept failing"
  fi
  return 1
}

_arc_authorization_diagnostic_values() {
  local vm_ip="$1" arc_resource_id="$2" principal_id="$3" cluster_id="$4" subscription_id="$5"
  local assignment_visible=false role_allows_action=false principal_matches=false
  local himdsd_active=false azcmagent_connected=false current_principal remote_status

  if _arc_role_assignment_visible "${principal_id}" "${cluster_id}" "${subscription_id}" 2>/dev/null; then
    assignment_visible=true
  fi
  if _aks_contributor_allows_bootstrap_data "${subscription_id}" 2>/dev/null; then
    role_allows_action=true
  fi
  current_principal="$(az resource show \
    --ids "${arc_resource_id}" \
    --api-version "${arcHybridComputeAPIVersion}" \
    --query identity.principalId \
    --subscription "${subscription_id}" \
    --output tsv 2>/dev/null || true)"
  if [[ -n "${current_principal}" && "${current_principal,,}" == "${principal_id,,}" ]]; then
    principal_matches=true
  fi
  if remote_status="$(remote_exec "${vm_ip}" \
    "himdsd_active=false; azcmagent_connected=false; systemctl is-active --quiet himdsd.service && himdsd_active=true; sudo azcmagent show 2>/dev/null | grep -Eq 'Agent Status[[:space:]]*:[[:space:]]*Connected' && azcmagent_connected=true; printf '%s|%s\\n' \"\${himdsd_active}\" \"\${azcmagent_connected}\"")"; then
    IFS='|' read -r himdsd_active azcmagent_connected <<<"${remote_status}"
  fi

  printf '%s|%s|%s|%s|%s\n' \
    "${assignment_visible}" "${role_allows_action}" "${principal_matches}" \
    "${himdsd_active}" "${azcmagent_connected}"
}

_log_arc_authorization_diagnostics() {
  local attempt="$1" values="$2"
  local assignment_visible role_allows_action principal_matches himdsd_active azcmagent_connected
  IFS='|' read -r assignment_visible role_allows_action principal_matches himdsd_active azcmagent_connected <<<"${values}"

  log_info "Arc authorization diagnostics (${attempt}/${arcBootstrapFetchAttempts}): roleAssignmentVisible=${assignment_visible} roleAllowsListBootstrapData=${role_allows_action} arcPrincipalMatches=${principal_matches} himdsdActive=${himdsd_active} azcmagentConnected=${azcmagent_connected}"
}

_arc_fetch_error_summary() {
  local output="$1"
  local summary

  summary="$(grep -m1 -F 'fetch bootstrap data returned HTTP status' <<<"${output}" || true)"
  if [[ -n "${summary}" ]]; then
    printf '%s\n' "${summary}"
  else
    printf '%s\n' "remote fetch failed without a structured ARM response"
  fi
}

_fetch_arc_bootstrap_config() {
  local vm_ip="$1" vm_private_ip="$2" vm_name="$3" cluster_id="$4" output="$5"
  local arc_resource_id="$6" principal_id="$7" subscription_id="$8"
  local remote_binary="/tmp/aks-flex-node-arc-fetch"
  local remote_output="/tmp/aks-flex-node-arc-bootstrap.json"

  remote_copy "${E2E_BINARY}" "${vm_ip}" "${remote_binary}"
  remote_exec "${vm_ip}" "sudo chmod 0755 '${remote_binary}'"

  local fetched=0 last_fetch_error="" final_diagnostics=""
  # A newly created Arc principal and role assignment can take several minutes
  # to become effective across ARM frontends.
  for attempt in $(seq 1 "${arcBootstrapFetchAttempts}"); do
    if last_fetch_error="$(remote_exec "${vm_ip}" "sudo '${remote_binary}' fetch-bootstrap-data --auth arc --cluster-resource-id '${cluster_id}' --agent-pool-name '${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}' --output '${remote_output}'" 2>&1)"; then
      fetched=1
      break
    fi
    if [[ "${attempt}" -eq 1 || $((attempt % 10)) -eq 0 || "${attempt}" -eq "${arcBootstrapFetchAttempts}" ]]; then
      log_warn "Arc bootstrap-data fetch failed: $(_arc_fetch_error_summary "${last_fetch_error}")"
      final_diagnostics="$(_arc_authorization_diagnostic_values \
        "${vm_ip}" "${arc_resource_id}" "${principal_id}" "${cluster_id}" "${subscription_id}")"
      _log_arc_authorization_diagnostics "${attempt}" "${final_diagnostics}"
    fi
    if [[ "${attempt}" -lt "${arcBootstrapFetchAttempts}" ]]; then
      log_info "Arc bootstrap-data fetch not ready; retrying (${attempt}/${arcBootstrapFetchAttempts})"
      sleep "${arcBootstrapFetchPollInterval}"
    fi
  done
  if [[ "${fetched}" -ne 1 ]]; then
    local assignment_visible role_allows_action principal_matches himdsd_active azcmagent_connected
    IFS='|' read -r assignment_visible role_allows_action principal_matches himdsd_active azcmagent_connected <<<"${final_diagnostics}"
    if [[ "${last_fetch_error}" != *"HTTP status 403"* ]]; then
      log_error "Arc identity could not fetch AKS bootstrap data; ARM did not return HTTP 403"
    elif [[ "${assignment_visible}" != "true" ]]; then
      log_error "Arc identity could not fetch AKS bootstrap data because the expected role assignment is not visible"
    elif [[ "${principal_matches}" != "true" ]]; then
      log_error "Arc identity could not fetch AKS bootstrap data because the Arc principal no longer matches or could not be verified"
    elif [[ "${role_allows_action}" != "true" ]]; then
      log_error "Arc identity could not fetch AKS bootstrap data because AKS Contributor does not allow listBootstrapData or the role definition could not be verified"
    elif [[ "${himdsd_active}" != "true" || "${azcmagent_connected}" != "true" ]]; then
      log_error "Arc identity could not fetch AKS bootstrap data because the Arc identity service is not healthy"
    else
      log_error "Arc authorization is still HTTP 403 despite a verified assignment, matching principal, effective role action, and healthy Arc agent"
    fi
    return 1
  fi

  install -m 0600 /dev/null "${output}"
  remote_exec "${vm_ip}" "sudo cat '${remote_output}'" > "${output}"
  install -m 0600 /dev/null "${output}.tmp"
  jq \
    --arg nodeIP "${vm_private_ip}" \
    --arg nodeName "${vm_name}" \
    --arg machineEndpointURL "${E2E_CONTROLLER_SERVICE_PROXY_PATH}" \
    '.azure.arc = {"enabled": true}
      | del(.azure.managedIdentity, .azure.servicePrincipal)
      | .agent = ((.agent // {}) * {
          "logLevel": "debug",
          "nodeName": $nodeName,
          "machineClient": {"mode": "in-cluster", "endpointUrl": $machineEndpointURL},
          "requireMachineRegistration": true
        })
      | .node.kubelet.nodeIP = $nodeIP' \
    "${output}" > "${output}.tmp"
  mv "${output}.tmp" "${output}"

  remote_exec "${vm_ip}" "sudo rm -f '${remote_output}' '${remote_binary}'"
}

_validate_arc_service() {
  local vm_ip="$1"

  remote_exec "${vm_ip}" 'sudo bash -s' <<'REMOTE'
set -euo pipefail

grep -Eq '^After=.*(^| )himdsd\.service( |$)' /etc/systemd/system/aks-flex-node-agent.service
grep -Eq '^Wants=.*(^| )himdsd\.service( |$)' /etc/systemd/system/aks-flex-node-agent.service
jq -e '.azure.arc.enabled == true and (.azure | has("managedIdentity") | not) and (.azure | has("servicePrincipal") | not)' /etc/aks-flex-node/config.json >/dev/null
systemctl is-active --quiet himdsd.service
systemctl is-active --quiet aks-flex-node-agent.service
azcmagent show | grep -Eq 'Agent Status[[:space:]]*:[[:space:]]*Connected'

# Host networking reconciliation can replace iptables rules. Reapply the Arc
# evaluation guard and prove the Azure VM IMDS endpoints remain unavailable.
systemctl restart block-azure-imds.service
for ip in 169.254.169.254 169.254.169.253; do
  iptables -C OUTPUT -d "${ip}/32" -j REJECT
done
REMOTE
}

node_join_arc() {
  log_section "Joining Arc Node"
  local start
  start=$(timer_start)

  local vm_ip vm_private_ip vm_name arc_machine_name resource_group location subscription_id tenant_id cluster_id
  vm_ip="$(state_get arc_vm_ip)"
  vm_private_ip="$(state_get arc_vm_private_ip)"
  vm_name="$(state_get arc_vm_name)"
  arc_machine_name="$(state_get arc_machine_name)"
  resource_group="$(state_get resource_group)"
  location="$(state_get location)"
  subscription_id="$(state_get subscription_id)"
  tenant_id="$(state_get tenant_id)"
  cluster_id="$(state_get cluster_id)"

  _validate_arc_resource_providers
  _remove_arc_vm_extensions "${resource_group}" "${vm_name}"
  _prepare_arc_evaluation_vm "${vm_ip}"
  _connect_arc_machine "${vm_ip}" "${arc_machine_name}" "${resource_group}" "${location}" "${subscription_id}" "${tenant_id}"

  local arc_resource_id principal_id
  arc_resource_id="/subscriptions/${subscription_id}/resourceGroups/${resource_group}/providers/Microsoft.HybridCompute/machines/${arc_machine_name}"
  principal_id="$(_wait_for_arc_identity "${arc_resource_id}")"
  state_set "arc_machine_id" "${arc_resource_id}"
  state_set "arc_principal_id" "${principal_id}"

  if ! _aks_contributor_allows_bootstrap_data "${subscription_id}"; then
    log_error "AKS Contributor does not allow ${arcBootstrapDataAction}"
    return 1
  fi
  _ensure_arc_role_assignment "${principal_id}" "${cluster_id}" "${subscription_id}"

  local config_file="${E2E_WORK_DIR}/config-arc.json"
  _fetch_arc_bootstrap_config \
    "${vm_ip}" "${vm_private_ip}" "${vm_name}" "${cluster_id}" "${config_file}" \
    "${arc_resource_id}" "${principal_id}" "${subscription_id}"

  # The E2E CSR approver uses this source as a fallback when the managed AKS
  # approver isn't available. Runtime Machine reconciliation still uses ARM.
  machine_configmap_upsert "${vm_name}" "${E2E_KUBERNETES_VERSION}" "${E2E_KUBERNETES_VERSION}"
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-arc"
  _validate_arc_service "${vm_ip}"

  log_success "Arc node joined in $(timer_elapsed "${start}")s"
}

node_unjoin_arc() {
  log_section "Unjoining Arc Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get arc_vm_ip)"
  vm_name="$(state_get arc_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  # Flex Node reset must preserve the externally managed Arc connection.
  remote_exec "${vm_ip}" "systemctl is-active --quiet himdsd.service && sudo azcmagent show | grep -Eq 'Agent Status[[:space:]]*:[[:space:]]*Connected'"

  log_success "Arc node unjoined in $(timer_elapsed "${start}")s"
}
