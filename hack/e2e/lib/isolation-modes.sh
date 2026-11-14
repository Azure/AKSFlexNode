#!/usr/bin/env bash
# Lifecycle coverage on the existing token VM; no infrastructure provisioning.
set -euo pipefail
[[ -n "${_E2E_ISOLATION_MODES_LOADED:-}" ]] && return 0
readonly _E2E_ISOLATION_MODES_LOADED=1

# shellcheck source=common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
# shellcheck source=controller.sh
source "$(dirname "${BASH_SOURCE[0]}")/controller.sh"
# shellcheck source=node-join.sh
source "$(dirname "${BASH_SOURCE[0]}")/node-join.sh"
# shellcheck source=node-join-token.sh
source "$(dirname "${BASH_SOURCE[0]}")/node-join-token.sh"
# shellcheck source=validate.sh
source "$(dirname "${BASH_SOURCE[0]}")/validate.sh"
# shellcheck source=runtime.sh
source "$(dirname "${BASH_SOURCE[0]}")/runtime.sh"
# shellcheck source=cleanup.sh
source "$(dirname "${BASH_SOURCE[0]}")/cleanup.sh"

_im_k() { kubectl --request-timeout=20s "$@"; }
_im_fail() { log_error "$*"; return 1; }
_im_node() {
  local node
  node="$(_im_k get node "${im_name}" --ignore-not-found -o json)" || return
  printf '%s\n' "${node:-null}"
}

# Keep persisted configurations and startup records out of diagnostics.
_im_diagnostics() {
  local phase="$1"
  mkdir -p "${im_logs}"
  {
    runtime_on "${im_ip}" diagnostics || true
    _im_k get node "${im_name}" -o wide || true
    _im_k -n "${im_ns}" get pods,services -l "${im_selector}" -o wide || true
    _im_k -n "${im_ns}" describe pods -l "${im_selector}" || true
    _im_k -n "${im_ns}" get events --sort-by=.lastTimestamp || true
    _im_k -n "${im_ns}" logs -l "${im_selector}" --all-containers --tail=80 --prefix || true
    _im_k get machineoperations -l "${im_selector}" \
      -o custom-columns=NAME:.metadata.name,PHASE:.status.phase || true
  } >"${im_logs}/${phase}.log" 2>&1
  _collect_vm_logs "${im_ip}" "${im_owner}-${phase}" >/dev/null || true
  log_info "Isolation diagnostics: ${im_logs}/${phase}.log"
}

_im_finish() {
  local rc="$1" cleanup_rc=0
  if (( rc != 0 )); then _im_diagnostics failure || true; fi
  if (( rc == 0 && im_sentinels == 1 )); then _im_host_sentinels remove || cleanup_rc=$?; fi
  # Selectors prevent cleanup of any object not created by this invocation.
  if (( im_namespace_created == 1 )); then
    _im_k delete machineoperations -l "${im_selector}" --ignore-not-found --timeout=30s >/dev/null || cleanup_rc=$?
    # Namespace GC may run after the pods' default 30-second termination grace.
    _im_k delete namespaces -l "${im_selector}" --ignore-not-found --timeout=90s --request-timeout=95s >/dev/null || cleanup_rc=$?
  fi
  if (( rc == 0 && cleanup_rc != 0 )); then
    rc="${cleanup_rc}"
    log_error "Isolation cleanup failed (status ${rc})"
    _im_diagnostics cleanup-failure || true
  fi
  exit "${rc}"
}

_im_drain() {
  local node
  node="$(_im_node)" || return
  [[ "${node}" != null ]] || return 0
  jq -e --arg ip "${im_private_ip}" \
    'any(.status.addresses[]; .type == "InternalIP" and .address == $ip)' <<<"${node}" >/dev/null ||
    { _im_fail "Refusing to drain ${im_name}: token VM InternalIP mismatch"; return 1; }
  # Remove only our bare pods. Unowned unmanaged pods/PDBs must block the drain.
  _im_k -n "${im_ns}" delete pods,services -l "${im_selector}" --ignore-not-found --timeout=60s >/dev/null || return
  _im_k cordon "${im_name}" || return
  _im_k drain "${im_name}" --ignore-daemonsets --delete-emptydir-data --timeout=120s --request-timeout=125s
}

_im_reset() {
  machine_configmap_delete "${im_name}" || return
  local goal binary="${1:-/opt/unbounded/agent/bin/aks-flex-node}"
  goal="$(_im_k -n "${E2E_CONTROLLER_NAMESPACE}" get configmap "${E2E_MACHINE_CONFIGMAP}" --ignore-not-found -o json)" || return
  jq -e --arg key "${im_name}" --arg jsonKey "${im_name}.json" \
    '(.data[$key] == null) and (.data[$jsonKey] == null)' <<<"${goal:-null}" >/dev/null ||
    { _im_fail 'Token VM Machine goal remains published before reset'; return 1; }
  remote_exec "${im_ip}" "sudo -n timeout --kill-after=5s 180 '${binary}' reset"
}

_im_delete() {
  _im_k delete node "${im_name}" --ignore-not-found --wait=true --timeout=60s
}

_im_host_sentinels() {
  remote_exec "${im_ip}" "sudo -n timeout 30 python3 - '${im_owner}' '$1'" <<'PY'
import pathlib
import sys

owner, action = sys.argv[1:]
expected = ("aks-flex-node-e2e:" + owner + "\n").encode()
paths = [pathlib.Path(root) / ("." + owner + ".aks-flex-node-e2e-preserve")
         for root in ("/etc/cni/net.d", "/var/lib/cni", "/var/lib/containerd")]
for path in paths:
    if path.is_symlink():
        raise SystemExit("refusing symlink sentinel: " + str(path))
    if path.exists():
        if path.read_bytes() != expected:
            raise SystemExit("host sentinel content changed: " + str(path))
    elif action != "create":
        raise SystemExit("host sentinel missing: " + str(path))
for path in paths:
    if action == "create" and not path.exists():
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("xb") as stream:
            stream.write(expected)
    elif action == "remove":
        path.unlink()
PY
}

# Pure assertions are shared by the live poller and offline contract tests.
_im_assert_ready() {
  local mode="$1" old_uid="$2" generation="$3" before="$4" snapshot="$5" node="$6"
  jq -en --arg mode "${mode}" --arg old "${old_uid}" --arg generation "${generation}" \
    --arg version "${E2E_KUBERNETES_VERSION#v}" --argjson before "${before}" \
    --argjson s "${snapshot}" --argjson n "${node}" '
    $s.mode == $mode and $s.generation == $generation and
    ($s.kubeletVersion | ltrimstr("v")) == $version and
    ($s.identity | type == "string" and length > 0) and
    ($s.registration | type == "string" and length > 0) and
    ($s.corePids | type == "object" and length > 0 and all(.[]; type == "number" and . > 0)) and
    ($n.metadata.uid | type == "string" and length > 0 and . != $old) and
    any($n.status.conditions[]; .type == "Ready" and .status == "True") and
    ($n.status.nodeInfo.kubeletVersion | ltrimstr("v")) == $version and
    $n.metadata.labels["kubernetes.azure.com/flex-runtime-generation"] == $s.registration and
    ($before == null or $s.registration != $before.registration) and
    (if $mode == "container" then
       ($s.slot == "kube1" or $s.slot == "kube2") and
       ($before == null or ($s.slot != $before.slot and $s.identity != $before.identity))
     else $s.slot == "" end)' >/dev/null
}

_im_assert_stable() {
  local before="$1" after="$2" uid="$3" node="$4" reboot="${5:-false}"
  jq -en --argjson b "${before}" --argjson a "${after}" --arg uid "${uid}" \
    --argjson n "${node}" --argjson reboot "${reboot}" '
    ($a | del(.corePids)) == ($b | del(.corePids)) and
    ($reboot or $a.corePids == $b.corePids) and
    $n.metadata.uid == $uid and
    $n.metadata.labels["kubernetes.azure.com/flex-runtime-generation"] == $a.registration and
    any($n.status.conditions[]; .type == "Ready" and .status == "True")' >/dev/null
}

_im_wait_ready() {
  local old_uid="$1" generation="$2" before="${3:-null}" snapshot node
  local deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
  while (( SECONDS < deadline )); do
    if snapshot="$(runtime_on "${im_ip}" snapshot)" && node="$(_im_node)" &&
      _im_assert_ready "${im_mode}" "${old_uid}" "${generation}" "${before}" "${snapshot}" "${node}" &&
      runtime_on "${im_ip}" validate "${im_mode}"; then
      validate_node_joined "${im_name}" || return
      validate_node_ip "${im_name}" "${im_private_ip}" || return
      return 0
    fi
    sleep 5
  done
  _im_fail "Timed out waiting for fresh Ready ${im_mode} Node and applied generation ${generation}"
}

_im_network() {
  local native manifest target source pod_ip service_ip
  native="$(_im_k get nodes -o json | jq -er --arg flex "${im_name}" '
    [.items[] | select(.metadata.name != $flex and
      .metadata.labels["kubernetes.azure.com/managed"] != "false" and
      (.metadata.labels["kubernetes.azure.com/agentpool"] // "" | length > 0) and
      .metadata.labels["kubernetes.azure.com/nodepool-type"] != "FlexNodes" and
      .metadata.labels["kubernetes.io/os"] == "linux" and
      (.metadata.labels["kubernetes.azure.com/flex-runtime-generation"] == null) and
      (.spec.providerID // "" | startswith("azure://")) and (.spec.unschedulable != true) and
      any(.status.conditions[]; .type == "Ready" and .status == "True"))] |
    sort_by(.metadata.name) | .[0].metadata.name // error("no managed AKS native Node")')" || return
  _im_k -n "${im_ns}" delete pods,services -l "${im_selector}" --ignore-not-found --timeout=60s >/dev/null || return
  manifest="$(jq -n --arg ns "${im_ns}" --arg owner "${im_owner}" --arg native "${native}" --arg flex "${im_name}" '
    {apiVersion:"v1",kind:"List",items:[ ["native","flex"][] as $role |
      {apiVersion:"v1",kind:"Pod",metadata:{name:$role,namespace:$ns,
        labels:{"e2e.aks-flex-node.io/isolation-owner":$owner,peer:$role}},spec:{
        nodeName:(if $role == "native" then $native else $flex end),
        containers:[{name:"http",image:"busybox:1.36",command:["sh","-ec",
          ("mkdir -p /www; echo " + $role + " > /www/index.html; exec httpd -f -p 8080 -h /www")],
          readinessProbe:{httpGet:{path:"/",port:8080},periodSeconds:2},
          resources:{requests:{cpu:"10m",memory:"16Mi"},limits:{memory:"64Mi"}}}]}},
      {apiVersion:"v1",kind:"Service",metadata:{name:$role,namespace:$ns,
        labels:{"e2e.aks-flex-node.io/isolation-owner":$owner}},spec:{
        selector:{"e2e.aks-flex-node.io/isolation-owner":$owner,peer:$role},
        ports:[{port:8080,targetPort:8080}]}} ]}')" || return
  _im_k create -f - <<<"${manifest}" >/dev/null || return
  # Inspect failing pods before spending the remaining readiness budget.
  local first=60 remaining=$((E2E_POD_READY_TIMEOUT - 60))
  (( E2E_POD_READY_TIMEOUT >= 60 )) || first="${E2E_POD_READY_TIMEOUT}"
  if ! _im_k -n "${im_ns}" wait pods -l "${im_selector}" --for=condition=Ready --timeout="${first}s" --request-timeout="$((first + 5))s"; then
    _im_diagnostics pods-pending || return
    (( remaining > 0 )) || return 1
    _im_k -n "${im_ns}" wait pods -l "${im_selector}" --for=condition=Ready --timeout="${remaining}s" --request-timeout="$((remaining + 5))s" || return
  fi
  for source in native flex; do
    target=native; [[ "${source}" != native ]] || target=flex
    pod_ip="$(_im_k -n "${im_ns}" get pod "${target}" -o jsonpath='{.status.podIP}')" || return
    service_ip="$(_im_k -n "${im_ns}" get service "${target}" -o jsonpath='{.spec.clusterIP}')" || return
    [[ -n "${pod_ip}" && -n "${service_ip}" ]] || return 1
    # Pod readiness can precede EndpointSlice/kube-proxy convergence.
    # shellcheck disable=SC2016
    _im_k -n "${im_ns}" exec "${source}" -- timeout 45 sh -ec '
      test "$(echo exec-ok)" = exec-ok
      until nslookup kubernetes.default.svc.cluster.local; do sleep 1; done
      for address in "$2" "$3" "$4"; do
        until test "$(wget -q -T 5 -O - "http://${address}:8080/")" = "$1"; do sleep 1; done
      done' sh "${target}" "${pod_ip}" "${service_ip}" "${target}.${im_ns}.svc.cluster.local" || return
  done
}

_im_runtime_epoch() {
  local epoch
  if [[ "${im_mode}" == container ]]; then
    # OCI inner PIDs repeat after restart; PID 1's kernel start time does not.
    epoch="$(runtime_on "${im_ip}" exec -- cat /proc/1/stat | awk '{sub(/^.*\) /, ""); print $20}')" || return
  else
    epoch="$(runtime_on "${im_ip}" exec -- systemctl show unbounded-kubelet.service --property=ExecMainStartTimestampMonotonic --value)" || return
  fi
  [[ "${epoch}" =~ ^[1-9][0-9]*$ ]] || return 1
  printf '%s\n' "${epoch}"
}

_im_reboot() {
  local before node uid operation="${im_owner}-reboot" phase after before_epoch after_epoch
  before="$(runtime_on "${im_ip}" snapshot)" || return
  before_epoch="$(_im_runtime_epoch)" || return
  node="$(_im_node)" || return
  uid="$(jq -er '.metadata.uid' <<<"${node}")" || return
  _im_k create -f - >/dev/null <<EOF || return
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: ${operation}
  labels:
    e2e.aks-flex-node.io/isolation-owner: ${im_owner}
spec:
  machineRef: ${im_name}
  operationKind: NodeReboot
  ttlSecondsAfterFinished: 3600
EOF
  local deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
  while (( SECONDS < deadline )); do
    phase="$(_im_k get machineoperation "${operation}" -o jsonpath='{.status.phase}')" || return
    case "${phase}" in
      Failed) _im_fail "NodeReboot failed for ${im_mode}"; return 1 ;;
      Complete)
        if after="$(runtime_on "${im_ip}" snapshot)" && node="$(_im_node)" &&
          after_epoch="$(_im_runtime_epoch)" && [[ "${after_epoch}" != "${before_epoch}" ]] &&
          _im_assert_stable "${before}" "${after}" "${uid}" "${node}" true &&
          runtime_on "${im_ip}" validate "${im_mode}"; then
          validate_node_joined "${im_name}" || return
          validate_node_ip "${im_name}" "${im_private_ip}"
          return
        fi ;;
    esac
    sleep 5
  done
  _im_fail "NodeReboot did not recover within ${E2E_NODE_JOIN_TIMEOUT}s"
}

_im_repave() {
  local before uid after node i generation="${im_owner}-settings"
  before="$(runtime_on "${im_ip}" snapshot)" || return
  node="$(_im_node)" || return
  uid="$(jq -er '.metadata.uid' <<<"${node}")" || return
  machine_configmap_upsert "${im_name}" "${E2E_KUBERNETES_VERSION}" "${generation}" || return
  # E2E join config polls the Machine every 5s. Keep the existing Node through
  # several polls: a settings-only goal must not restart any core service.
  for ((i=0; i<6; i++)); do
    sleep 5
    after="$(runtime_on "${im_ip}" snapshot)" || return
    node="$(_im_node)" || return
    _im_assert_stable "${before}" "${after}" "${uid}" "${node}" ||
      { _im_fail "Live Node/core identity changed before Node deletion"; return 1; }
  done
  _im_diagnostics pre-repave || return
  _im_drain || return
  _im_delete || return
  _im_wait_ready "${uid}" "${generation}" "${before}"
}

_im_lifecycle() {
  local previous_uid node present previous_mode=none previous
  runtime_install "${im_ip}" || return
  remote_copy "${E2E_BINARY}" "${im_ip}" /tmp/aks-flex-node-e2e-reset || return
  remote_exec "${im_ip}" 'sudo -n timeout 15 chmod 0755 /tmp/aks-flex-node-e2e-reset' || return
  present="$(remote_exec "${im_ip}" 'sudo -n timeout 15 python3 -c '\''import os; print("present" if os.path.exists("/etc/aks-flex-node/daemon-state.json") else "absent")'\''')" || return
  case "${present}" in
    present)
      previous="$(runtime_on "${im_ip}" snapshot)" || return
      previous_mode="$(jq -er '.mode' <<<"${previous}")" || return ;;
    absent) ;;
    *) _im_fail 'Cannot determine previous token VM runtime'; return 1 ;;
  esac
  node="$(_im_node)" || return
  previous_uid="$(jq -r '.metadata.uid // ""' <<<"${node}")" || return
  _im_diagnostics pre-initial-reset || return
  _im_drain || return
  _im_reset /tmp/aks-flex-node-e2e-reset || return
  _im_delete || return
  if [[ "${present}" == present ]]; then
    runtime_on "${im_ip}" validate-reset "${previous_mode}" || return
  else
    runtime_on "${im_ip}" validate-reset "${im_mode}" --allow-missing-agent || return
  fi
  if [[ "${im_mode}" == container ]]; then
    _im_host_sentinels create || return
    im_sentinels=1
  fi
  # Mode selection occurs only after the previous runtime and Node are gone.
  node_join_token '' "${im_mode}" || return
  _im_wait_ready "${previous_uid}" "${E2E_KUBERNETES_VERSION}" || return
  if [[ "${im_mode}" == none ]]; then runtime_on "${im_ip}" boot-guard none || return; fi
  _im_network || return
  _im_reboot || return
  _im_network || return
  _im_repave || return
  _im_network || return
  node="$(_im_node)" || return
  previous_uid="$(jq -er '.metadata.uid' <<<"${node}")" || return
  _im_diagnostics pre-reset || return
  _im_drain || return
  _im_reset || return
  _im_delete || return
  runtime_on "${im_ip}" validate-reset "${im_mode}" || return
  if (( im_sentinels == 1 )); then _im_host_sentinels check || return; fi
  _im_diagnostics pre-second-reset || return
  _im_reset || return
  runtime_on "${im_ip}" validate-reset "${im_mode}" || return
  if (( im_sentinels == 1 )); then _im_host_sentinels check || return; fi
  node="$(_im_node)" || return
  [[ "${node}" == null ]] || { _im_fail 'Node reappeared after reset'; return 1; }
  node_join_token '' "${im_mode}" || return
  _im_wait_ready "${previous_uid}" "${E2E_KUBERNETES_VERSION}" || return
  _im_network || return
  _im_diagnostics complete
}

isolation_modes_mode() (
  local im_mode="${1:-}"
  case "${im_mode}" in none|container) ;; *) _im_fail "Unsupported isolation mode: ${im_mode}"; return 1 ;; esac
  local im_name im_ip im_private_ip im_owner im_ns im_selector im_logs im_sentinels=0 im_namespace_created=0
  local E2E_SSH_OPTS="${E2E_SSH_OPTS:-} -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=3"
  im_name="$(state_get token_vm_name)"; im_ip="$(state_get token_vm_ip)"; im_private_ip="$(state_get token_vm_private_ip)"
  if [[ ! "${im_name}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || ! is_valid_ipv4 "${im_ip}" || ! is_valid_ipv4 "${im_private_ip}"; then
    _im_fail 'Existing token VM name/public/private IP required in E2E state'
    return 1
  fi
  im_owner="iso-${im_mode}-$(date +%s)-${RANDOM}"; im_ns="${im_owner}"
  im_selector="e2e.aks-flex-node.io/isolation-owner=${im_owner}"
  im_logs="${E2E_LOG_DIR}/${im_owner}"
  trap '_im_finish "$?"' EXIT
  log_section "Isolation lifecycle: ${im_mode} on ${im_name}"
  _im_k create -f - >/dev/null <<EOF || return
apiVersion: v1
kind: Namespace
metadata:
  name: ${im_ns}
  labels:
    e2e.aks-flex-node.io/isolation-owner: ${im_owner}
EOF
  im_namespace_created=1
  _im_lifecycle
)

isolation_modes_all() {
  local mode
  for mode in none container; do isolation_modes_mode "${mode}" || return; done
}
