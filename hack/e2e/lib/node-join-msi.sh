#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-msi.sh - Join / unjoin an AKS flex node using MSI auth
#
# Functions:
#   node_join_msi                  - Join through the in-cluster Machine endpoint
#   node_join_msi_arm_registration - Join through the ARM Machine endpoint
#   node_unjoin_msi                - Simulate RP delete and verify node cleanup
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_MSI_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_MSI_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# prepare_localdns_host_resolver - Remove DHCP search/routing domains.
#
# Unbounded intentionally rejects systemd-resolved split-DNS layouts because
# flattening per-domain routing into one LocalDNS upstream list changes DNS
# semantics. Azure's Ubuntu image supplies a DHCP search domain by default, so
# put the disposable E2E host into the supported single-upstream layout before
# LocalDNS preflight runs.
# ---------------------------------------------------------------------------
prepare_localdns_host_resolver() {
  local vm_ip="$1"

  log_info "Configuring the MSI host resolver for LocalDNS..."
  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE'
set -euo pipefail

interface="$(ip -4 route show default | awk 'NR == 1 { print $5 }')"
if [[ ! "${interface}" =~ ^[a-zA-Z0-9_.:-]+$ ]]; then
  echo "could not determine a safe primary interface name: ${interface}" >&2
  exit 1
fi

cat > /etc/netplan/99-aks-flex-localdns.yaml <<EOF
network:
  version: 2
  ethernets:
    ${interface}:
      dhcp4-overrides:
        use-domains: false
EOF
chmod 0600 /etc/netplan/99-aks-flex-localdns.yaml
netplan generate
netplan apply

# Match Unbounded's supported-domain rule: no domain is allowed except the
# catch-all routing domain (~.).
if resolvectl domain | awk -F: '
  {
    for (i = 2; i <= NF; i++) {
      count = split($i, domains, /[[:space:]]+/)
      for (j = 1; j <= count; j++)
        if (domains[j] != "" && domains[j] != "~.")
          found = 1
    }
  }
  END { exit !found }
'; then
  echo "unsupported systemd-resolved domain remains after netplan apply:" >&2
  resolvectl domain >&2
  exit 1
fi

resolvectl domain
resolvectl dns "${interface}"
REMOTE
}

# ---------------------------------------------------------------------------
# _node_join_msi - Join the MSI VM with the requested Machine backend.
# ---------------------------------------------------------------------------
_node_join_msi() {
  local machine_client_mode="$1"
  local node_name_override="$2"
  local bootstrap_unit="$3"

  case "${machine_client_mode}" in
    arm|in-cluster) ;;
    *)
      log_error "Unsupported MSI Machine client mode: ${machine_client_mode}"
      return 1
      ;;
  esac

  log_section "Joining MSI Node (${machine_client_mode} Machine client)"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get msi_vm_ip)"
  local vm_private_ip
  vm_private_ip="$(state_get msi_vm_private_ip)"
  local cluster_id
  cluster_id="$(state_get cluster_id)"
  local subscription_id
  subscription_id="$(state_get subscription_id)"
  local tenant_id
  tenant_id="$(state_get tenant_id)"
  local location
  location="$(state_get location)"
  local server_url
  server_url="$(state_get server_url)"
  local ca_cert_data
  ca_cert_data="$(state_get ca_cert_data)"

  # Step 1: Generate a dual-auth operator-first config. The initial token comes
  # from the same RP action that repave must refresh, while MSI remains available
  # to the host daemon for subsequent listBootstrapData calls.
  local config_file="${E2E_WORK_DIR}/config-msi.json"
  local bootstrap_data_file="${E2E_WORK_DIR}/bootstrap-data-msi.json"
  cat > "${config_file}" <<EOF
{
  "azure": {
    "subscriptionId": "${subscription_id}",
    "tenantId": "${tenant_id}",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}",
    "managedIdentity": {},
    "targetCluster": {
      "resourceId": "${cluster_id}",
      "location": "${location}"
    }
  },
  "node": {
    "kubelet": {
      "clusterFQDN": "${server_url}",
      "caCertData": "${ca_cert_data}",
      "nodeIP": "${vm_private_ip}"
    }
  },
  "agent": {
    "logLevel": "debug",
    "logDir": "/var/log/aks-flex-node",
    "machineClient": {
      "mode": "arm"
    },
    "requireMachineRegistration": true
  },
  "networking": {
    "localDNS": {
      "mode": "Required",
      "vnetDNSOverrides": {
        ".": {
          "queryLogging": "Error",
          "protocol": "PreferUDP",
          "forwardDestination": "VnetDNS",
          "forwardPolicy": "Sequential",
          "maxConcurrent": 1000,
          "cacheDurationInSeconds": 3600,
          "serveStaleDurationInSeconds": 3600,
          "serveStale": "Immediate"
        }
      },
      "kubeDNSOverrides": {
        ".": {
          "queryLogging": "Error",
          "protocol": "ForceTCP",
          "forwardDestination": "ClusterCoreDNS",
          "forwardPolicy": "Sequential",
          "maxConcurrent": 1000,
          "cacheDurationInSeconds": 3600,
          "serveStaleDurationInSeconds": 3600,
          "serveStale": "Immediate"
        }
      }
    }
  },
  "components": {
    "kubernetes": "${E2E_KUBERNETES_VERSION}",
    "containerd": "${E2E_CONTAINERD_VERSION}",
    "runc": "${E2E_RUNC_VERSION}"
  }
}
EOF

  jq \
    --arg machineClientMode "${machine_client_mode}" \
    --arg machineEndpointURL "${E2E_CONTROLLER_SERVICE_PROXY_PATH}" \
    --arg nodeName "${node_name_override}" \
    '.agent.machineClient = if $machineClientMode == "in-cluster" then
        {"mode": "in-cluster", "endpointUrl": $machineEndpointURL}
      else
        {"mode": "arm"}
      end
      | if $nodeName != "" then .agent.nodeName = $nodeName else . end' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"

  log_info "Fetching initial AKS RP bootstrap data for the MSI node..."
  install -m 0600 /dev/null "${bootstrap_data_file}"
  with_cluster_lock az rest \
    --only-show-errors \
    --method post \
    --url "https://management.azure.com${cluster_id}/agentPools/${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}/listBootstrapData?api-version=2026-05-02-preview" \
    --output json > "${bootstrap_data_file}"
  jq -e '.azure.bootstrapToken.token | type == "string" and length > 0' "${bootstrap_data_file}" >/dev/null
  install -m 0600 /dev/null "${config_file}.tmp"
  jq --slurpfile bootstrapData "${bootstrap_data_file}" '
    .azure.bootstrapToken = $bootstrapData[0].azure.bootstrapToken
    | .node.kubelet.clusterFQDN = ($bootstrapData[0].node.kubelet.clusterFQDN // .node.kubelet.clusterFQDN)
    | .node.kubelet.caCertData = ($bootstrapData[0].node.kubelet.caCertData // .node.kubelet.caCertData)
  ' "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"
  chmod 0600 "${config_file}"
  rm -f "${bootstrap_data_file}"

  # Step 2: Put systemd-resolved into the layout supported by LocalDNS.
  prepare_localdns_host_resolver "${vm_ip}"

  # Step 3: The controller-backed mode needs a pre-created Machine fixture.
  # ARM mode intentionally starts without one so EnsureMachine registers it.
  if [[ "${machine_client_mode}" == "in-cluster" ]]; then
    ensure_flex_controller
    machine_configmap_upsert "$(state_get msi_vm_name)" "${E2E_KUBERNETES_VERSION}" "${E2E_KUBERNETES_VERSION}"
  fi
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "${bootstrap_unit}"

  log_success "MSI node joined with ${machine_client_mode} Machine client in $(timer_elapsed "${start}")s"
}

node_join_msi() {
  _node_join_msi "in-cluster" "" "aks-flex-node-msi"
}

node_join_msi_arm_registration() {
  local machine_name="$1"
  _node_join_msi "arm" "${machine_name}" "aks-flex-node-msi-arm-registration"
}

# ---------------------------------------------------------------------------
# node_unjoin_msi - Simulate RP delete and verify node cleanup
# ---------------------------------------------------------------------------
node_unjoin_msi() {
  log_section "Unjoining MSI Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get msi_vm_ip)"
  vm_name="$(state_get msi_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  log_success "MSI node unjoined in $(timer_elapsed "${start}")s"
}
