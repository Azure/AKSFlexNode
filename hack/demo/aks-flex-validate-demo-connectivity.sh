#!/usr/bin/env bash
# Validate connectivity and latency for demo-site Flex nodes.
#
# The script creates temporary agnhost netexec pods pinned to each demo node and
# one or more Azure nodes, then tests:
#   - demo node -> Azure node pod connectivity and latency
#   - demo node -> demo node pod connectivity and latency
#
# Usage:
#   hack/demo/aks-flex-validate-demo-connectivity.sh

set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
NAMESPACE=${NAMESPACE:-aks-flex-demo-netcheck}
DEMO_SITE=${DEMO_SITE:-demo}
AZURE_SITE=${AZURE_SITE:-aks-site}
IMAGE=${IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.53}
PING_COUNT=${PING_COUNT:-5}
PING_TIMEOUT=${PING_TIMEOUT:-10}
WAIT_TIMEOUT=${WAIT_TIMEOUT:-180s}
KEEP_PODS=${KEEP_PODS:-false}
SUMMARY_ROWS=()
VALIDATION_FAILURES=0

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

usage() {
  cat <<USAGE
Usage: $0

Environment overrides:
  KUBECTL       kubectl binary to use (default: kubectl)
  NAMESPACE     temporary namespace (default: aks-flex-demo-netcheck)
  DEMO_SITE     demo site label value (default: demo)
  AZURE_SITE    Azure site label value (default: aks-site)
  IMAGE         test image (default: registry.k8s.io/e2e-test-images/agnhost:2.53)
  PING_COUNT    ping count per check (default: 5)
  PING_TIMEOUT  ping deadline seconds per check (default: 10)
  WAIT_TIMEOUT  pod readiness timeout (default: 180s)
  KEEP_PODS     keep temporary pods after the test (default: false)
USAGE
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

sanitize_name() {
  tr '[:upper:]' '[:lower:]' <<<"$1" | sed -E 's/[^a-z0-9-]+/-/g; s/^-+//; s/-+$//' | cut -c1-50
}

node_names_for_site() {
  local site="$1"
  ${KUBECTL} get nodes \
    -l "net.unbounded-cloud.io/site=${site}" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

create_namespace() {
  ${KUBECTL} create namespace "${NAMESPACE}" --dry-run=client -o yaml | ${KUBECTL} apply -f - >/dev/null
}

create_probe_pod() {
  local node_name="$1"
  local pod_name="$2"

  cat <<EOF | ${KUBECTL} apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: aks-flex-demo-netcheck
    aks-flex-demo/node: ${pod_name}
spec:
  restartPolicy: Never
  nodeName: ${node_name}
  tolerations:
    - operator: Exists
  containers:
    - name: netexec
      image: ${IMAGE}
      imagePullPolicy: IfNotPresent
      args:
        - netexec
        - --http-port=8080
        - --udp-port=0
      ports:
        - name: http
          containerPort: 8080
      readinessProbe:
        httpGet:
          path: /hostname
          port: http
        initialDelaySeconds: 1
        periodSeconds: 2
EOF
}

wait_for_pods() {
  highlight "Waiting for connectivity probe pods"
  ${KUBECTL} -n "${NAMESPACE}" wait \
    --for=condition=Ready \
    pod \
    -l app.kubernetes.io/name=aks-flex-demo-netcheck \
    --timeout="${WAIT_TIMEOUT}"
  ${KUBECTL} -n "${NAMESPACE}" get pods -o wide
}

pod_ip() {
  local pod_name="$1"
  ${KUBECTL} -n "${NAMESPACE}" get pod "${pod_name}" -o jsonpath='{.status.podIP}'
}

pod_node() {
  local pod_name="$1"
  ${KUBECTL} -n "${NAMESPACE}" get pod "${pod_name}" -o jsonpath='{.spec.nodeName}'
}

run_ping_check() {
  local source_pod="$1"
  local dest_pod="$2"
  local description="$3"
  local dest_ip output avg source_node dest_node http_status http_output http_error

  source_node="$(pod_node "${source_pod}")"
  dest_node="$(pod_node "${dest_pod}")"
  dest_ip="$(pod_ip "${dest_pod}")"
  [[ -n "${dest_ip}" ]] || fail "destination pod ${dest_pod} does not have a pod IP"

  highlight "${description}"
  log "source=${source_node}/${source_pod} destination=${dest_node}/${dest_pod} ip=${dest_ip}"

  if ! output="$(${KUBECTL} -n "${NAMESPACE}" exec "${source_pod}" -- ping -c "${PING_COUNT}" -w "${PING_TIMEOUT}" "${dest_ip}" 2>&1)"; then
    printf '%s\n' "${output}"
    warn "Ping failed for ${description}"
    SUMMARY_ROWS+=("${description}|${source_node}|${dest_node}|${dest_ip}|failed|SKIPPED")
    VALIDATION_FAILURES=$((VALIDATION_FAILURES + 1))
    return 0
  fi

  printf '%s\n' "${output}" | tail -3
  avg="$(awk -F'= ' '/rtt|round-trip/ {split($2, values, "/"); print values[2]}' <<<"${output}" | tail -1)"
  if [[ -n "${avg}" ]]; then
    success "Average latency: ${avg} ms"
  else
    success "Ping succeeded"
  fi

  http_output="$(mktemp)"
  http_error="$(mktemp)"
  if ${KUBECTL} -n "${NAMESPACE}" exec "${source_pod}" -- wget -qO- --timeout=3 "http://${dest_ip}:8080/hostname" >"${http_output}" 2>"${http_error}"; then
    http_status="OK"
    success "HTTP connectivity OK: $(cat "${http_output}")"
  else
    http_status="FAILED"
    warn "HTTP connectivity check failed: $(cat "${http_error}")"
  fi
  rm -f "${http_output}" "${http_error}"

  SUMMARY_ROWS+=("${description}|${source_node}|${dest_node}|${dest_ip}|${avg:-n/a}|${http_status}")
}

print_summary() {
  highlight "Connectivity validation summary"
  printf '%-46s %-32s %-32s %-15s %-14s %-8s\n' "Check" "Source node" "Destination node" "Dest pod IP" "Ping avg" "HTTP"
  printf '%-46s %-32s %-32s %-15s %-14s %-8s\n' "-----" "-----------" "----------------" "-----------" "--------" "----"

  local row description source_node dest_node dest_ip avg http_status
  for row in "${SUMMARY_ROWS[@]}"; do
    IFS='|' read -r description source_node dest_node dest_ip avg http_status <<<"${row}"
    if [[ "${avg}" != "n/a" && "${avg}" != "failed" ]]; then
      avg="${avg} ms"
    fi
    printf '%-46s %-32s %-32s %-15s %-14s %-8s\n' "${description}" "${source_node}" "${dest_node}" "${dest_ip}" "${avg}" "${http_status}"
  done
}

cleanup() {
  if [[ "${KEEP_PODS}" == "true" ]]; then
    warn "KEEP_PODS=true; leaving namespace ${NAMESPACE} in place."
    return
  fi
  ${KUBECTL} delete namespace "${NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
}

main() {
  if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
  fi
  if [[ $# -ne 0 ]]; then
    usage >&2
    exit 2
  fi

  require_command "${KUBECTL}"
  require_command awk
  trap cleanup EXIT

  mapfile -t demo_nodes < <(node_names_for_site "${DEMO_SITE}")
  mapfile -t azure_nodes < <(node_names_for_site "${AZURE_SITE}")

  (( ${#demo_nodes[@]} >= 2 )) || fail "expected at least two nodes with net.unbounded-cloud.io/site=${DEMO_SITE}; found ${#demo_nodes[@]}"
  (( ${#azure_nodes[@]} >= 1 )) || fail "expected at least one node with net.unbounded-cloud.io/site=${AZURE_SITE}; found ${#azure_nodes[@]}"

  highlight "Demo connectivity validation"
  log "demoSite=${DEMO_SITE} demoNodes=${demo_nodes[*]}"
  log "azureSite=${AZURE_SITE} azureNodes=${azure_nodes[*]}"

  create_namespace

  declare -A pod_for_node=()
  local node pod
  for node in "${demo_nodes[@]}" "${azure_nodes[@]:0:1}"; do
    pod="netcheck-$(sanitize_name "${node}")"
    pod_for_node["${node}"]="${pod}"
    create_probe_pod "${node}" "${pod}"
  done

  wait_for_pods

  local first_demo second_demo first_azure
  first_demo="${demo_nodes[0]}"
  second_demo="${demo_nodes[1]}"
  first_azure="${azure_nodes[0]}"

  run_ping_check "${pod_for_node[${first_demo}]}" "${pod_for_node[${first_azure}]}" "Demo node to Azure node connectivity and latency"
  run_ping_check "${pod_for_node[${first_demo}]}" "${pod_for_node[${second_demo}]}" "Demo node to demo node connectivity and latency"
  run_ping_check "${pod_for_node[${second_demo}]}" "${pod_for_node[${first_demo}]}" "Reverse demo node to demo node connectivity and latency"

  print_summary
  if (( VALIDATION_FAILURES > 0 )); then
    fail "Demo connectivity validation completed with ${VALIDATION_FAILURES} failed check(s)."
  fi
  success "Demo connectivity validation complete."
}

main "$@"
