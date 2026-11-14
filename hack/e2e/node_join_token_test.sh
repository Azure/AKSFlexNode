#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/node-join-token.sh
source "${script_dir}/lib/node-join-token.sh"

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
E2E_WORK_DIR="${work}"
E2E_KUBELET_MAX_PODS=58
E2E_KUBELET_SYSTEM_RESERVED_CPU=50m
E2E_KUBELET_SYSTEM_RESERVED_MEMORY=100Mi
E2E_KUBELET_KUBE_RESERVED_CPU=200m
E2E_KUBELET_KUBE_RESERVED_MEMORY=650Mi
E2E_KUBERNETES_VERSION=1.35.0
E2E_CONTAINERD_VERSION=2.0.4
E2E_RUNC_VERSION=1.1.12
E2E_TARGET_AGENT_POOL_NAME=aksflexnodes
E2E_CONTROLLER_SERVICE_PROXY_PATH=/api/v1/namespaces/kube-system/services/https:controller:443/proxy

state_get() {
  case "$1" in
    token_vm_ip) echo 192.0.2.1 ;;
    token_vm_private_ip) echo 10.0.0.5 ;;
    *) printf '%s\n' "$1" ;;
  esac
}
ensure_flex_controller() { echo controller >> "${work}/operations"; }
machine_configmap_upsert() { echo goal >> "${work}/operations"; }
_deploy_and_start_agent() { printf '%s\n' "$4" > "${work}/release"; }
with_cluster_lock() {
  echo cluster >> "${work}/operations"
  if [[ "$1" == mark_e2e_bootstrap_token_aks_managed ]]; then
    return 0
  fi
  shift
  local action="$1"; shift
  if [[ "${action}" == setup-node-rbac ]]; then
    return 0
  fi
  [[ "${action}" == generate-node-config ]]
  local output=""
  while (($#)); do
    if [[ "$1" == --output ]]; then output="$2"; break; fi
    shift
  done
  jq -n '{agent:{}, azure:{bootstrapToken:{token:"abcdef.0123456789abcdef"}}, node:{kubelet:{}}, bootstrap:{}}' > "${output}"
}

for mode in nspawn none container; do
  node_join_token "" "${mode}" > "${work}/join.log"
  jq -e --arg mode "${mode}" '
    .node.kubelet.nodeIP == "10.0.0.5" and
    .node.maxPods == 58 and
    .agent.machineClient.mode == "in-cluster" and
    .agent.requireMachineRegistration == true and
    .components.kubernetes == "1.35.0" and
    (if $mode == "nspawn" then
      (.bootstrap | has("isolation") | not) and
      (.bootstrap | has("experimentalContainerIsolation") | not) and
      (.agent | has("machineReconcileInterval") | not)
    else
      .bootstrap.isolation == $mode and
      .bootstrap.experimentalContainerIsolation == ($mode == "container") and
      .agent.machineReconcileInterval == "5s"
    end)' "${work}/config-token.json" > /dev/null
  [[ ! -s "${work}/release" || "$(<"${work}/release")" == "" ]]
  echo "PASS token config: ${mode}"
done

# Legacy migration and ordinary joins must keep the original config shape.
node_join_token v0.2.0 > "${work}/join.log"
[[ "$(<"${work}/release")" == v0.2.0 ]]
jq -e '(.bootstrap | has("isolation") | not)' "${work}/config-token.json" > /dev/null
node_join_token > "${work}/join.log"
jq -e '(.bootstrap | has("isolation") | not)' "${work}/config-token.json" > /dev/null

for mode in none container invalid; do
  : > "${work}/operations"
  release=v0.2.0
  [[ "${mode}" != invalid ]] || release=""
  if node_join_token "${release}" "${mode}" > "${work}/rejected.log" 2>&1; then
    echo "unexpectedly accepted release=${release} isolation=${mode}" >&2
    exit 1
  fi
  [[ ! -s "${work}/operations" ]]
  echo "PASS rejection before cluster mutation: ${mode}"
done
