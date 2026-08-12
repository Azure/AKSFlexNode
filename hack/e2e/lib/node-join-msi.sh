#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-msi.sh - Join / unjoin an AKS flex node using MSI auth
#
# Functions:
#   node_join_msi   - Generate MSI config, deploy binary, run agent
#   node_unjoin_msi - Simulate RP delete and verify node cleanup
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
# node_join_msi - Join the MSI VM
# ---------------------------------------------------------------------------
node_join_msi() {
  log_section "Joining MSI Node"
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
      "mode": "in-cluster",
      "endpointUrl": "${E2E_CONTROLLER_SERVICE_PROXY_PATH}"
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

  log_info "Fetching initial AKS RP bootstrap data for the MSI repave scenario..."
  with_cluster_lock az rest \
    --only-show-errors \
    --method post \
    --url "https://management.azure.com${cluster_id}/agentPools/${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}/listBootstrapData?api-version=${E2E_BOOTSTRAP_DATA_API_VERSION}" \
    --output json > "${bootstrap_data_file}"
  chmod 0600 "${bootstrap_data_file}"
  jq -e '.azure.bootstrapToken.token | type == "string" and length > 0' "${bootstrap_data_file}" >/dev/null
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

  # Step 3: Publish the AKS Machine goal and deploy the agent.
  ensure_flex_controller
  machine_configmap_upsert "$(state_get msi_vm_name)" "${E2E_KUBERNETES_VERSION}" "${E2E_KUBERNETES_VERSION}"
  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-msi"

  log_success "MSI node joined in $(timer_elapsed "${start}")s"
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
