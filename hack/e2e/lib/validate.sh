#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/validate.sh - Node join verification and smoke tests
#
# Functions:
#   validate_node_joined  <vm_name>  - Wait for a specific node to appear in kubectl
#   validate_all_nodes                - Verify MSI, token, offline, kubeadm, and Arc nodes joined
#   validate_kubelet_reservations <vm_name> <vm_ip> [max_pods] [system_cpu] [system_memory] [kube_cpu] [kube_memory]
#                                     - Verify the applied kubelet reservation config
#   validate_npd_status   <vm_name> <vm_ip> - Verify node-problem-detector is active
#   validate_localdns_status <vm_name> <vm_ip> - Verify LocalDNS behavior
#   validate_localdns_after_reboot <vm_name> <vm_ip> - Verify LocalDNS after nspawn reboot
#   validate_node_absent  <vm_name>  - Wait for a node to disappear from kubectl
#   validate_all_nodes_absent         - Verify all flex nodes are gone after unjoin
#   smoke_test            <vm_name> <label>  - Schedule an nginx pod on a node
#   smoke_test_all                    - Run smoke tests on all nodes
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_VALIDATE_LOADED:-}" ]] && return 0
readonly _E2E_VALIDATE_LOADED=1
_E2E_LOCALDNS_REBOOT_VALIDATED=0

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# validate_node_joined - Wait for a node to appear in the cluster
# ---------------------------------------------------------------------------
validate_node_joined() {
  local vm_name="$1"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0

  log_info "Waiting for node '${vm_name}' to join cluster and become Ready (timeout: ${timeout}s)..."

  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local ready
    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    if [[ "${ready}" == "True" ]]; then
      log_success "Node '${vm_name}' joined the cluster and is Ready"
      kubectl get node "${vm_name}" -o wide
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    if [[ -n "${ready}" ]]; then
      log_debug "Waiting for ${vm_name} to become Ready (current Ready=${ready})... (${elapsed}/${timeout}s)"
    else
      log_debug "Waiting for ${vm_name} to register... (${elapsed}/${timeout}s)"
    fi
  done

  log_error "Node '${vm_name}' did not become Ready within ${timeout}s"
  log_error "Current nodes:"
  kubectl get nodes 2>&1 || true
  echo ""
  log_error "Certificate Signing Requests:"
  kubectl get csr 2>&1 || true
  return 1
}

# ---------------------------------------------------------------------------
# validate_node_ip - Ensure node InternalIP matches the expected provisioned IP
# ---------------------------------------------------------------------------
validate_node_ip() {
  local vm_name="$1"
  local expected_ip="$2"

  if [[ -z "${expected_ip}" ]] || ! is_valid_ipv4 "${expected_ip}"; then
    log_error "Expected node IP is empty or invalid for '${vm_name}': '${expected_ip}'"
    return 1
  fi

  local internal_ips
  internal_ips="$(kubectl get node "${vm_name}" -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}')"
  if grep -Fxq "${expected_ip}" <<<"${internal_ips}"; then
    log_success "Node '${vm_name}' InternalIP includes expected IP '${expected_ip}'"
    return 0
  fi

  log_error "Node '${vm_name}' InternalIP mismatch; expected '${expected_ip}'"
  log_error "Observed InternalIP values:"
  echo "${internal_ips}"
  return 1
}

# ---------------------------------------------------------------------------
# quantity_to_milli_cpu - Convert a Kubernetes CPU quantity to millicores
# ---------------------------------------------------------------------------
quantity_to_milli_cpu() {
  local value="$1"

  if [[ "${value}" =~ ^([0-9]+)m$ ]]; then
    echo "${BASH_REMATCH[1]}"
    return 0
  fi
  if [[ "${value}" =~ ^([0-9]+)$ ]]; then
    echo $(( BASH_REMATCH[1] * 1000 ))
    return 0
  fi

  log_error "Unsupported CPU quantity: '${value}'"
  return 1
}

# ---------------------------------------------------------------------------
# quantity_to_bytes - Convert a Kubernetes memory quantity to bytes
# ---------------------------------------------------------------------------
quantity_to_bytes() {
  local value="$1"

  if [[ ! "${value}" =~ ^([0-9]+)(Ki|Mi|Gi|Ti|k|M|G|T)?$ ]]; then
    log_error "Unsupported memory quantity: '${value}'"
    return 1
  fi

  local number="${BASH_REMATCH[1]}"
  case "${BASH_REMATCH[2]:-}" in
    "")  echo "${number}" ;;
    Ki)  echo $(( number * 1024 )) ;;
    Mi)  echo $(( number * 1024 * 1024 )) ;;
    Gi)  echo $(( number * 1024 * 1024 * 1024 )) ;;
    Ti)  echo $(( number * 1024 * 1024 * 1024 * 1024 )) ;;
    k)   echo $(( number * 1000 )) ;;
    M)   echo $(( number * 1000000 )) ;;
    G)   echo $(( number * 1000000000 )) ;;
    T)   echo $(( number * 1000000000000 )) ;;
  esac
}

# ---------------------------------------------------------------------------
# _read_applied_kubelet_config - Print the kubelet configuration applied inside
# the nspawn machine as flattened `key=value` / `section.key=value` lines.
# ---------------------------------------------------------------------------
_read_applied_kubelet_config() {
  local vm_ip="$1"

  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE'
set -euo pipefail
machine=$(machinectl list --no-legend | awk '$1 ~ /^kube[12]$/ {print $1; exit}')
test -n "${machine}"
systemd-run --quiet --pipe --wait --machine="${machine}" cat /var/lib/kubelet/config.yaml | awk '
function trim(value) { gsub(/^[ \t"]+|[ \t"]+$/, "", value); return value }
/^[^[:space:]#-]/ {
  key = $0; sub(/:.*$/, "", key)
  value = $0; sub(/^[^:]*:[ \t]*/, "", value)
  section = key
  if (value != "") { print key "=" trim(value); section = "" }
  next
}
/^  [^ \t#-]/ {
  if (section == "") next
  line = $0; sub(/^  /, "", line)
  key = line; sub(/:.*$/, "", key)
  value = line; sub(/^[^:]*:[ \t]*/, "", value)
  if (value != "") print section "." key "=" trim(value)
}
'
REMOTE
}

_applied_kubelet_value() {
  local applied="$1" key="$2"
  awk -v key="${key}" 'index($0, key "=") == 1 { print substr($0, length(key) + 2); exit }' <<<"${applied}"
}

# ---------------------------------------------------------------------------
# validate_kubelet_reservations - Verify the reservation configuration the agent
# generated is applied by kubelet and reflected in node allocatable capacity.
#
# Expected values are optional. When provided (the overridden-config scenario)
# the applied configuration must match them exactly; otherwise only the
# AKS-compatible defaults are sanity checked.
# ---------------------------------------------------------------------------
validate_kubelet_reservations() {
  local vm_name="$1" vm_ip="$2"
  local expected_max_pods="${3:-}"
  local expected_system_cpu="${4:-}" expected_system_memory="${5:-}"
  local expected_kube_cpu="${6:-}" expected_kube_memory="${7:-}"

  log_info "Validating applied kubelet resource reservations on '${vm_name}'..."

  local applied
  if ! applied="$(_read_applied_kubelet_config "${vm_ip}")"; then
    log_error "Could not read the applied kubelet configuration from '${vm_name}'"
    return 1
  fi

  local max_pods system_cpu system_memory kube_cpu kube_memory
  max_pods="$(_applied_kubelet_value "${applied}" maxPods)"
  system_cpu="$(_applied_kubelet_value "${applied}" systemReserved.cpu)"
  system_memory="$(_applied_kubelet_value "${applied}" systemReserved.memory)"
  kube_cpu="$(_applied_kubelet_value "${applied}" kubeReserved.cpu)"
  kube_memory="$(_applied_kubelet_value "${applied}" kubeReserved.memory)"

  local field
  for field in max_pods system_cpu system_memory kube_cpu kube_memory; do
    if [[ -z "${!field}" ]]; then
      log_error "Applied kubelet configuration on '${vm_name}' is missing ${field}"
      echo "${applied}" >&2
      return 1
    fi
  done

  log_info "Applied kubelet reservations on '${vm_name}': maxPods=${max_pods}" \
    "systemReserved={cpu=${system_cpu}, memory=${system_memory}}" \
    "kubeReserved={cpu=${kube_cpu}, memory=${kube_memory}}"

  local -a expectations=(
    "maxPods:${expected_max_pods}:${max_pods}"
    "systemReserved.cpu:${expected_system_cpu}:${system_cpu}"
    "systemReserved.memory:${expected_system_memory}:${system_memory}"
    "kubeReserved.cpu:${expected_kube_cpu}:${kube_cpu}"
    "kubeReserved.memory:${expected_kube_memory}:${kube_memory}"
  )
  local expectation name expected got failed=0
  for expectation in "${expectations[@]}"; do
    IFS=':' read -r name expected got <<<"${expectation}"
    if [[ -n "${expected}" && "${expected}" != "${got}" ]]; then
      log_error "Applied kubelet ${name} on '${vm_name}' is '${got}', want '${expected}'"
      failed=1
    fi
  done
  if [[ "${failed}" -eq 1 ]]; then
    return 1
  fi

  local system_cpu_milli kube_cpu_milli system_memory_bytes kube_memory_bytes
  system_cpu_milli="$(quantity_to_milli_cpu "${system_cpu}")" || return 1
  kube_cpu_milli="$(quantity_to_milli_cpu "${kube_cpu}")" || return 1
  system_memory_bytes="$(quantity_to_bytes "${system_memory}")" || return 1
  kube_memory_bytes="$(quantity_to_bytes "${kube_memory}")" || return 1

  # The AKS-compatible defaults always reserve resources for Kubernetes daemons.
  if (( kube_cpu_milli <= 0 || kube_memory_bytes <= 0 )); then
    log_error "Applied kubeReserved on '${vm_name}' does not reserve resources: cpu=${kube_cpu}, memory=${kube_memory}"
    return 1
  fi

  local capacity_cpu allocatable_cpu capacity_memory allocatable_memory allocatable_pods
  capacity_cpu="$(kubectl get node "${vm_name}" -o jsonpath='{.status.capacity.cpu}')"
  allocatable_cpu="$(kubectl get node "${vm_name}" -o jsonpath='{.status.allocatable.cpu}')"
  capacity_memory="$(kubectl get node "${vm_name}" -o jsonpath='{.status.capacity.memory}')"
  allocatable_memory="$(kubectl get node "${vm_name}" -o jsonpath='{.status.allocatable.memory}')"
  allocatable_pods="$(kubectl get node "${vm_name}" -o jsonpath='{.status.allocatable.pods}')"

  local capacity_cpu_milli allocatable_cpu_milli capacity_memory_bytes allocatable_memory_bytes
  capacity_cpu_milli="$(quantity_to_milli_cpu "${capacity_cpu}")" || return 1
  allocatable_cpu_milli="$(quantity_to_milli_cpu "${allocatable_cpu}")" || return 1
  capacity_memory_bytes="$(quantity_to_bytes "${capacity_memory}")" || return 1
  allocatable_memory_bytes="$(quantity_to_bytes "${allocatable_memory}")" || return 1

  if [[ "${allocatable_pods}" != "${max_pods}" ]]; then
    log_error "Node '${vm_name}' allocatable pods is '${allocatable_pods}', want '${max_pods}'"
    return 1
  fi

  local expected_allocatable_cpu_milli=$(( capacity_cpu_milli - system_cpu_milli - kube_cpu_milli ))
  if (( allocatable_cpu_milli != expected_allocatable_cpu_milli )); then
    log_error "Node '${vm_name}' allocatable CPU is ${allocatable_cpu_milli}m, want ${expected_allocatable_cpu_milli}m" \
      "(capacity ${capacity_cpu_milli}m minus reservations)"
    return 1
  fi

  # Kubelet also subtracts the hard eviction threshold (100Mi by default) from
  # allocatable memory. Allow a margin above that default so the assertion keeps
  # holding if the threshold changes, while still catching missing reservations.
  local reserved_memory_bytes=$(( system_memory_bytes + kube_memory_bytes ))
  local upper_memory_bytes=$(( capacity_memory_bytes - reserved_memory_bytes ))
  local eviction_allowance_bytes=$(( 256 * 1024 * 1024 ))
  if (( allocatable_memory_bytes > upper_memory_bytes ||
        allocatable_memory_bytes < upper_memory_bytes - eviction_allowance_bytes )); then
    log_error "Node '${vm_name}' allocatable memory is ${allocatable_memory_bytes} bytes," \
      "want at most ${upper_memory_bytes} bytes (capacity ${capacity_memory_bytes} minus reservations)"
    return 1
  fi

  log_success "Applied kubelet resource reservations verified on '${vm_name}'"
}

# ---------------------------------------------------------------------------
# validate_npd_status - Ensure node-problem-detector is active and reporting
# ---------------------------------------------------------------------------
validate_npd_status() {
  local vm_name="$1"
  local vm_ip="$2"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0
  local npd_condition_jsonpath='{.status.conditions[?(@.type=="KernelDeadlock")].status}'
  local condition_error="${E2E_WORK_DIR}/npd-condition-${vm_name}.err"
  local quoted_timeout

  log_info "Validating node-problem-detector on '${vm_name}'..."

  if ! [[ "${timeout}" =~ ^[0-9]+$ ]]; then
    log_error "E2E_NODE_JOIN_TIMEOUT must be numeric, got '${timeout}'"
    return 1
  fi
  printf -v quoted_timeout "%q" "${timeout}"

  remote_exec "${vm_ip}" "E2E_NODE_JOIN_TIMEOUT=${quoted_timeout} bash -s" <<'REMOTE'
set -euo pipefail

deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
active_machine_error="/tmp/aks-flex-node-e2e-active-machine-$$.err"
status_error="/tmp/aks-flex-node-e2e-npd-status-$$.err"
while true; do
  if [[ ! -f /etc/aks-flex-node/daemon-state.json ]]; then
    active_machine=""
    echo "/etc/aks-flex-node/daemon-state.json is missing" > "${active_machine_error}"
  else
    active_machine="$(sudo python3 - <<'PY' 2>"${active_machine_error}" || true
import json
with open("/etc/aks-flex-node/daemon-state.json", encoding="utf-8") as state:
    print(json.load(state).get("activeMachine", ""))
PY
)"
  fi
  if [[ -n "${active_machine}" ]] && sudo machinectl show "${active_machine}" &>/dev/null; then
    status="$(sudo systemd-run --machine="${active_machine}" --quiet --pipe systemctl is-active node-problem-detector.service 2>"${status_error}" || true)"
    if [[ "${status}" == "active" ]]; then
      echo "node-problem-detector.service is active in ${active_machine}"
      exit 0
    fi
  fi

  if (( SECONDS >= deadline )); then
    echo "node-problem-detector.service did not become active"
    if [[ -s "${active_machine_error}" ]]; then
      cat "${active_machine_error}"
    fi
    if [[ -s "${status_error}" ]]; then
      cat "${status_error}"
    fi
    sudo machinectl list --no-pager || true
    if [[ -n "${active_machine:-}" ]]; then
      sudo systemd-run --machine="${active_machine}" --quiet --pipe systemctl status node-problem-detector.service --no-pager -l || true
      sudo systemd-run --machine="${active_machine}" --quiet --pipe journalctl -u node-problem-detector.service -n 50 --no-pager || true
    fi
    exit 1
  fi

  sleep 5
done
REMOTE

  local kernel_deadlock
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    kernel_deadlock="$(kubectl get node "${vm_name}" -o jsonpath="${npd_condition_jsonpath}" 2>"${condition_error}" || true)"
    if [[ "${kernel_deadlock}" == "False" ]]; then
      log_success "node-problem-detector is active and reporting on '${vm_name}'"
      return 0
    fi

    sleep 10
    elapsed=$((elapsed + 10))
    log_debug "Waiting for node-problem-detector condition on ${vm_name}... (${elapsed}/${timeout}s)"
  done

  log_error "node-problem-detector did not report KernelDeadlock=False on '${vm_name}' within ${timeout}s"
  if [[ -s "${condition_error}" ]]; then
    cat "${condition_error}" >&2
  fi
  kubectl describe node "${vm_name}" 2>&1 || true
  return 1
}

# ---------------------------------------------------------------------------
# validate_localdns_status - Verify nspawn LocalDNS on the selected VM.
validate_localdns_status() {
  local vm_name="$1"
  local vm_ip="$2"
  log_info "Validating LocalDNS on ${vm_ip}..."
  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE'
set -euo pipefail
machine=$(sudo machinectl list --no-legend | awk '$1 ~ /^kube[12]$/ {print $1; exit}')
test -n "${machine}"
sudo systemd-run --quiet --pipe --wait --machine="${machine}" systemctl is-active --quiet localdns.service
sudo systemd-run --quiet --pipe --wait --machine="${machine}" \
  grep -qx 'nameserver 169.254.10.10' /etc/unbounded/localdns/resolv.conf
sudo systemd-run --quiet --pipe --wait --machine="${machine}" \
  systemctl cat kubelet.service | grep -q -- '--resolv-conf=/etc/unbounded/localdns/resolv.conf'
sudo ip address show dev localdns | grep -q '169.254.10.10/32'
sudo ip address show dev localdns | grep -q '169.254.10.11/32'
for chain in output prerouting; do
  rules="$(sudo nft list chain ip unbounded_localdns "${chain}")"
  for address in 169.254.10.10 169.254.10.11; do
    for protocol in tcp udp; do
      grep -Fq \
        "ip daddr ${address} ${protocol} dport 53 notrack comment \"unbounded-localdns: skip conntrack\"" \
        <<<"${rules}"
    done
  done
done
REMOTE

  local cluster_pod="localdns-clusterfirst-${vm_name}"
  local default_pod="localdns-default-${vm_name}"
  kubectl delete pod "${cluster_pod}" "${default_pod}" --ignore-not-found --wait=true >/dev/null
  kubectl run "${cluster_pod}" --image=busybox:1.36 --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"${vm_name}\",\"dnsPolicy\":\"ClusterFirst\"}}" \
    --command -- nslookup kubernetes.default.svc.cluster.local >/dev/null
  kubectl run "${default_pod}" --image=busybox:1.36 --restart=Never \
    --overrides="{\"spec\":{\"nodeName\":\"${vm_name}\",\"dnsPolicy\":\"Default\"}}" \
    --command -- nslookup mcr.microsoft.com >/dev/null

  local pod
  for pod in "${cluster_pod}" "${default_pod}"; do
    local phase=""
    for _ in $(seq 1 60); do
      phase="$(kubectl get pod "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
      [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]] && break
      sleep 2
    done
    if [[ "${phase}" != "Succeeded" ]]; then
      kubectl describe pod "${pod}" >&2 || true
      kubectl logs "${pod}" >&2 || true
      log_error "LocalDNS query pod ${pod} ended in phase ${phase}"
      return 1
    fi
  done

  if ! kubectl logs "${cluster_pod}" | grep -Eq 'Server:[[:space:]]*169\.254\.10\.11'; then
    log_error "ClusterFirst pod did not query LocalDNS cluster listener"
    return 1
  fi
  if ! kubectl logs "${default_pod}" | grep -Eq 'Server:[[:space:]]*169\.254\.10\.10'; then
    log_error "Default-policy pod did not query LocalDNS node listener"
    return 1
  fi
  kubectl delete pod "${cluster_pod}" "${default_pod}" --ignore-not-found --wait=false >/dev/null

  log_success "LocalDNS validation passed on ${vm_ip}"
}

# validate_localdns_after_reboot - Verify LocalDNS survives an nspawn reboot.
# ---------------------------------------------------------------------------
validate_localdns_after_reboot() {
  local vm_name="$1"
  local vm_ip="$2"
  local old_renew_time
  old_renew_time="$(kubectl -n kube-node-lease get lease "${vm_name}" -o jsonpath='{.spec.renewTime}')"

  log_section "Validating LocalDNS After Nspawn Reboot"
  log_info "Rebooting the nspawn machine on ${vm_name} (${vm_ip})..."
  remote_exec "${vm_ip}" "sudo bash -s" <<'REMOTE' || return 1
set -euo pipefail
machine=$(machinectl list --no-legend | awk '$1 ~ /^kube[12]$/ {print $1; exit}')
test -n "${machine}"
machinectl reboot "${machine}"
for _ in $(seq 1 60); do
  if systemd-run --quiet --pipe --wait --machine="${machine}" \
      systemctl is-active --quiet localdns.service kubelet.service containerd.service >/dev/null 2>&1; then
    exit 0
  fi
  sleep 5
done
machinectl status "${machine}" >&2 || true
systemd-run --quiet --pipe --wait --machine="${machine}" \
  systemctl --no-pager --full status localdns.service kubelet.service containerd.service >&2 || true
exit 1
REMOTE

  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local ready renew_time
    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    renew_time="$(kubectl -n kube-node-lease get lease "${vm_name}" -o jsonpath='{.spec.renewTime}' 2>/dev/null || true)"
    if [[ "${ready}" == "True" && -n "${renew_time}" && "${renew_time}" != "${old_renew_time}" ]]; then
      validate_localdns_status "${vm_name}" "${vm_ip}" || return 1
      log_success "LocalDNS recovered after nspawn reboot on ${vm_name}"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  log_error "Node ${vm_name} did not renew its lease and become Ready after nspawn reboot"
  kubectl describe node "${vm_name}" >&2 || true
  return 1
}

# validate_all_nodes - Check all MSI, token, offline, kubeadm, and Arc VMs joined
# ---------------------------------------------------------------------------
validate_all_nodes() {
  log_section "Validating Node Join"

  # Re-fetch kubeconfig to be safe
  local cluster_name resource_group
  cluster_name="$(state_get cluster_name)"
  resource_group="$(state_get resource_group)"

  az aks get-credentials \
    --resource-group "${resource_group}" \
    --name "${cluster_name}" \
    --overwrite-existing \
    --admin

  local msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name arc_vm_name
  local msi_vm_ip token_vm_ip offline_vm_ip kubeadm_vm_ip arc_vm_ip
  local msi_vm_private_ip token_vm_private_ip offline_vm_private_ip arc_vm_private_ip
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"
  msi_vm_ip="$(state_get msi_vm_ip)"
  msi_vm_private_ip="$(state_get msi_vm_private_ip)"
  token_vm_ip="$(state_get token_vm_ip)"
  offline_vm_ip="$(state_get offline_vm_ip)"
  kubeadm_vm_ip="$(state_get kubeadm_vm_ip)"
  arc_vm_name="$(state_get arc_vm_name)"
  arc_vm_ip="$(state_get arc_vm_ip)"
  arc_vm_private_ip="$(state_get arc_vm_private_ip)"
  token_vm_private_ip="$(state_get token_vm_private_ip)"
  offline_vm_private_ip="$(state_get offline_vm_private_ip)"

  local failed=0
  validate_node_joined "${msi_vm_name}" || failed=1
  validate_node_joined "${token_vm_name}" || failed=1
  validate_node_joined "${offline_vm_name}" || failed=1
  validate_node_joined "${kubeadm_vm_name}" || failed=1
  validate_node_joined "${arc_vm_name}" || failed=1
  validate_node_ip "${msi_vm_name}" "${msi_vm_private_ip}" || failed=1
  validate_node_ip "${token_vm_name}" "${token_vm_private_ip}" || failed=1
  validate_node_ip "${offline_vm_name}" "${offline_vm_private_ip}" || failed=1
  validate_node_ip "${arc_vm_name}" "${arc_vm_private_ip}" || failed=1
  # The MSI node keeps the AKS-compatible reservation defaults; the token node
  # overrides them through node.maxPods/systemReserved/kubeReserved.
  validate_kubelet_reservations "${msi_vm_name}" "${msi_vm_ip}" || failed=1
  validate_kubelet_reservations "${token_vm_name}" "${token_vm_ip}" \
    "${E2E_KUBELET_MAX_PODS}" \
    "${E2E_KUBELET_SYSTEM_RESERVED_CPU}" "${E2E_KUBELET_SYSTEM_RESERVED_MEMORY}" \
    "${E2E_KUBELET_KUBE_RESERVED_CPU}" "${E2E_KUBELET_KUBE_RESERVED_MEMORY}" || failed=1
  validate_npd_status "${msi_vm_name}" "${msi_vm_ip}" || failed=1
  validate_localdns_status "${msi_vm_name}" "${msi_vm_ip}" || failed=1
  if [[ "${_E2E_LOCALDNS_REBOOT_VALIDATED}" != "1" ]]; then
    if validate_localdns_after_reboot "${msi_vm_name}" "${msi_vm_ip}"; then
      _E2E_LOCALDNS_REBOOT_VALIDATED=1
    else
      failed=1
    fi
  fi
  validate_npd_status "${token_vm_name}" "${token_vm_ip}" || failed=1
  # TODO: re-enable once NPD is included in the upstream Unbounded bootstrap
  # artifact bundle and resolver used by offline artifact mode.
  log_info "Skipping node-problem-detector validation on offline node '${offline_vm_name}'"
  validate_npd_status "${kubeadm_vm_name}" "${kubeadm_vm_ip}" || failed=1
  validate_npd_status "${arc_vm_name}" "${arc_vm_ip}" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more nodes failed to join"
    return 1
  fi

  echo ""
  log_info "All cluster nodes:"
  kubectl get nodes -o wide
  log_success "All nodes verified in cluster"
}

# ---------------------------------------------------------------------------
# validate_node_absent - Wait for a node to disappear from the cluster
# ---------------------------------------------------------------------------
validate_node_absent() {
  local vm_name="$1"
  local timeout="${E2E_NODE_JOIN_TIMEOUT}"
  local elapsed=0

  log_info "Waiting for node '${vm_name}' to leave cluster (timeout: ${timeout}s)..."

  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    if ! kubectl get node "${vm_name}" &>/dev/null; then
      log_success "Node '${vm_name}' is no longer in the cluster"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    log_debug "Waiting for ${vm_name} to disappear... (${elapsed}/${timeout}s)"
  done

  log_error "Node '${vm_name}' still present in cluster after ${timeout}s"
  log_error "Current nodes:"
  kubectl get nodes 2>&1 || true
  return 1
}

# ---------------------------------------------------------------------------
# validate_all_nodes_absent - Check all flex nodes are gone after unjoin
# ---------------------------------------------------------------------------
validate_all_nodes_absent() {
  log_section "Validating Nodes Absent After Unjoin"

  local msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name arc_vm_name
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"
  arc_vm_name="$(state_get arc_vm_name)"

  local failed=0
  # TODO: MSI validation skipped until credential plugin auth is supported
  log_info "Skipping MSI node absence validation (credential plugin auth not yet supported)"
  validate_node_absent "${token_vm_name}" || failed=1
  validate_node_absent "${offline_vm_name}" || failed=1
  validate_node_absent "${kubeadm_vm_name}" || failed=1
  validate_node_absent "${arc_vm_name}" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more nodes still present after unjoin"
    return 1
  fi

  echo ""
  log_info "All cluster nodes:"
  kubectl get nodes -o wide
  log_success "All flex nodes confirmed absent"
}

# ---------------------------------------------------------------------------
# smoke_test - Schedule a pod on a specific node and wait for Ready
# ---------------------------------------------------------------------------
smoke_test() {
  local vm_name="$1"
  local label="$2"
  local pod_name="e2e-smoke-${label}"

  log_info "Smoke test: scheduling '${pod_name}' on node '${vm_name}'..."

  # Create pod manifest
  local manifest="${E2E_WORK_DIR}/${pod_name}.yaml"
  cat > "${manifest}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
spec:
  nodeSelector:
    kubernetes.io/hostname: ${vm_name}
  tolerations:
  - effect: NoSchedule
    operator: Exists
  containers:
  - name: nginx
    image: nginx:alpine
    resources:
      requests:
        memory: "64Mi"
        cpu: "100m"
      limits:
        memory: "128Mi"
        cpu: "200m"
EOF

  kubectl apply -f "${manifest}"

  if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${E2E_POD_READY_TIMEOUT}s"; then
    log_success "Smoke test PASSED for '${pod_name}' on '${vm_name}'"
    kubectl get pod "${pod_name}" -o wide
    kubectl delete pod "${pod_name}" --wait=false
    return 0
  else
    log_error "Smoke test FAILED for '${pod_name}' on '${vm_name}'"
    kubectl describe pod "${pod_name}" 2>&1 || true
    kubectl delete pod "${pod_name}" --wait=false 2>/dev/null || true
    return 1
  fi
}

# ---------------------------------------------------------------------------
# smoke_test_all - Run smoke tests on all nodes
# ---------------------------------------------------------------------------
smoke_test_all() {
  log_section "Running Smoke Tests"

  local msi_vm_name token_vm_name offline_vm_name kubeadm_vm_name arc_vm_name
  msi_vm_name="$(state_get msi_vm_name)"
  token_vm_name="$(state_get token_vm_name)"
  offline_vm_name="$(state_get offline_vm_name)"
  kubeadm_vm_name="$(state_get kubeadm_vm_name)"
  arc_vm_name="$(state_get arc_vm_name)"

  # Unbounded-Net is installed as the E2E CNI, so smoke pods exercise normal pod
  # sandbox networking instead of relying on a synthetic bridge config.
  local failed=0
  smoke_test "${msi_vm_name}" "msi" || failed=1
  smoke_test "${token_vm_name}" "token" || failed=1
  smoke_test "${offline_vm_name}" "offline" || failed=1
  smoke_test "${kubeadm_vm_name}" "kubeadm" || failed=1
  smoke_test "${arc_vm_name}" "arc" || failed=1

  if [[ "${failed}" -eq 1 ]]; then
    log_error "One or more smoke tests failed"
    return 1
  fi

  log_success "All smoke tests passed"
}
