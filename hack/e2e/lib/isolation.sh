#!/usr/bin/env bash
# Worker-isolation coverage reuses the runner's token VM, controller and workload checks.
set -euo pipefail

[[ -n "${_E2E_ISOLATION_LOADED:-}" ]] && return 0
readonly _E2E_ISOLATION_LOADED=1

_isolation_snapshot() {
  remote_exec "$1" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
state=/etc/aks-flex-node/daemon-state.json
mode="$(jq -er '.isolation' "${state}")"
slot="$(jq -er '.activeMachine' "${state}")"
settings="$(jq -er '.appliedSettingsVersion' "${state}")"
case "${mode}" in
  none)
    systemctl is-active --quiet unbounded-kubelet.service
    systemctl is-active --quiet unbounded-containerd.service
    version="$(/opt/unbounded/node/current/bin/kubelet --version)"
    location="$(readlink -f /opt/unbounded/node/current)"
    ;;
  container)
    systemctl is-active --quiet "unbounded-node-${slot}.service"
    /opt/unbounded/runtime/runc state "unbounded-${slot}" | jq -e '.status == "running"' >/dev/null
    timeout 20 /opt/unbounded/runtime/runc exec "unbounded-${slot}" systemctl is-active --quiet kubelet.service
    timeout 20 /opt/unbounded/runtime/runc exec "unbounded-${slot}" systemctl is-active --quiet containerd.service
    version="$(timeout 20 /opt/unbounded/runtime/runc exec "unbounded-${slot}" /usr/local/bin/kubelet --version)"
    location="/var/lib/unbounded/containers/${slot}"
    ;;
  *) exit 1 ;;
esac
version="${version##* }"
printf '%s|%s|%s|%s|%s\n' "${mode}" "${slot}" "${version#v}" "${settings}" "${location}"
REMOTE
}

_isolation_wait_runtime() {
  local vm_ip="$1" expected="$2" deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT)) snapshot mode slot version settings location
  while (( SECONDS < deadline )); do
    if snapshot="$(_isolation_snapshot "${vm_ip}" 2>/dev/null)"; then
      IFS='|' read -r mode slot version settings location <<<"${snapshot}"
      if [[ "${mode}" == "${expected}" && "${version}" == "${E2E_KUBERNETES_VERSION#v}" ]]; then
        log_info "Worker ${mode}/${slot} runs Kubernetes ${version} at ${location}, settings ${settings}"
        return 0
      fi
    fi
    sleep 5
  done
  log_error "Worker ${expected} did not become active"
  remote_exec "${vm_ip}" "sudo systemctl --failed --no-pager; sudo journalctl -u aks-flex-node-agent -n 100 --no-pager" || true
  return 1
}

_isolation_credentials() {
  remote_exec "$1" 'sudo sha256sum /var/lib/kubelet/pki/kubelet-client-current.pem' | awk '{print $1}'
}

_isolation_reapply_and_restart() {
  local vm_ip="$1" vm_name="$2" mode="$3" credentials uid operation
  credentials="$(_isolation_credentials "${vm_ip}")"
  uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')"
  remote_exec "${vm_ip}" 'sudo /opt/unbounded/agent/bin/aks-flex-node start --config /etc/aks-flex-node/config.json'
  operation="isolation-${mode}-restart-$(date +%s)-${RANDOM}"
  kubectl apply -f - <<EOF
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: ${operation}
spec:
  machineRef: ${vm_name}
  operationKind: NodeReboot
  ttlSecondsAfterFinished: 3600
EOF
  _agent_upgrade_wait_phase "${operation}" Complete
  _isolation_wait_runtime "${vm_ip}" "${mode}"
  validate_node_joined "${vm_name}"
  [[ "$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')" == "${uid}" ]]
  [[ "$(_isolation_credentials "${vm_ip}")" == "${credentials}" ]]
  smoke_test "${vm_name}" "${mode}-soft-restart"
}

_isolation_host_reboot() {
  local vm_ip="$1" vm_name="$2" mode="$3" before after
  before="$(remote_exec "${vm_ip}" 'cat /proc/sys/kernel/random/boot_id')"
  remote_exec "${vm_ip}" 'sudo systemctl reboot' || true
  local deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
  while (( SECONDS < deadline )); do
    if after="$(remote_exec "${vm_ip}" 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null)" && [[ -n "${after}" && "${after}" != "${before}" ]]; then
      _isolation_wait_runtime "${vm_ip}" "${mode}"
      validate_node_joined "${vm_name}"
      smoke_test "${vm_name}" "${mode}-host-reboot"
      return 0
    fi
    sleep 5
  done
  log_error "Host did not reboot and reconnect"
  return 1
}

_isolation_repave() {
  local vm_ip="$1" vm_name="$2" mode="$3" old_uid old_snapshot old_slot settings snapshot observed slot version applied uid ready
  old_uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')"
  old_snapshot="$(_isolation_snapshot "${vm_ip}")"
  IFS='|' read -r _ old_slot _ _ _ <<<"${old_snapshot}"
  settings="isolation-${mode}-$(date +%s)-${RANDOM}"
  _trigger_mode_repave token "${E2E_KUBERNETES_VERSION}" "${settings}"
  local deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
  while (( SECONDS < deadline )); do
    if snapshot="$(_isolation_snapshot "${vm_ip}" 2>/dev/null)"; then
      IFS='|' read -r observed slot version applied _ <<<"${snapshot}"
      uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
      ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
      if [[ "${observed}" == "${mode}" && "${applied}" == "${settings}" && "${version}" == "${E2E_KUBERNETES_VERSION#v}" && -n "${uid}" && "${uid}" != "${old_uid}" && "${ready}" == True ]]; then
        if [[ "${mode}" == none ]]; then [[ "${slot}" == "${old_slot}" ]]; else [[ "${slot}" != "${old_slot}" ]]; fi
        smoke_test "${vm_name}" "${mode}-repave"
        return 0
      fi
    fi
    sleep 5
  done
  log_error "Repave did not produce the requested settings and a new Ready Node"
  return 1
}

_isolation_assert_clean() {
  local vm_ip="$1" mode="$2"
  remote_exec "${vm_ip}" "sudo bash -s -- ${mode}" <<'REMOTE'
set -euo pipefail
case "$1" in
  none)
    for path in /opt/unbounded/node /etc/systemd/system/unbounded-kubelet.service /etc/systemd/system/unbounded-containerd.service /etc/systemd/system/node-problem-detector.service; do
      [[ ! -e "${path}" && ! -L "${path}" ]] || { echo "Reset left ${path}" >&2; exit 1; }
    done
    ;;
  container)
    for path in /var/lib/unbounded/containers /opt/unbounded/runtime/runc /etc/systemd/system/unbounded-node-kube1.service /etc/systemd/system/unbounded-node-kube2.service; do
      [[ ! -e "${path}" && ! -L "${path}" ]] || { echo "Reset left ${path}" >&2; exit 1; }
    done
    ;;
esac
[[ ! -e /var/lib/kubelet/kubeconfig ]]
[[ ! -e /etc/aks-flex-node/daemon-state.json ]]
[[ -x /opt/unbounded/agent/bin/aks-flex-node ]]
REMOTE
}

isolation_all() {
  local vm_ip vm_name mode joined
  vm_ip="$(state_get token_vm_ip)"
  vm_name="$(state_get token_vm_name)"
  _agent_upgrade_ensure_api
  joined="$(kubectl get node "${vm_name}" --ignore-not-found -o name)"
  if [[ -n "${joined}" ]]; then
    node_unjoin_token
    validate_node_absent "${vm_name}"
  else
    remote_exec "${vm_ip}" 'if [[ -x /opt/unbounded/agent/bin/aks-flex-node ]]; then sudo /opt/unbounded/agent/bin/aks-flex-node reset; fi'
  fi
  for mode in none container; do
    log_section "Worker isolation: ${mode}"
    node_join_token "" "${mode}"
    _isolation_wait_runtime "${vm_ip}" "${mode}"
    validate_node_joined "${vm_name}"
    validate_node_ip "${vm_name}" "$(state_get token_vm_private_ip)"
    smoke_test "${vm_name}" "${mode}-join"
    _isolation_reapply_and_restart "${vm_ip}" "${vm_name}" "${mode}"
    _isolation_host_reboot "${vm_ip}" "${vm_name}" "${mode}"
    _isolation_repave "${vm_ip}" "${vm_name}" "${mode}"
    node_unjoin_token
    _isolation_assert_clean "${vm_ip}" "${mode}"
    validate_node_absent "${vm_name}"
    node_join_token "" "${mode}"
    _isolation_wait_runtime "${vm_ip}" "${mode}"
    validate_node_joined "${vm_name}"
    smoke_test "${vm_name}" "${mode}-rejoin"
    node_unjoin_token
    _isolation_assert_clean "${vm_ip}" "${mode}"
    validate_node_absent "${vm_name}"
  done
  # Preserve the normal runner's final nspawn deployment for log collection.
  node_join_token
  validate_node_joined "${vm_name}"
}
