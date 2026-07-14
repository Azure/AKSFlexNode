#!/usr/bin/env bash
# Demo helper for triggering a FlexNode soft reboot through MachineOperation.
#
# Usage:
#   hack/demo/aks-flex-node-soft-reboot.sh <node-name>
#
# The script creates a NodeReboot MachineOperation, waits for it to complete,
# waits for the node to become Ready again, and prints the kubelet Rebooted event.

set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
OPERATION_TIMEOUT=${OPERATION_TIMEOUT:-10m}
NODE_READY_TIMEOUT=${NODE_READY_TIMEOUT:-5m}
POLL_INTERVAL=${POLL_INTERVAL:-3}
TTL_SECONDS=${TTL_SECONDS:-300}

usage() {
  cat <<USAGE
Usage: $0 <node-name>

Environment overrides:
  KUBECTL                 kubectl binary to use (default: kubectl)
  OPERATION_TIMEOUT      MachineOperation completion timeout (default: 10m)
  NODE_READY_TIMEOUT     node Ready/boot-id timeout (default: 5m)
  POLL_INTERVAL          polling interval seconds (default: 3)
  TTL_SECONDS            MachineOperation TTL after finish; 0 disables TTL (default: 300)
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

check_machineoperation_api() {
  if ! ${KUBECTL} api-resources --api-group=unbounded-cloud.io | awk '{print $1}' | grep -qx 'machineoperations'; then
    fail "MachineOperation API is not installed. Install the unbounded MachineOperation CRD before running this script."
  fi
}

node_boot_id() {
  local node_name="$1"
  ${KUBECTL} get node "${node_name}" -o 'jsonpath={.status.nodeInfo.bootID}' 2>/dev/null || true
}

node_ready() {
  local node_name="$1"
  ${KUBECTL} get node "${node_name}" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

create_machine_operation() {
  local node_name="$1"
  local operation_name="$2"

  highlight "Creating NodeReboot MachineOperation"
  log "Requesting soft reboot for node ${node_name}."
  cat <<EOF | ${KUBECTL} apply -f - >/dev/null
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: ${operation_name}
spec:
  machineRef: ${node_name}
  operationKind: NodeReboot
  ttlSecondsAfterFinished: ${TTL_SECONDS}
EOF

  success "MachineOperation created: ${operation_name}"
}

wait_for_operation() {
  local operation_name="$1"
  local started_at="$2"
  local deadline
  deadline=$((SECONDS + $(duration_to_seconds "${OPERATION_TIMEOUT}")))

  highlight "Waiting for MachineOperation completion"
  log "Waiting for MachineOperation ${operation_name} to complete."
  while (( SECONDS < deadline )); do
    local phase message
    phase="$(${KUBECTL} get machineoperation "${operation_name}" -o 'jsonpath={.status.phase}' 2>/dev/null || true)"
    message="$(${KUBECTL} get machineoperation "${operation_name}" -o 'jsonpath={.status.message}' 2>/dev/null || true)"

    case "${phase}" in
      Complete)
        success "MachineOperation completed: ${message:-NodeReboot completed}"
        success "MachineOperation completion duration: $(format_duration $((SECONDS - started_at)))"
        return 0
        ;;
      Failed)
        ${KUBECTL} get machineoperation "${operation_name}" -o yaml || true
        fail "MachineOperation ${operation_name} failed: ${message}"
        ;;
      *)
        log "Waiting for soft reboot operation to complete..."
        sleep "${POLL_INTERVAL}"
        ;;
    esac
  done

  ${KUBECTL} get machineoperation "${operation_name}" -o yaml || true
  fail "timed out waiting for MachineOperation ${operation_name}"
}

wait_for_node_reboot() {
  local node_name="$1"
  local before_boot_id="$2"
  local started_at="$3"
  local deadline
  deadline=$((SECONDS + $(duration_to_seconds "${NODE_READY_TIMEOUT}")))

  highlight "Waiting for node reboot"
  log "Waiting for node ${node_name} to report Ready after reboot."
  while (( SECONDS < deadline )); do
    local ready after_boot_id
    ready="$(node_ready "${node_name}")"
    after_boot_id="$(node_boot_id "${node_name}")"

    if [[ "${ready}" == "True" && -n "${after_boot_id}" && "${after_boot_id}" != "${before_boot_id}" ]]; then
      success "Node ${node_name} is Ready after reboot."
      success "bootID: ${before_boot_id} -> ${after_boot_id}"
      success "Soft reboot operation duration: $(format_duration $((SECONDS - started_at)))"
      ${KUBECTL} get node "${node_name}" -o wide
      print_reboot_event "${node_name}" "${after_boot_id}"
      return 0
    fi

    log "Waiting for node reboot event..."
    sleep "${POLL_INTERVAL}"
  done

  ${KUBECTL} get node "${node_name}" -o wide || true
  fail "timed out waiting for ${node_name} to become Ready with a new boot ID"
}

print_reboot_event() {
  local node_name="$1"
  local boot_id="$2"
  local event_json event_line

  event_json="$(${KUBECTL} get events -A \
    --field-selector "involvedObject.kind=Node,involvedObject.name=${node_name},reason=Rebooted" \
    -o json 2>/dev/null || true)"

  if [[ -n "${event_json}" ]]; then
    event_line="$(jq -r --arg boot "${boot_id}" '
      .items[] |
      select((.message // "") | contains($boot)) |
      [
        (.metadata.namespace // ""),
        (.lastTimestamp // .eventTime // .metadata.creationTimestamp // ""),
        (.type // ""),
        (.reason // ""),
        (.message // "")
      ] | @tsv
    ' <<<"${event_json}" | tail -1)"
  else
    event_line=""
  fi

  if [[ -n "${event_line}" ]]; then
    highlight "Node reboot event"
    printf '%s%s%s\n' "${BOLD}${YELLOW}" "${event_line}" "${RESET}"
    return 0
  fi

  warn "Recent node Rebooted events:"
  ${KUBECTL} get events -A \
    --field-selector "involvedObject.kind=Node,involvedObject.name=${node_name},reason=Rebooted" \
    --sort-by=.lastTimestamp | tail -5 || true
}

main() {
  if [[ $# -ne 1 ]]; then
    usage >&2
    exit 2
  fi

  require_command "${KUBECTL}"
  require_command jq
  check_machineoperation_api

  local node_name="$1"
  local before_boot_id operation_name reboot_started_at
  ${KUBECTL} get node "${node_name}" >/dev/null
  before_boot_id="$(node_boot_id "${node_name}")"
  [[ -n "${before_boot_id}" ]] || fail "node ${node_name} does not have a boot ID yet"

  operation_name="${node_name}-soft-reboot-$(date +%s)"

  highlight "Starting AKS Flex Node soft reboot"
  log "node=${node_name}"
  log "current bootID=${before_boot_id}"

  reboot_started_at=${SECONDS}
  create_machine_operation "${node_name}" "${operation_name}"
  wait_for_operation "${operation_name}" "${reboot_started_at}"
  wait_for_node_reboot "${node_name}" "${before_boot_id}" "${reboot_started_at}"
}

main "$@"
