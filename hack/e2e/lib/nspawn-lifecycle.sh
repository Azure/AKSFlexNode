#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/nspawn-lifecycle.sh - Host-side nspawn lifecycle E2E validation
#
# Functions:
#   nspawn_lifecycle_all - Validate generated hooks on every Flex Node and
#                          exercise config regeneration/reconcile on one node.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NSPAWN_LIFECYCLE_LOADED:-}" ]] && return 0
readonly _E2E_NSPAWN_LIFECYCLE_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/validate.sh"

_validate_nspawn_lifecycle_contract() {
  local mode="$1"
  local vm_ip
  vm_ip="$(state_get "${mode}_vm_ip")"

  log_info "Validating installed nspawn lifecycle hooks on ${mode} node..."
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail

helper="/usr/local/bin/unbounded-agent-nspawn-lifecycle"
state_file="/etc/aks-flex-node/daemon-state.json"
machine="$(sudo python3 - <<'PY'
import json
with open("/etc/aks-flex-node/daemon-state.json", encoding="utf-8") as state:
    print(json.load(state).get("activeMachine", ""))
PY
)"

case "${machine}" in
  kube1|kube2) ;;
  *) echo "invalid active machine in ${state_file}: ${machine:-empty}" >&2; exit 1 ;;
esac

sudo test -x "${helper}"
sudo "${helper}" nspawn-lifecycle --help >/dev/null

regeneration_unit="/etc/systemd/system/unbounded-agent-regenerate-config@${machine}.service"
override="/etc/systemd/system/systemd-nspawn@${machine}.service.d/override.conf"
sudo grep -Fqx "ExecStart=${helper} nspawn-lifecycle pre-start ${machine}" "${regeneration_unit}"
sudo grep -Fqx "ExecStartPost=${helper} nspawn-lifecycle post-start ${machine}" "${override}"

state="$(sudo machinectl show "${machine}" --property=State --value)"
if [[ "${state}" != "running" ]]; then
  echo "active machine ${machine} is ${state:-unknown}, want running" >&2
  exit 1
fi

printf '%s\n' "${machine}"
REMOTE
  log_success "Installed lifecycle helper and generated hooks are valid on ${mode} node"
}

_reconcile_nspawn_lifecycle() {
  local mode="$1"
  local vm_ip vm_name timeout
  vm_ip="$(state_get "${mode}_vm_ip")"
  vm_name="$(state_get "${mode}_vm_name")"
  timeout="${E2E_NODE_JOIN_TIMEOUT}"

  log_info "Regenerating config and reconciling the active nspawn machine on ${mode} node..."
  remote_exec "${vm_ip}" "E2E_NSPAWN_LIFECYCLE_TIMEOUT=${timeout} bash -s" <<'REMOTE'
set -euo pipefail

helper="/usr/local/bin/unbounded-agent-nspawn-lifecycle"
machine="$(sudo python3 - <<'PY'
import json
with open("/etc/aks-flex-node/daemon-state.json", encoding="utf-8") as state:
    print(json.load(state).get("activeMachine", ""))
PY
)"
case "${machine}" in
  kube1|kube2) ;;
  *) echo "invalid active machine: ${machine:-empty}" >&2; exit 1 ;;
esac

nspawn_config="/etc/systemd/nspawn/${machine}.nspawn"
marker="# aks-flex-node-e2e-lifecycle-marker"
sudo test -f "${nspawn_config}"
printf '%s\n' "${marker}" | sudo tee -a "${nspawn_config}" >/dev/null
sudo grep -Fqx "${marker}" "${nspawn_config}"

# This directly exercises the application-owned persisted-config loader. A
# successful pre-start rewrites the generated file and removes the marker.
sudo "${helper}" nspawn-lifecycle pre-start "${machine}"
if sudo grep -Fq "${marker}" "${nspawn_config}"; then
  echo "pre-start did not regenerate ${nspawn_config}" >&2
  exit 1
fi

old_leader="$(sudo machinectl show "${machine}" --property=Leader --value)"
if [[ -z "${old_leader}" || "${old_leader}" == "0" ]]; then
  echo "failed to capture leader for ${machine}" >&2
  exit 1
fi

# Reconcile restarts systemd-nspawn@<machine>; its generated dependencies invoke
# pre-start and post-start through the copied lifecycle helper.
sudo "${helper}" nspawn-lifecycle reconcile "${machine}"

deadline=$((SECONDS + E2E_NSPAWN_LIFECYCLE_TIMEOUT))
while (( SECONDS < deadline )); do
  state="$(sudo machinectl show "${machine}" --property=State --value 2>/dev/null || true)"
  new_leader="$(sudo machinectl show "${machine}" --property=Leader --value 2>/dev/null || true)"
  if [[ "${state}" == "running" && -n "${new_leader}" && "${new_leader}" != "0" && "${new_leader}" != "${old_leader}" ]]; then
    echo "${machine} restarted from leader ${old_leader} to ${new_leader}"
    exit 0
  fi
  sleep 5
done

sudo systemctl status "systemd-nspawn@${machine}.service" --no-pager -l || true
sudo journalctl -u "unbounded-agent-regenerate-config@${machine}.service" -n 100 --no-pager || true
sudo journalctl -u "systemd-nspawn@${machine}.service" -n 100 --no-pager || true
echo "timed out waiting for ${machine} to restart with a new leader" >&2
exit 1
REMOTE

  validate_node_joined "${vm_name}"
  smoke_test "${vm_name}" "${mode}-nspawn-lifecycle"
  log_success "Nspawn lifecycle reconcile passed on ${mode} node"
}

nspawn_lifecycle_all() {
  log_section "Nspawn Lifecycle Reconciliation"

  local mode
  for mode in msi token offline kubeadm; do
    _validate_nspawn_lifecycle_contract "${mode}"
  done

  # One representative online bootstrap-token node exercises the mutating
  # lifecycle flow; every mode above verifies the installed helper contract.
  _reconcile_nspawn_lifecycle token
}
