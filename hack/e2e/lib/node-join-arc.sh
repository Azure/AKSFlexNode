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

_fetch_arc_bootstrap_config() {
  local vm_ip="$1" vm_private_ip="$2" vm_name="$3" cluster_id="$4" output="$5"
  local remote_binary="/tmp/aks-flex-node-arc-fetch"
  local remote_output="/tmp/aks-flex-node-arc-bootstrap.json"

  remote_copy "${E2E_BINARY}" "${vm_ip}" "${remote_binary}"
  remote_exec "${vm_ip}" "sudo chmod 0755 '${remote_binary}'"

  local fetched=0
  # A newly created Arc principal and role assignment can take several minutes
  # to become effective across ARM frontends.
  for attempt in $(seq 1 60); do
    if remote_exec "${vm_ip}" "sudo '${remote_binary}' fetch-bootstrap-data --auth arc --cluster-resource-id '${cluster_id}' --agent-pool-name '${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}' --output '${remote_output}'"; then
      fetched=1
      break
    fi
    log_info "Arc bootstrap-data fetch not ready; retrying (${attempt}/60)"
    sleep 15
  done
  if [[ "${fetched}" -ne 1 ]]; then
    log_error "Arc identity could not fetch AKS bootstrap data"
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

  if [[ "$(state_get arc_role_assigned)" != "true" ]]; then
    az role assignment create \
      --assignee-object-id "${principal_id}" \
      --assignee-principal-type ServicePrincipal \
      --role "${aksContributorRoleDefinitionID}" \
      --scope "${cluster_id}" \
      --output none
    state_set "arc_role_assigned" "true"
  fi

  local config_file="${E2E_WORK_DIR}/config-arc.json"
  _fetch_arc_bootstrap_config "${vm_ip}" "${vm_private_ip}" "${vm_name}" "${cluster_id}" "${config_file}"

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
