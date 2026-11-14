#!/usr/bin/env bash
# Offline contracts: all VM/cluster effects are file-backed stubs, never root/cloud.
set -euo pipefail
# shellcheck source=lib/isolation-modes.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/isolation-modes.sh"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
export E2E_LOG_DIR="${work}/logs" E2E_BINARY="${work}/binary"
E2E_KUBERNETES_VERSION=1.35.0
E2E_NODE_JOIN_TIMEOUT=300
E2E_POD_READY_TIMEOUT=120

record() { printf '%s\n' "$*" >>"${case_dir}/trace"; }
fail() { echo "FAIL: $*" >&2; return 1; }
assert_contains() { grep -Eq "$1" "${case_dir}/trace" || fail "missing trace: $1"; }
assert_absent() { if grep -Eq "$1" "${case_dir}/trace"; then fail "unexpected trace: $1"; fi; }
reset_fixture() {
  case_dir="${work}/${1}"
  mkdir -p "${case_dir}"
  : >"${case_dir}/trace"
  echo 0 >"${case_dir}/counter"
  echo 10000 >"${case_dir}/epoch"
  echo null >"${case_dir}/node"
  echo null >"${case_dir}/runtime"
  echo null >"${case_dir}/goal"
  unset FAIL_EXEC FAIL_RESET FAIL_VALIDATE STALE_GOAL EARLY_REPAVE STUCK_GOAL REBOOT_DELAY LOSE_SENTINEL REUSE_INNER_PIDS STALE_EPOCH FAIL_CLEANUP NAMESPACE_DELAY
}

state_get() {
  case "$1" in token_vm_name) echo token-vm ;; token_vm_ip) echo 192.0.2.10 ;;
    token_vm_private_ip) echo 10.0.0.10 ;; *) fail "unexpected state key $1" ;; esac
}
# Any accidental infrastructure path fails immediately, even in a successful test.
az() { record FORBIDDEN_AZ; return 90; }
infra_deploy() { record FORBIDDEN_INFRA; return 90; }
remote_copy() { record "copy $2 $3"; }
remote_exec() {
  record "remote $*"
  case "$2" in
    *'os.path.exists'*)
      if [[ "$(<"${case_dir}/runtime")" == null ]]; then echo absent; else echo present; fi ;;
    *' reset')
      local count
      count="$(grep -c ' reset$' "${case_dir}/trace")"
      if [[ "${FAIL_RESET:-}" == "${count}" ]]; then return 42; fi
      echo null >"${case_dir}/runtime"
      if [[ "${LOSE_SENTINEL:-0}" == 1 && "$2" == *'/opt/unbounded/agent/bin/'* ]]; then
        find "${case_dir}/host" -name '*.aks-flex-node-e2e-preserve' -delete
      fi ;;
    *'timeout 30 python3 - '*)
      local owner action
      read -r owner action <<<"${2#*python3 - }"
      owner="${owner//\'/}"; action="${action//\'/}"
      record "sentinel ${action}"
      printf '%s\n' "$(</dev/stdin)" >"${case_dir}/sentinel.py"
      # Execute the real sentinel contract, relocating every host path to TempDir.
      python3 -c 'import pathlib, sys
script, owner, action, root = sys.argv[1:]
source = pathlib.Path(script).read_text()
for path in ("/etc/cni/net.d", "/var/lib/cni", "/var/lib/containerd"):
    source = source.replace(path, root + path)
sys.argv = [script, owner, action]
exec(compile(source, script, "exec"))' "${case_dir}/sentinel.py" "${owner}" "${action}" "${case_dir}/host" ;;
    *chmod*) ;;
    *) fail "unexpected remote command" ;;
  esac
}

make_node() {
  local mode="$1" generation="$2" slot="${3:-}" count registration identity
  count=$(( $(<"${case_dir}/counter") + 1 )); echo "${count}" >"${case_dir}/counter"
  registration="registration-${generation}-${count}"
  identity="${slot:-binary-1}"
  jq -n --arg mode "${mode}" --arg slot "${slot}" --arg generation "${generation}" \
    --arg identity "${identity}" --arg registration "${registration}" --argjson count "${count}" \
    '{mode:$mode,slot:$slot,generation:$generation,identity:$identity,kubeletVersion:"v1.35.0",
      registration:$registration,corePids:{"kubelet.service":($count * 100),"containerd.service":($count * 100 + 1)}}' >"${case_dir}/runtime"
  jq -n --arg uid "uid-${count}" --arg reg "${registration}" '
    {metadata:{name:"token-vm",uid:$uid,labels:{"kubernetes.azure.com/flex-runtime-generation":$reg}},
     status:{addresses:[{type:"InternalIP",address:"10.0.0.10"}],conditions:[{type:"Ready",status:"True"}],
       nodeInfo:{kubeletVersion:"v1.35.0"}}}' >"${case_dir}/node"
}

node_join_token() {
  [[ "$1" == '' ]] || { fail 'release must be empty'; return 1; }
  [[ "$(<"${case_dir}/runtime")" == null && "$(<"${case_dir}/node")" == null ]] || { fail 'live mode flip'; return 1; }
  record "join $2"
  machine_configmap_upsert token-vm "${E2E_KUBERNETES_VERSION}" "${E2E_KUBERNETES_VERSION}"
  local slot=""; [[ "$2" != container ]] || slot=kube1
  make_node "$2" "${E2E_KUBERNETES_VERSION}" "${slot}"
}
machine_configmap_upsert() {
  record "goal $*"
  [[ "$1" == token-vm && "$2" == 1.35.0 ]] || { fail 'goal version upgrade or other worker mutation'; return 1; }
  printf '%s\n' "$3" >"${case_dir}/goal"
}
machine_configmap_delete() {
  record "delete-goal $1"
  [[ "$1" == token-vm ]] || { fail 'deleted another goal'; return 1; }
  [[ "${STUCK_GOAL:-0}" == 1 ]] || echo null >"${case_dir}/goal"
}
validate_node_joined() { record "ready $1"; [[ "$1" == token-vm ]]; }
validate_node_ip() { record "ip $*"; [[ "$1 $2" == 'token-vm 10.0.0.10' ]]; }
runtime_install() { record "runtime-install $1"; }
runtime_on() {
  local ip="$1" action="$2"; shift 2
  [[ "${ip}" == 192.0.2.10 ]] || return 91
  record "runtime ${action} $*"
  case "${action}" in
    snapshot) printf '%s\n' "$(<"${case_dir}/runtime")" ;;
    validate) [[ "${FAIL_VALIDATE:-0}" != 1 ]] ;;
    validate-reset) [[ "$(<"${case_dir}/runtime")" == null ]] ;;
    diagnostics) echo '{"sanitized":true}' ;;
    boot-guard) [[ "$*" == none ]] ;;
    exec)
      case "$*" in
        '-- cat /proc/1/stat')
          local i
          printf '1 (systemd)'
          for ((i=3; i<22; i++)); do printf ' 0'; done
          printf ' %s\n' "$(<"${case_dir}/epoch")" ;;
        '-- systemctl show unbounded-kubelet.service --property=ExecMainStartTimestampMonotonic --value')
          printf '%s\n' "$(<"${case_dir}/epoch")" ;;
        *) return 96 ;;
      esac ;;
    *) fail "unexpected runtime action ${action}" ;;
  esac
}
sleep() {
  SECONDS=$((SECONDS + $1))
  if [[ "${EARLY_REPAVE:-0}" == 1 && "$(<"${case_dir}/goal")" == iso-* ]]; then
    local tmp="${case_dir}/tmp"
    jq '.corePids["kubelet.service"] += 1' "${case_dir}/runtime" >"${tmp}"; mv "${tmp}" "${case_dir}/runtime"
  fi
}

kubectl() {
  local ns="" selector="" verb resource name=""
  local -a args=()
  while (( $# )); do
    case "$1" in
      --request-timeout=*) ;;
      -n) ns="$2"; shift ;;
      -l) selector="$2"; shift ;;
      -o) shift ;;
      *) args+=("$1") ;;
    esac
    shift
  done
  verb="${args[0]:-}"; resource="${args[1]:-}"; name="${args[2]:-}"
  record "kubectl ${verb} ${resource} ${name} ns=${ns} selector=${selector} ${args[*]:3}"
  case "${verb} ${resource}" in
    'create -f')
      local manifest kind
      manifest="$(</dev/stdin)"
      # create (not apply) and unique names avoid overwriting pre-existing objects.
      if [[ "${manifest}" == \{* ]]; then
        kind="$(jq -r '.kind' <<<"${manifest}")"
        [[ "${kind}" == List ]] || return 92
        jq -e 'all(.items[]; .metadata.labels["e2e.aks-flex-node.io/isolation-owner"] | startswith("iso-"))' <<<"${manifest}" >/dev/null || return
        jq -e '[.items[] | select(.kind == "Pod") | .spec.nodeName] | sort == ["managed-aks", "token-vm"]' <<<"${manifest}" >/dev/null || return
        printf '%s\n' "${manifest}" >"${case_dir}/network-manifest"
      elif [[ "${manifest}" == *'kind: MachineOperation'* ]]; then
        [[ "${manifest}" == *'machineRef: token-vm'* && "${manifest}" == *'operationKind: NodeReboot'* ]] || return 92
        [[ "${manifest}" == *'e2e.aks-flex-node.io/isolation-owner: iso-'* ]] || return 92
        echo "${SECONDS}" >"${case_dir}/reboot-start"
        rm -f "${case_dir}/reboot-done"
      elif [[ "${manifest}" != *'kind: Namespace'* || "${manifest}" != *'e2e.aks-flex-node.io/isolation-owner: iso-'* ]]; then
        return 92
      fi ;;
    'get node')
      [[ "${name}" == token-vm ]] || return 93
      [[ "$(<"${case_dir}/node")" == null ]] || printf '%s\n' "$(<"${case_dir}/node")" ;;
    'get nodes')
      # The lexically first worker is Flex, not a valid native endpoint.
      printf '%s\n' '{"items":[
        {"metadata":{"name":"aaa-flex","labels":{"kubernetes.azure.com/managed":"false","kubernetes.azure.com/agentpool":"flex","kubernetes.io/os":"linux"}},
         "spec":{"providerID":"azure:///flex"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
        {"metadata":{"name":"aaa-other-test-worker","labels":{"kubernetes.io/os":"linux"}},
         "spec":{"providerID":"azure:///test"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
        {"metadata":{"name":"aab-flex-pool","labels":{"kubernetes.azure.com/nodepool-type":"FlexNodes","kubernetes.azure.com/agentpool":"flex","kubernetes.io/os":"linux"}},
         "spec":{"providerID":"azure:///test"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
        {"metadata":{"name":"managed-aks","labels":{"kubernetes.azure.com/agentpool":"system","kubernetes.io/os":"linux"}},
         "spec":{"providerID":"azure:///managed"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
    'get configmap')
      if [[ "$(<"${case_dir}/goal")" != null ]]; then echo '{"data":{"token-vm.json":"goal"}}'; else echo '{}'; fi ;;
    'get pod') echo 10.240.0.10 ;;
    'get service') echo 10.1.0.10 ;;
    'get machineoperation')
      if (( SECONDS - $(<"${case_dir}/reboot-start") < ${REBOOT_DELAY:-0} )); then echo InProgress; return; fi
      if [[ ! -e "${case_dir}/reboot-done" ]]; then
        if [[ "${REUSE_INNER_PIDS:-0}" != 1 ]]; then
          jq '.corePids |= with_entries(.value += 10)' "${case_dir}/runtime" >"${case_dir}/tmp"
          mv "${case_dir}/tmp" "${case_dir}/runtime"
        fi
        [[ "${STALE_EPOCH:-0}" == 1 ]] || echo $(( $(<"${case_dir}/epoch") + 100 )) >"${case_dir}/epoch"
        touch "${case_dir}/reboot-done"
      fi
      echo Complete ;;
    'delete node')
      [[ "${name}" == token-vm ]] || return 94
      echo null >"${case_dir}/node"
      if [[ "$(<"${case_dir}/runtime")" != null ]]; then
        local mode slot generation
        mode="$(jq -r .mode "${case_dir}/runtime")"; slot="$(jq -r .slot "${case_dir}/runtime")"
        if [[ "${mode}" == container ]]; then
          if [[ "${slot}" == kube1 ]]; then slot=kube2; else slot=kube1; fi
        fi
        generation="$(<"${case_dir}/goal")"
        [[ "${STALE_GOAL:-0}" != 1 ]] || generation=stale
        make_node "${mode}" "${generation}" "${slot}"
      fi ;;
    'delete pods,services'|'delete machineoperations'|'delete namespaces')
      [[ "${selector}" == e2e.aks-flex-node.io/isolation-owner=iso-* ]] || return 94
      if [[ "${FAIL_CLEANUP:-0}" == 1 && "${resource}" != pods,services ]]; then return 53; fi
      if [[ "${resource}" == namespaces ]]; then
        local timeout=0 option
        for option in "${args[@]}"; do
          if [[ "${option}" == --timeout=*s ]]; then timeout="${option#--timeout=}"; timeout="${timeout%s}"; fi
        done
        (( timeout >= ${NAMESPACE_DELAY:-0} )) || return 54
      fi ;;
    'cordon token-vm') ;;
    'drain token-vm')
      [[ " ${args[*]} " != *' --force '* ]] || return 94
      [[ " ${args[*]} " == *' --timeout=120s '* ]] || return 94 ;;
    'wait pods') ;;
    'exec native'|'exec flex')
      # Match the script sent to the pod verbatim.
      # shellcheck disable=SC2016
      [[ "${ns}" == iso-* && " ${args[*]} " == *'nslookup kubernetes.default.svc.cluster.local'* &&
         " ${args[*]} " == *'http://${address}:8080/'* && " ${args[*]} " == *'.svc.cluster.local'* ]] || return 95
      [[ "${FAIL_EXEC:-0}" != 1 ]] || return 47 ;;
    'get pods,services'|'describe pods'|'get events'|'logs -l'|'get machineoperations') ;;
    'logs --all-containers') ;;
    *) fail "unexpected kubectl: ${verb} ${resource} ${name}" ;;
  esac
}

mode_validation() {
  reset_fixture validation
  local mode
  for mode in '' nspawn token NONE 'container;exit'; do
    if isolation_modes_mode "${mode}"; then fail "accepted invalid mode ${mode}"; fi
  done
  [[ ! -s "${case_dir}/trace" ]] || fail 'invalid mode caused side effects'
}

both_modes() {
  reset_fixture both
  isolation_modes_all
  [[ "$(grep '^join ' "${case_dir}/trace")" == $'join none\njoin none\njoin container\njoin container' ]]
  [[ "$(grep -c '^runtime boot-guard none' "${case_dir}/trace")" == 1 ]]
  [[ "$(grep -c '^runtime validate-reset' "${case_dir}/trace")" == 6 ]]
  [[ "$(grep -c '^kubectl exec ' "${case_dir}/trace")" == 16 ]]
  assert_absent 'FORBIDDEN|upgrade|goal token-vm [^1]'
  # Every reset is preceded by goal removal and diagnostics; no join until deletion.
  awk '/runtime diagnostics/{diag=1} /delete-goal/{goal=1}
       /remote .* reset$/{if (!diag || !goal) exit 1; diag=goal=0; reset=1}
       /kubectl delete node/{deleted=1}
       /^join /{if (!reset || !deleted) exit 1; reset=deleted=0}' "${case_dir}/trace"
}

existing_mode() {
  reset_fixture existing
  make_node nspawn 1.35.0 kube1
  isolation_modes_mode none
  assert_contains '^runtime validate-reset nspawn$'
  [[ "$(head -n1 <(grep '^join ' "${case_dir}/trace"))" == 'join none' ]]
}

failure_propagation() {
  reset_fixture failed-network
  FAIL_EXEC=1
  local rc=0
  isolation_modes_mode none || rc=$?
  [[ "${rc}" == 47 ]] || fail "lost network failure: ${rc}"
  assert_absent '^kubectl create .*reboot|^join container'
  assert_contains '^runtime diagnostics'
  [[ -f "$(find "${E2E_LOG_DIR}" -path '*iso-none*/failure.log' -print -quit)" ]]
  assert_contains '^kubectl delete namespaces .*selector=e2e.aks-flex-node.io/isolation-owner='
}

cleanup_exit_codes() {
  reset_fixture cleanup-after-success
  FAIL_CLEANUP=1
  local rc=0
  isolation_modes_mode none || rc=$?
  [[ "${rc}" == 53 ]] || fail "cleanup failure reported success: ${rc}"
  assert_contains '^kubectl delete machineoperations'
  assert_contains '^kubectl delete namespaces'
  reset_fixture cleanup-after-failure
  FAIL_CLEANUP=1
  FAIL_EXEC=1
  rc=0
  isolation_modes_mode none || rc=$?
  [[ "${rc}" == 47 ]] || fail "cleanup overwrote original error: ${rc}"
  assert_contains '^kubectl delete machineoperations'
  assert_contains '^kubectl delete namespaces'
}

namespace_cleanup_budget() {
  reset_fixture namespace-grace
  NAMESPACE_DELAY=45
  isolation_modes_mode none
  reset_fixture namespace-stalled
  NAMESPACE_DELAY=120
  local rc=0
  isolation_modes_mode none || rc=$?
  [[ "${rc}" == 54 ]] || fail "namespace cleanup did not respect its deadline: ${rc}"
}

reset_fences() {
  reset_fixture reset-failure
  FAIL_RESET=1
  if isolation_modes_all; then fail 'reset failure was hidden'; fi
  assert_absent '^join '
  reset_fixture second-reset-failure
  FAIL_RESET=3
  if isolation_modes_mode container; then fail 'idempotent reset failure was hidden'; fi
  [[ "$(grep -c '^join ' "${case_dir}/trace")" == 1 ]] || fail 'rejoined before successful second reset'
  reset_fixture stuck-goal
  STUCK_GOAL=1
  echo remains >"${case_dir}/goal"
  if isolation_modes_mode none; then fail 'reset accepted goal deletion failure'; fi
  assert_absent 'remote .* reset$|^join '
}

assertion_contracts() {
  reset_fixture assertions
  make_node container expected kube1
  local before after node stale node_stale before_same_slot
  before="$(<"${case_dir}/runtime")"
  make_node container expected kube2
  after="$(<"${case_dir}/runtime")"; node="$(<"${case_dir}/node")"
  _im_assert_ready container uid-1 expected "${before}" "${after}" "${node}"
  if _im_assert_ready container uid-2 expected "${before}" "${after}" "${node}"; then fail 'accepted stale UID'; fi
  if _im_assert_ready container uid-1 newer "${before}" "${after}" "${node}"; then fail 'accepted stale generation'; fi
  before_same_slot="$(jq '.slot="kube2" | .identity="kube2"' <<<"${before}")"
  if _im_assert_ready container uid-1 expected "${before_same_slot}" "${after}" "${node}"; then fail 'accepted unchanged slot'; fi
  if _im_assert_ready container uid-1 expected "${before}" "${after}" "$(jq '.metadata.labels={}' <<<"${node}")"; then fail 'accepted wrong registration'; fi
  stale="$(jq --arg reg "$(jq -r '.registration' <<<"${before}")" '.registration=$reg' <<<"${after}")"
  node_stale="$(jq --arg reg "$(jq -r '.registration' <<<"${before}")" '.metadata.labels["kubernetes.azure.com/flex-runtime-generation"]=$reg' <<<"${node}")"
  if _im_assert_ready container uid-1 expected "${before}" "${stale}" "${node_stale}"; then fail 'accepted reused container registration'; fi
  _im_assert_stable "${after}" "${after}" uid-2 "${node}"
  if _im_assert_stable "${before}" "${after}" uid-2 "${node}"; then fail 'accepted changed live runtime'; fi
  # Reboot identity is checked separately: OCI inner PIDs may repeat exactly.
  _im_assert_stable "${after}" "${after}" uid-2 "${node}" true
  make_node none expected
  before="$(<"${case_dir}/runtime")"
  make_node none expected
  after="$(<"${case_dir}/runtime")"; node="$(<"${case_dir}/node")"
  _im_assert_ready none uid-3 expected "${before}" "${after}" "${node}"
  stale="$(jq --arg reg "$(jq -r '.registration' <<<"${before}")" '.registration=$reg' <<<"${after}")"
  node_stale="$(jq --arg reg "$(jq -r '.registration' <<<"${before}")" '.metadata.labels["kubernetes.azure.com/flex-runtime-generation"]=$reg' <<<"${node}")"
  if _im_assert_ready none uid-3 expected "${before}" "${stale}" "${node_stale}"; then fail 'accepted reused none registration'; fi
}

live_generation_fence() {
  reset_fixture early-repave
  EARLY_REPAVE=1
  if isolation_modes_mode none; then fail 'live repave accepted'; fi
  [[ "$(grep -c '^kubectl delete node ' "${case_dir}/trace")" == 1 ]] || fail 'deleted Node after live repave violation'
  reset_fixture stale-applied
  STALE_GOAL=1
  E2E_NODE_JOIN_TIMEOUT=10
  if isolation_modes_mode container; then fail 'stale applied generation accepted'; fi
  [[ "$(grep -c '^join ' "${case_dir}/trace")" == 1 ]] || fail 'continued after stale generation'
}

container_host_ownership() {
  reset_fixture sentinels
  local im_ip=192.0.2.10 im_owner=iso-container-contract marker
  marker="${case_dir}/host/etc/cni/net.d/.${im_owner}.aks-flex-node-e2e-preserve"
  _im_host_sentinels create
  _im_host_sentinels create
  _im_host_sentinels check
  echo unrelated >"${case_dir}/host/etc/cni/net.d/keep.conf"
  echo wrong >"${marker}"
  if _im_host_sentinels create; then fail 'overwrote a differing preexisting sentinel'; fi
  if _im_host_sentinels check; then fail 'accepted changed sentinel'; fi
  printf 'aks-flex-node-e2e:%s\n' "${im_owner}" >"${marker}"
  _im_host_sentinels remove
  [[ ! -e "${marker}" && -f "${case_dir}/host/etc/cni/net.d/keep.conf" ]]
  reset_fixture lost-sentinel
  LOSE_SENTINEL=1
  if isolation_modes_mode container; then fail 'container reset destroyed host-owned state'; fi
  [[ "$(grep -c '^join ' "${case_dir}/trace")" == 1 ]]
  assert_absent '^sentinel remove'
}

container_reboot_budget() {
  reset_fixture slow-reboot
  REBOOT_DELAY=90
  REUSE_INNER_PIDS=1
  isolation_modes_mode container
  assert_contains '^runtime validate container'
  assert_contains '^runtime exec -- cat /proc/1/stat'
  [[ "$(grep -c '^join container' "${case_dir}/trace")" == 2 ]]
  reset_fixture stale-reboot-epoch
  STALE_EPOCH=1
  E2E_NODE_JOIN_TIMEOUT=10
  if isolation_modes_mode container; then fail 'accepted reboot without new init epoch'; fi
  [[ "$(grep -c '^join container' "${case_dir}/trace")" == 1 ]]
}

failed=0
for test in mode_validation both_modes existing_mode failure_propagation cleanup_exit_codes namespace_cleanup_budget reset_fences assertion_contracts live_generation_fence container_host_ownership container_reboot_budget; do
  set +e
  (set -e; "${test}") >"${work}/${test}.log" 2>&1
  rc=$?
  set -e
  if (( rc == 0 )); then
    echo "PASS ${test}"
  else
    echo "FAIL ${test} (${rc})"
    # These are exclusively local stub logs, never live VM/configuration dumps.
    while IFS= read -r line; do printf '%s\n' "${line}"; done <"${work}/${test}.log"
    failed=$((failed + 1))
  fi
done
(( failed == 0 ))
