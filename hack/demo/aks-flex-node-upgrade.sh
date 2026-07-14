#!/usr/bin/env bash
# Demo helper for the AKS Flex Node in-cluster machine endpoint upgrade flow.
#
# Usage:
#   hack/demo/aks-flex-node-upgrade.sh <node-name> <target-kubernetes-version>
#
# The script simulates the AKS RP upgrade orchestration used by FlexNode:
#   1. Write the desired Machine goal to the controller ConfigMap.
#   2. Cordon and drain the Kubernetes Node.
#   3. Ask for confirmation, then delete the Node to trigger daemon repave.
#   4. Wait for the Node to rejoin Ready with the target kubelet version.

set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
MACHINE_CONFIGMAP_NAMESPACE=${MACHINE_CONFIGMAP_NAMESPACE:-kube-system}
MACHINE_CONFIGMAP_NAME=${MACHINE_CONFIGMAP_NAME:-aks-flex-machines}
DRAIN_TIMEOUT=${DRAIN_TIMEOUT:-10m}
JOIN_TIMEOUT=${JOIN_TIMEOUT:-20m}
POLL_INTERVAL=${POLL_INTERVAL:-5}

usage() {
  cat <<USAGE
Usage: $0 <node-name> <target-kubernetes-version>

Environment overrides:
  KUBECTL                         kubectl binary to use (default: kubectl)
  MACHINE_CONFIGMAP_NAMESPACE     machine ConfigMap namespace (default: kube-system)
  MACHINE_CONFIGMAP_NAME          machine ConfigMap name (default: aks-flex-machines)
  DRAIN_TIMEOUT                   kubectl drain timeout (default: 10m)
  JOIN_TIMEOUT                    node rejoin timeout (default: 20m)
  POLL_INTERVAL                   node watch poll interval seconds (default: 5)
USAGE
}

if [[ -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\033[1m'
  CYAN=$'\033[36m'
  GREEN=$'\033[32m'
  YELLOW=$'\033[33m'
  RED=$'\033[31m'
  RESET=$'\033[0m'
else
  BOLD=""
  CYAN=""
  GREEN=""
  YELLOW=""
  RED=""
  RESET=""
fi

log() {
  printf '%s\n' "$*"
}

highlight() {
  printf '\n%s%s%s\n' "${BOLD}${CYAN}" "$*" "${RESET}"
}

success() {
  printf '%s%s%s\n' "${BOLD}${GREEN}" "$*" "${RESET}"
}

warn() {
  printf '%s%s%s\n' "${BOLD}${YELLOW}" "$*" "${RESET}"
}

fail() {
  printf '%sERROR: %s%s\n' "${BOLD}${RED}" "$*" "${RESET}" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

normalize_plain_version() {
  local version="$1"
  version="${version#v}"
  [[ -n "${version}" ]] || fail "target version is empty"
  printf '%s' "${version}"
}

normalize_kubelet_version() {
  local version="$1"
  version="${version#v}"
  printf 'v%s' "${version}"
}

confirm_delete() {
  local node_name="$1"
  printf '\n'
  success "Workloads on node ${node_name} have been evicted."
  warn "Deleting the Kubernetes Node is the signal that allows the FlexNode daemon to start the upgrade."
  read -r -p "Delete node ${node_name} now to trigger the upgrade operation? [y/N] " answer
  case "${answer}" in
    y|Y|yes|YES) return 0 ;;
    *) fail "upgrade trigger cancelled by user" ;;
  esac
}

update_machine_goal() {
  local node_name="$1"
  local target_version="$2"
  local settings_version="$3"

  highlight "Sending upgrade request to AKS RP"
  log "node=${node_name} kubernetesVersion=${target_version} settingsVersion=${settings_version}"
  local current_json cm_json
  cm_json="$(${KUBECTL} -n "${MACHINE_CONFIGMAP_NAMESPACE}" get configmap "${MACHINE_CONFIGMAP_NAME}" -o json 2>/dev/null || true)"
  if [[ -n "${cm_json}" ]]; then
    current_json="$(jq -r --arg key "${node_name}.json" --arg legacy "${node_name}" '.data[$key] // .data[$legacy] // empty' <<<"${cm_json}")"
  else
    current_json=""
  fi
  if [[ -z "${current_json}" ]]; then
    log "No existing Machine entry found for ${node_name}; creating a minimal entry."
    current_json='{}'
  fi

  local tmp
  tmp="$(mktemp)"
  jq \
    --arg node "${node_name}" \
    --arg version "${target_version}" \
    --arg settings "${settings_version}" \
    '
      .id = (.id // ("configmap://" + $node)) |
      .name = $node |
      .properties = (.properties // {}) |
      .properties.eTag = $settings |
      .properties.kubernetes = (.properties.kubernetes // {}) |
      .properties.kubernetes.orchestratorVersion = $version |
      .properties.kubernetes.nodeLabels = (.properties.kubernetes.nodeLabels // {"kubernetes.azure.com/managed":"false"})
    ' <<<"${current_json}" > "${tmp}"

  if [[ -z "${cm_json}" ]]; then
    ${KUBECTL} -n "${MACHINE_CONFIGMAP_NAMESPACE}" create configmap "${MACHINE_CONFIGMAP_NAME}" \
      --dry-run=client -o yaml | ${KUBECTL} apply -f - >/dev/null
  fi

  local patch
  patch="$(jq -n --arg key "${node_name}.json" --rawfile machine "${tmp}" '{data: {($key): $machine}}')"
  ${KUBECTL} -n "${MACHINE_CONFIGMAP_NAMESPACE}" patch configmap "${MACHINE_CONFIGMAP_NAME}" \
    --type merge \
    --patch "${patch}" >/dev/null

  rm -f "${tmp}"
  success "Upgrade request accepted. Machine goal now served by the in-cluster endpoint:"
  ${KUBECTL} get --raw \
    "/api/v1/namespaces/${MACHINE_CONFIGMAP_NAMESPACE}/services/http:aks-flex-controller:80/proxy/machines/${node_name}" | jq .
}

cordon_and_drain() {
  local node_name="$1"
  highlight "Preparing node ${node_name} for upgrade"
  log "Cordoning node ${node_name}."
  ${KUBECTL} cordon "${node_name}"

  log "Draining node ${node_name}; this may take a few minutes."
  ${KUBECTL} drain "${node_name}" \
    --ignore-daemonsets \
    --delete-emptydir-data \
    --force \
    --timeout="${DRAIN_TIMEOUT}"
  success "Drain complete for node ${node_name}."
}

wait_for_upgrade() {
  local node_name="$1"
  local target_kubelet_version="$2"
  local old_node_uid="$3"
  local started_at="$4"
  local deadline
  deadline=$((SECONDS + $(duration_to_seconds "${JOIN_TIMEOUT}")))

  highlight "Waiting for upgraded node to rejoin"
  log "Waiting for node ${node_name} to re-register, become Ready, and become schedulable with kubelet ${target_kubelet_version}."
  while (( SECONDS < deadline )); do
    local ready version node_uid unschedulable
    ready="$(${KUBECTL} get node "${node_name}" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    version="$(${KUBECTL} get node "${node_name}" -o 'jsonpath={.status.nodeInfo.kubeletVersion}' 2>/dev/null || true)"
    node_uid="$(${KUBECTL} get node "${node_name}" -o 'jsonpath={.metadata.uid}' 2>/dev/null || true)"
    unschedulable="$(${KUBECTL} get node "${node_name}" -o 'jsonpath={.spec.unschedulable}' 2>/dev/null || true)"

    log "Waiting for upgraded node ${node_name} to become Ready and schedulable..."
    if [[ -n "${node_uid}" && "${node_uid}" != "${old_node_uid}" && "${ready}" == "True" && "${version}" == "${target_kubelet_version}" && "${unschedulable}" != "true" ]]; then
      success "Upgrade complete: node ${node_name} is Ready and schedulable with kubelet ${version}."
      success "Upgrade operation duration: $(format_duration $((SECONDS - started_at)))"
      ${KUBECTL} get node "${node_name}" -o wide
      return 0
    fi
    sleep "${POLL_INTERVAL}"
  done

  ${KUBECTL} get node "${node_name}" -o wide || true
  fail "timed out waiting for ${node_name} to re-register as Ready and schedulable with kubelet ${target_kubelet_version}"
}

duration_to_seconds() {
  local duration="$1"
  case "${duration}" in
    *s) printf '%s' "${duration%s}" ;;
    *m) printf '%s' "$(( ${duration%m} * 60 ))" ;;
    *h) printf '%s' "$(( ${duration%h} * 3600 ))" ;;
    *[!0-9]*) fail "unsupported duration ${duration}; use seconds, Nm, or Nh" ;;
    *) printf '%s' "${duration}" ;;
  esac
}

format_duration() {
  local total_seconds="$1"
  local minutes seconds
  minutes=$((total_seconds / 60))
  seconds=$((total_seconds % 60))
  if (( minutes > 0 )); then
    printf '%dm%02ds' "${minutes}" "${seconds}"
  else
    printf '%ds' "${seconds}"
  fi
}

main() {
  if [[ $# -ne 2 ]]; then
    usage >&2
    exit 2
  fi

  require_command "${KUBECTL}"
  require_command jq

  local node_name="$1"
  local target_version target_kubelet_version settings_version old_node_uid upgrade_started_at
  target_version="$(normalize_plain_version "$2")"
  target_kubelet_version="$(normalize_kubelet_version "${target_version}")"
  settings_version="${target_version}-upgrade-$(date +%Y%m%d%H%M%S)"

  highlight "Starting AKS Flex Node upgrade"
  log "node=${node_name} targetVersion=${target_version}"

  old_node_uid="$(${KUBECTL} get node "${node_name}" -o 'jsonpath={.metadata.uid}')"
  [[ -n "${old_node_uid}" ]] || fail "could not read current UID for node ${node_name}"
  update_machine_goal "${node_name}" "${target_version}" "${settings_version}"
  cordon_and_drain "${node_name}"
  confirm_delete "${node_name}"

  highlight "Triggering the upgrade operation"
  log "Deleting node ${node_name} to trigger the FlexNode daemon upgrade operation."
  upgrade_started_at=${SECONDS}
  ${KUBECTL} delete node "${node_name}"
  wait_for_upgrade "${node_name}" "${target_kubelet_version}" "${old_node_uid}" "${upgrade_started_at}"
}

main "$@"
