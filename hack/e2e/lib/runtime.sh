#!/usr/bin/env bash
# Mode-aware probes; source after common.sh for remote_exec/remote_copy.

[[ -n "${_E2E_RUNTIME_LOADED:-}" ]] && return 0
readonly _E2E_RUNTIME_LOADED=1

runtime_install() {
  local vm_ip="$1"
  local helper
  helper="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/runtime.py"
  remote_copy "${helper}" "${vm_ip}" /tmp/aks-flex-node-e2e-runtime.py
}

runtime_on() {
  local vm_ip="$1"; shift
  local command='timeout --signal=TERM --kill-after=5s 150s sudo -n python3 /tmp/aks-flex-node-e2e-runtime.py'
  local arg quoted
  for arg in "$@"; do
    quoted="${arg//\'/\'\\\'\'}"
    command+=" '${quoted}'"
  done
  remote_exec "${vm_ip}" "${command}"
}
