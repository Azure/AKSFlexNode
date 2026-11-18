#!/usr/bin/env bash
# Worker-isolation coverage reuses the runner's token VM, controller and workload checks.
set -euo pipefail

[[ -n "${_E2E_ISOLATION_LOADED:-}" ]] && return 0
readonly _E2E_ISOLATION_LOADED=1

_isolation_snapshot() {
  remote_exec "$1" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
probe() {
  local name="$1" output rc
  shift
  if output="$("$@" && printf '\034')"; then
    printf '%s' "${output%$'\034'}"
    return 0
  else
    rc=$?
  fi
  printf 'snapshot check %s failed (exit %s)\n' "${name}" "${rc}" >&2
  case "${name}" in
    host-kubelet|host-containerd|supervisor|runc-running|guest-kubelet|guest-containerd|kubelet-version|node-location)
      [[ -z "${output}" ]] || printf 'check stdout (first 4096 bytes): %q\n' "${output:0:4096}" >&2
      ;;
  esac
  return "${rc}"
}
state=/etc/aks-flex-node/daemon-state.json
mode="$(probe isolation jq -er '.isolation' "${state}")"
slot="$(probe activeMachine jq -er '.activeMachine' "${state}")"
case "${slot}" in kube1|kube2) ;; *) echo 'snapshot check activeMachine: invalid slot' >&2; exit 1 ;; esac
settings="$(probe appliedSettingsVersion jq -er '.appliedSettingsVersion' "${state}")"
case "${mode}" in
  none)
    probe host-kubelet systemctl is-active --quiet unbounded-kubelet.service
    probe host-containerd systemctl is-active --quiet unbounded-containerd.service
    version="$(probe kubelet-version /opt/unbounded/node/current/bin/kubelet --version)"
    location="$(probe node-location readlink -f /opt/unbounded/node/current)"
    ;;
  container)
    probe supervisor systemctl is-active --quiet "unbounded-node-${slot}.service"
    runtime_state="$(probe runc-state /opt/unbounded/runtime/runc state "unbounded-${slot}")"
    probe runc-running jq -e '.status == "running"' <<<"${runtime_state}" >/dev/null
    # Foreground runc exec forwards stdin; it must not consume the remote script.
    probe guest-kubelet timeout 20 /opt/unbounded/runtime/runc exec "unbounded-${slot}" systemctl is-active --quiet kubelet.service </dev/null
    probe guest-containerd timeout 20 /opt/unbounded/runtime/runc exec "unbounded-${slot}" systemctl is-active --quiet containerd.service </dev/null
    version="$(probe kubelet-version timeout 20 /opt/unbounded/runtime/runc exec "unbounded-${slot}" /usr/local/bin/kubelet --version </dev/null)"
    location="/var/lib/unbounded/containers/${slot}"
    ;;
  *) echo 'snapshot check isolation: invalid mode' >&2; exit 1 ;;
esac
version="${version##* }"
printf '%s|%s|%s|%s|%s\n' "${mode}" "${slot}" "${version#v}" "${settings}" "${location}"
REMOTE
}

_isolation_runtime_diagnostics() {
  local vm_ip="$1" expected="$2"
  if remote_exec "${vm_ip}" "sudo bash -s -- ${expected}" <<'REMOTE'
set -uo pipefail
state=/etc/aks-flex-node/daemon-state.json
run() {
  local name="$1" rc
  shift
  printf '\n--- %s ---\n' "${name}"
  # Drain excess output so truncation does not replace the command's exit with SIGPIPE.
  timeout -k 2 20 "$@" </dev/null 2>&1 | { head -c 8192; cat >/dev/null; }
  rc=${PIPESTATUS[0]}
  printf '\nexit=%s\n' "${rc}"
}
# Process trees can include arbitrary workload command lines and credentials.
# Journals below are restricted to systemd's own unit events for the same reason.
run_status() {
  local name="$1"
  shift
  run "${name}" bash -o pipefail -c '"$@" 2>&1 | awk '\''/^[[:space:]]*CGroup:/ {hidden=1} !hidden {print}'\''' _ "$@"
}
run_version() {
  local name="$1"
  shift
  run "${name}" bash -c '
    value="$("$@" && printf "\037")"
    rc=$?
    value="${value%$'"'"'\037'"'"'}"
    printf "raw version (first 256 bytes): %q\n" "${value:0:256}"
    exit "${rc}"
  ' _ "$@"
}
# Only string-valued applied metadata is printed; the state also contains credentials.
run applied-metadata jq -c '{isolation,activeMachine,appliedSettingsVersion,appliedKubernetesVersion} | with_entries(.value |= if type == "string" then .[0:256] else null end)' "${state}"
mode="$(timeout -k 2 20 jq -r '.isolation | strings' "${state}" 2>/dev/null)"
slot="$(timeout -k 2 20 jq -r '.activeMachine | strings' "${state}" 2>/dev/null)"
case "${mode}" in none|container) ;; *) mode="$1" ;; esac
case "${mode}" in
  none)
    for unit in unbounded-kubelet.service unbounded-containerd.service; do
      run "${unit} properties" systemctl show "${unit}" --property=ActiveState,SubState,Result,InvocationID,MainPID
      run_status "${unit} status" systemctl status "${unit}" --no-pager -l -n 0
      run "${unit} journal" journalctl -u "${unit}" _PID=1 -n 30 --no-pager
    done
    run_version kubelet-version /opt/unbounded/node/current/bin/kubelet --version
    ;;
  container)
    case "${slot}" in kube1|kube2) slots=("${slot}") ;; *) slots=(kube1 kube2) ;; esac
    for slot in "${slots[@]}"; do
      unit="unbounded-node-${slot}.service"
      id="unbounded-${slot}"
      run "${unit} properties" systemctl show "${unit}" --property=ActiveState,SubState,Result,InvocationID,MainPID
      run_status "${unit} status" systemctl status "${unit}" --no-pager -l -n 0
      run "${unit} journal" journalctl -u "${unit}" _PID=1 -n 30 --no-pager
      # Do not print runc annotations: they are not needed to diagnose runtime state.
      run "${id} state" bash -o pipefail -c '/opt/unbounded/runtime/runc state "$1" | jq -c '\''{id,status,pid}'\''' _ "${id}"
      for unit in kubelet.service containerd.service; do
        run_status "${id} ${unit} status" /opt/unbounded/runtime/runc exec "${id}" systemctl status "${unit}" --no-pager -l -n 0
      done
      run_version "${id} kubelet-version" /opt/unbounded/runtime/runc exec "${id}" /usr/local/bin/kubelet --version
    done
    ;;
esac
run 'host failed units' systemctl --failed --no-pager
run 'agent journal' journalctl -u aks-flex-node-agent.service _PID=1 -n 100 --no-pager
REMOTE
  then
    return 0
  else
    local rc=$?
    log_error "Runtime diagnostics remote execution failed (exit ${rc})"
  fi
}

_isolation_wait_runtime() {
  local vm_ip="$1" expected="$2" deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT)) snapshot mode slot version settings location
  local stderr_file outcome=unattempted rc=0 desired_version="${E2E_KUBERNETES_VERSION#v}"
  stderr_file="$(mktemp)"
  while (( SECONDS < deadline )); do
    if snapshot="$(_isolation_snapshot "${vm_ip}" 2>"${stderr_file}")"; then
      if [[ -z "${snapshot}" ]]; then
        outcome=empty
      else
        outcome=mismatch
        IFS='|' read -r mode slot version settings location <<<"${snapshot}"
        # read only consumes the first line, so reject snapshots with additional lines.
        if [[ "${snapshot}" != *$'\n'* && "${mode}" == "${expected}" && "${version}" == "${desired_version}" ]]; then
          rm -f "${stderr_file}"
          log_info "Worker ${mode}/${slot} runs Kubernetes ${version} at ${location}, settings ${settings}"
          return 0
        fi
      fi
    else
      rc=$?
      outcome=probe-failed
    fi
    sleep 5
  done
  log_error "Worker ${expected} did not become active (last observation: ${outcome})"
  case "${outcome}" in
    empty)
      log_error 'Snapshot exit=0 with empty stdout (0 bytes)'
      ;;
    probe-failed)
      log_error "Snapshot exit=${rc}; last stderr (up to 4096 bytes):"
      tail -c 4096 "${stderr_file}" >&2
      ;;
    mismatch)
      printf 'Snapshot mismatch: expected mode=%q version=%q; observed mode=%q version=%q\n' \
        "${expected:0:256}" "${desired_version:0:256}" "${mode:0:256}" "${version:0:256}" >&2
      ;;
  esac
  rm -f "${stderr_file}"
  _isolation_runtime_diagnostics "${vm_ip}" "${expected}"
  return 1
}

_isolation_credentials() {
  remote_exec "$1" 'sudo sha256sum /var/lib/kubelet/pki/kubelet-client-current.pem' | awk '{print $1}'
}

_isolation_config_digests() {
  remote_exec "$1" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
state=/etc/aks-flex-node/daemon-state.json
config=/var/lib/kubelet/config.yaml
max_pods="$(jq -er '.nodeConfig.Kubelet.Configuration.maxPods | numbers' "${state}")"
# Both modes use the host's shared kubelet configuration.
if ! grep -Eq "^maxPods: ${max_pods}$" "${config}"; then
  echo "Rendered kubelet maxPods differs from applied state (${max_pods})" >&2
  exit 1
fi
state_digest="$(jq -cS '.' "${state}" | sha256sum | awk '{print $1}')"
config_digest="$(sha256sum "${config}" | awk '{print $1}')"
printf '%s|%s\n' "${state_digest}" "${config_digest}"
REMOTE
}

_isolation_assert_unchanged() {
  local vm_ip="$1" vm_name="$2" snapshot="$3" uid="$4" credentials="$5" config="$6" phase="$7"
  local actual_snapshot actual_uid actual_credentials actual_config
  actual_snapshot="$(_isolation_snapshot "${vm_ip}")" || return 1
  actual_uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')" || return 1
  actual_credentials="$(_isolation_credentials "${vm_ip}")" || return 1
  actual_config="$(_isolation_config_digests "${vm_ip}")" || return 1
  if [[ "${actual_snapshot}" != "${snapshot}" || "${actual_uid}" != "${uid}" ||
    "${actual_credentials}" != "${credentials}" || "${actual_config}" != "${config}" ]]; then
    log_error "${phase} changed worker state, Node UID, credentials, or kubelet configuration"
    return 1
  fi
}

_isolation_restart_observation() {
  local vm_ip="$1" mode="$2" slot="$3"
  remote_exec "${vm_ip}" "sudo bash -s -- ${mode} ${slot}" <<'REMOTE'
set -euo pipefail
boot_id="$(cat /proc/sys/kernel/random/boot_id)"
[[ -n "${boot_id}" ]]
case "$1" in
  none)
    first="$(systemctl show unbounded-kubelet.service --property=InvocationID --value)"
    second="$(systemctl show unbounded-containerd.service --property=InvocationID --value)"
    [[ "${first}" =~ ^[0-9a-f]{32}$ && "${first}" != 00000000000000000000000000000000 ]]
    [[ "${second}" =~ ^[0-9a-f]{32}$ && "${second}" != 00000000000000000000000000000000 ]]
    ;;
  container)
    first="$(systemctl show "unbounded-node-$2.service" --property=InvocationID --value)"
    second="$(/opt/unbounded/runtime/runc state "unbounded-$2" | jq -er 'select(.status == "running") | .pid | select(type == "number" and . > 0 and floor == .)')"
    [[ "${first}" =~ ^[0-9a-f]{32}$ && "${first}" != 00000000000000000000000000000000 ]]
    ;;
  *) exit 1 ;;
esac
printf '%s|%s|%s\n' "${boot_id}" "${first}" "${second}"
REMOTE
}

_isolation_reapply_and_restart() {
  local vm_ip="$1" vm_name="$2" mode="$3" stage="$4"
  local credentials uid snapshot config slot before after boot first second new_boot new_first new_second operation
  snapshot="$(_isolation_snapshot "${vm_ip}")"
  IFS='|' read -r _ slot _ _ _ <<<"${snapshot}"
  credentials="$(_isolation_credentials "${vm_ip}")"
  uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')"
  config="$(_isolation_config_digests "${vm_ip}")"
  if [[ -z "${uid}" || -z "${credentials}" ]]; then
    log_error "Missing Node UID or credentials"
    return 1
  fi

  remote_exec "${vm_ip}" 'sudo /opt/unbounded/agent/bin/aks-flex-node start --config /etc/aks-flex-node/config.json'
  _isolation_wait_runtime "${vm_ip}" "${mode}"
  validate_node_joined "${vm_name}"
  _isolation_assert_unchanged "${vm_ip}" "${vm_name}" "${snapshot}" "${uid}" "${credentials}" "${config}" "Bootstrap reapply"

  if [[ "${mode}" == none && "${stage}" == post-repave ]]; then
    # Use a private, temporary input without changing the installed config or auth.
    remote_exec "${vm_ip}" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
input="$(mktemp /etc/aks-flex-node/e2e-reapply.XXXXXX)"
trap 'rm -f "${input}"' EXIT
jq '.node.maxPods += 1' /etc/aks-flex-node/config.json > "${input}"
if /opt/unbounded/agent/bin/aks-flex-node start --config "${input}"; then
  echo "Changed bootstrap input was accepted"
else
  echo "Changed bootstrap input was rejected"
fi
REMOTE
    _isolation_wait_runtime "${vm_ip}" "${mode}"
    validate_node_joined "${vm_name}"
    _isolation_assert_unchanged "${vm_ip}" "${vm_name}" "${snapshot}" "${uid}" "${credentials}" "${config}" "Changed bootstrap input"
  fi

  before="$(_isolation_restart_observation "${vm_ip}" "${mode}" "${slot}")"
  IFS='|' read -r boot first second <<<"${before}"
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
  _isolation_assert_unchanged "${vm_ip}" "${vm_name}" "${snapshot}" "${uid}" "${credentials}" "${config}" "NodeReboot"
  after="$(_isolation_restart_observation "${vm_ip}" "${mode}" "${slot}")"
  IFS='|' read -r new_boot new_first new_second <<<"${after}"
  if [[ "${new_boot}" != "${boot}" || "${new_first}" == "${first}" || "${new_second}" == "${second}" ]]; then
    log_error "NodeReboot did not restart the worker on the same host boot"
    return 1
  fi
  smoke_test "${vm_name}" "${mode}-${stage}-soft-restart"
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
        if [[ "${mode}" == none && "${slot}" != "${old_slot}" ]]; then
          log_error "None repave changed slot"
          return 1
        fi
        if [[ "${mode}" == container && ( "${old_slot}" != kube1 || "${slot}" != kube2 ) ]]; then
          log_error "OCI repave did not move kube1 to kube2"
          return 1
        fi
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
    _isolation_reapply_and_restart "${vm_ip}" "${vm_name}" "${mode}" pre-repave
    _isolation_host_reboot "${vm_ip}" "${vm_name}" "${mode}"
    _isolation_repave "${vm_ip}" "${vm_name}" "${mode}"
    _isolation_reapply_and_restart "${vm_ip}" "${vm_name}" "${mode}" post-repave
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
