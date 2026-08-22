#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/infra.sh - Infrastructure provisioning via Bicep
#
# Functions:
#   infra_deploy   - Deploy AKS cluster and Flex Node VMs via Bicep template
#   infra_get_kubeconfig - Fetch admin kubeconfig for the AKS cluster
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_INFRA_LOADED:-}" ]] && return 0
readonly _E2E_INFRA_LOADED=1

# Ensure common.sh is loaded
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# Resolve the SSH public key to pass into Bicep
# ---------------------------------------------------------------------------
_resolve_ssh_key() {
  local key_file="${E2E_SSH_KEY_FILE:-}"

  # If caller didn't specify, find the first available key
  if [[ -z "${key_file}" ]]; then
    for candidate in "${HOME}/.ssh/id_ed25519.pub" "${HOME}/.ssh/id_rsa.pub" "${HOME}/.ssh/id_ecdsa.pub"; do
      if [[ -f "${candidate}" ]]; then
        key_file="${candidate}"
        break
      fi
    done
  fi

  if [[ -z "${key_file}" || ! -f "${key_file}" ]]; then
    # Generate a temporary keypair for the test run
    key_file="${E2E_WORK_DIR}/e2e_ssh_key.pub"
    if [[ ! -f "${key_file}" ]]; then
      log_info "No SSH key found; generating a temporary keypair..." >&2
      ssh-keygen -t ed25519 -f "${E2E_WORK_DIR}/e2e_ssh_key" -N "" -q
    fi
  fi

  cat "${key_file}"
}

# ---------------------------------------------------------------------------
# infra_deploy - Deploy the Bicep template
# ---------------------------------------------------------------------------
infra_deploy() {
  log_section "Deploying Infrastructure (Bicep)"
  local start
  start=$(timer_start)

  if [[ ! "${E2E_KUBERNETES_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    log_error "E2E_KUBERNETES_VERSION must be an exact x.y.z patch version, got '${E2E_KUBERNETES_VERSION}'"
    return 1
  fi

  local bicep_file="${E2E_INFRA_DIR}/main.bicep"
  if [[ ! -f "${bicep_file}" ]]; then
    log_error "Bicep template not found: ${bicep_file}"
    return 1
  fi

  # Ensure resource group exists
  if ! az group show --name "${E2E_RESOURCE_GROUP}" --output none 2>/dev/null; then
    log_info "Creating resource group: ${E2E_RESOURCE_GROUP} in ${E2E_LOCATION}"
    az group create \
      --name "${E2E_RESOURCE_GROUP}" \
      --location "${E2E_LOCATION}" \
      --tags "purpose=e2e-test" \
      --output none
  else
    log_info "Resource group already exists: ${E2E_RESOURCE_GROUP}"
  fi

  # Resolve SSH key
  local ssh_key
  ssh_key="$(_resolve_ssh_key)"
  configure_ssh_identity

  # Build tags
  local run_id="${GITHUB_RUN_ID:-local-$(date +%s)}"
  local tags_json
  tags_json=$(jq -n \
    --arg run "${run_id}" \
    --arg purpose "e2e-test" \
    '{"github-run": $run, "purpose": $purpose}')

  # Deploy
  log_info "Deploying Bicep template (this may take 5-10 minutes)..."
  local deployment_name="e2e-${E2E_NAME_SUFFIX}"
  az deployment group create \
    --resource-group "${E2E_RESOURCE_GROUP}" \
    --name "${deployment_name}" \
    --template-file "${bicep_file}" \
    --parameters \
      location="${E2E_LOCATION}" \
      nameSuffix="${E2E_NAME_SUFFIX}" \
      aksNodeVmSize="${E2E_AKS_NODE_VM_SIZE}" \
      vmSize="${E2E_VM_SIZE}" \
      kubernetesVersion="${E2E_KUBERNETES_VERSION}" \
      flexAgentPoolName="${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}" \
      sshPublicKey="${ssh_key}" \
      tags="${tags_json}" \
    --output none

  # Extract outputs
  log_info "Extracting deployment outputs..."
  local outputs
  outputs=$(az deployment group show \
    --resource-group "${E2E_RESOURCE_GROUP}" \
    --name "${deployment_name}" \
    --query properties.outputs \
    -o json)

  local cluster_name cluster_id msi_vm_name msi_vm_ip msi_vm_private_ip msi_vm_principal_id
  local token_vm_name token_vm_ip token_vm_private_ip offline_vm_name offline_vm_ip offline_vm_private_ip
  local kubeadm_vm_name kubeadm_vm_ip arc_vm_name arc_vm_ip arc_vm_private_ip admin_username

  cluster_name=$(echo "${outputs}"    | jq -r '.clusterName.value')
  cluster_id=$(echo "${outputs}"      | jq -r '.clusterId.value')
  msi_vm_name=$(echo "${outputs}"     | jq -r '.msiVmName.value')
  msi_vm_ip=$(echo "${outputs}"       | jq -r '.msiVmIp.value')
  msi_vm_private_ip=$(echo "${outputs}" | jq -r '.msiVmPrivateIp.value // ""')
  msi_vm_principal_id=$(echo "${outputs}" | jq -r '.msiVmPrincipalId.value')
  token_vm_name=$(echo "${outputs}"   | jq -r '.tokenVmName.value')
  token_vm_ip=$(echo "${outputs}"     | jq -r '.tokenVmIp.value')
  token_vm_private_ip=$(echo "${outputs}" | jq -r '.tokenVmPrivateIp.value // ""')
  offline_vm_name=$(echo "${outputs}" | jq -r '.offlineVmName.value')
  offline_vm_ip=$(echo "${outputs}"   | jq -r '.offlineVmIp.value')
  offline_vm_private_ip=$(echo "${outputs}" | jq -r '.offlineVmPrivateIp.value // ""')
  kubeadm_vm_name=$(echo "${outputs}" | jq -r '.kubeadmVmName.value')
  kubeadm_vm_ip=$(echo "${outputs}"   | jq -r '.kubeadmVmIp.value')
  arc_vm_name=$(echo "${outputs}"     | jq -r '.arcVmName.value')
  arc_vm_ip=$(echo "${outputs}"       | jq -r '.arcVmIp.value')
  arc_vm_private_ip=$(echo "${outputs}" | jq -r '.arcVmPrivateIp.value // ""')
  admin_username=$(echo "${outputs}"  | jq -r '.adminUsername.value')

  if [[ -z "${msi_vm_private_ip}" ]] || ! is_valid_ipv4 "${msi_vm_private_ip}"; then
    log_error "Missing or invalid MSI VM private IP from deployment outputs: '${msi_vm_private_ip}'"
    return 1
  fi
  if [[ -z "${token_vm_private_ip}" ]] || ! is_valid_ipv4 "${token_vm_private_ip}"; then
    log_error "Missing or invalid token VM private IP from deployment outputs: '${token_vm_private_ip}'"
    return 1
  fi
  if [[ -z "${offline_vm_private_ip}" ]] || ! is_valid_ipv4 "${offline_vm_private_ip}"; then
    log_error "Missing or invalid offline VM private IP from deployment outputs: '${offline_vm_private_ip}'"
    return 1
  fi
  if [[ -z "${arc_vm_private_ip}" ]] || ! is_valid_ipv4 "${arc_vm_private_ip}"; then
    log_error "Missing or invalid Arc VM private IP from deployment outputs: '${arc_vm_private_ip}'"
    return 1
  fi

  # Persist to state
  state_set "cluster_name"          "${cluster_name}"
  state_set "cluster_id"            "${cluster_id}"
  state_set "msi_vm_name"           "${msi_vm_name}"
  state_set "msi_vm_ip"             "${msi_vm_ip}"
  state_set "msi_vm_private_ip"     "${msi_vm_private_ip}"
  state_set "msi_vm_principal_id"   "${msi_vm_principal_id}"
  state_set "token_vm_name"         "${token_vm_name}"
  state_set "token_vm_ip"           "${token_vm_ip}"
  state_set "token_vm_private_ip"   "${token_vm_private_ip}"
  state_set "offline_vm_name"       "${offline_vm_name}"
  state_set "offline_vm_ip"         "${offline_vm_ip}"
  state_set "offline_vm_private_ip" "${offline_vm_private_ip}"
  state_set "kubeadm_vm_name"       "${kubeadm_vm_name}"
  state_set "kubeadm_vm_ip"         "${kubeadm_vm_ip}"
  state_set "arc_vm_name"           "${arc_vm_name}"
  state_set "arc_vm_ip"             "${arc_vm_ip}"
  state_set "arc_vm_private_ip"     "${arc_vm_private_ip}"
  state_set "arc_machine_name"      "${arc_vm_name}-connected"
  state_set "admin_username"        "${admin_username}"
  state_set "resource_group"        "${E2E_RESOURCE_GROUP}"
  state_set "location"              "${E2E_LOCATION}"
  state_set "subscription_id"       "${AZURE_SUBSCRIPTION_ID}"
  state_set "tenant_id"             "${AZURE_TENANT_ID}"
  state_set "deployment_name"       "${deployment_name}"

  log_info "Cluster:     ${cluster_name} (${cluster_id})"
  log_info "MSI VM:      ${msi_vm_name} @ ${msi_vm_ip}"
  log_info "Token VM:    ${token_vm_name} @ ${token_vm_ip}"
  log_info "Offline VM:  ${offline_vm_name} @ ${offline_vm_ip}"
  log_info "Kubeadm VM:  ${kubeadm_vm_name} @ ${kubeadm_vm_ip}"
  log_info "Arc VM:      ${arc_vm_name} @ ${arc_vm_ip}"

  # Get kubeconfig and extract cluster info
  infra_get_kubeconfig

  # Wait for SSH on all VMs (in parallel)
  log_info "Waiting for SSH on all VMs..."
  wait_for_ssh "${msi_vm_ip}" &
  local pid_msi=$!
  wait_for_ssh "${token_vm_ip}" &
  local pid_token=$!
  wait_for_ssh "${offline_vm_ip}" &
  local pid_offline=$!
  wait_for_ssh "${kubeadm_vm_ip}" &
  local pid_kubeadm=$!
  wait_for_ssh "${arc_vm_ip}" &
  local pid_arc=$!

  local ssh_failed=0
  wait "${pid_msi}" || ssh_failed=1
  wait "${pid_token}" || ssh_failed=1
  wait "${pid_offline}" || ssh_failed=1
  wait "${pid_kubeadm}" || ssh_failed=1
  wait "${pid_arc}" || ssh_failed=1

  if [[ "${ssh_failed}" -eq 1 ]]; then
    log_error "One or more VMs not reachable via SSH"
    return 1
  fi

  log_success "Infrastructure deployed in $(timer_elapsed "${start}")s"
}

# ---------------------------------------------------------------------------
# infra_get_kubeconfig - Fetch admin kubeconfig & extract API server info
# ---------------------------------------------------------------------------
infra_get_kubeconfig() {
  local cluster_name
  cluster_name="$(state_get cluster_name)"
  local cluster_id
  cluster_id="$(state_get cluster_id)"
  local resource_group
  resource_group="$(state_get resource_group)"

  local actual_kubernetes_version system_pool_version flex_pool_version
  actual_kubernetes_version="$(az aks show \
    --resource-group "${resource_group}" \
    --name "${cluster_name}" \
    --query 'currentKubernetesVersion || kubernetesVersion' \
    --output tsv)"
  if [[ "${actual_kubernetes_version}" != "${E2E_KUBERNETES_VERSION}" ]]; then
    log_error "AKS control-plane version is ${actual_kubernetes_version}, expected ${E2E_KUBERNETES_VERSION}"
    return 1
  fi
  state_set "kubernetes_version" "${actual_kubernetes_version}"
  log_info "Verified AKS control-plane version: ${actual_kubernetes_version}"

  system_pool_version="$(az rest \
    --method get \
    --url "https://management.azure.com${cluster_id}/agentPools/system?api-version=2026-05-02-preview" \
    --query 'properties.currentOrchestratorVersion || properties.orchestratorVersion' \
    --output tsv)"
  if [[ "${system_pool_version}" != "${E2E_KUBERNETES_VERSION}" ]]; then
    log_error "AKS system pool version is ${system_pool_version}, expected ${E2E_KUBERNETES_VERSION}"
    return 1
  fi
  state_set "system_pool_kubernetes_version" "${system_pool_version}"
  log_info "Verified AKS system pool version: ${system_pool_version}"

  flex_pool_version="$(az rest \
    --method get \
    --url "https://management.azure.com${cluster_id}/agentPools/${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}?api-version=2026-05-02-preview" \
    --query 'properties.currentOrchestratorVersion || properties.orchestratorVersion' \
    --output tsv)"
  if [[ "${flex_pool_version}" != "${E2E_KUBERNETES_VERSION}" ]]; then
    log_error "AKS FlexNodes pool version is ${flex_pool_version}, expected ${E2E_KUBERNETES_VERSION}"
    return 1
  fi
  state_set "flex_pool_kubernetes_version" "${flex_pool_version}"
  log_info "Verified AKS FlexNodes pool version: ${flex_pool_version}"

  log_info "Fetching kubeconfig for ${cluster_name}..."
  az aks get-credentials \
    --resource-group "${resource_group}" \
    --name "${cluster_name}" \
    --overwrite-existing \
    --admin

  # Extract API server URL and CA cert
  local server_url ca_cert_data
  server_url="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  ca_cert_data="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"

  state_set "server_url"   "${server_url}"
  state_set "ca_cert_data" "${ca_cert_data}"

  log_info "API Server: ${server_url}"
}
