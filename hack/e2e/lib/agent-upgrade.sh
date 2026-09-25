#!/usr/bin/env bash
# =============================================================================
# AgentUpgrade blue/green, rollback, retry, and nspawn synchronization E2E test.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_AGENT_UPGRADE_LOADED:-}" ]] && return 0
readonly _E2E_AGENT_UPGRADE_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

_agent_upgrade_ensure_api() {
  local unbounded_dir
  unbounded_dir="$(cd "${REPO_ROOT}" && go list -m -f '{{.Dir}}' github.com/Azure/unbounded)"
  kubectl apply -f "${unbounded_dir}/deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml"
  kubectl wait --for=condition=Established \
    customresourcedefinition/machineoperations.unbounded-cloud.io --timeout=60s
}

_agent_upgrade_prepare_server() {
  local vm_ip="$1"
  local upload_path="/tmp/aks-flex-node-e2e-upgrade-binary"
  remote_copy "${E2E_BINARY}" "${vm_ip}" "${upload_path}"

  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
work=/opt/aks-flex-node-e2e-upgrade
sudo rm -rf "${work}"
sudo install -d -m 0755 "${work}"
sudo install -m 0755 /tmp/aks-flex-node-e2e-upgrade-binary "${work}/aks-flex-node-linux-amd64"
sudo tar -C "${work}" -czf "${work}/success.tar.gz" aks-flex-node-linux-amd64

# It answers host-root the way a current release does, so verification accepts
# it and the failure comes from the daemon.
cat >/tmp/aks-flex-node-e2e-broken <<'BROKEN'
#!/bin/sh
if [ "${1:-}" = "version" ]; then
  echo "e2e-forced-daemon-failure"
  exit 0
fi
if [ "${1:-}" = "host-root" ]; then
  readlink -m /opt/unbounded
  exit 0
fi
exit 42
BROKEN
sudo install -m 0755 /tmp/aks-flex-node-e2e-broken "${work}/aks-flex-node-linux-amd64"
sudo tar -C "${work}" -czf "${work}/failure.tar.gz" aks-flex-node-linux-amd64

# A release before the host root has no host-root command.
cat >/tmp/aks-flex-node-e2e-legacy <<'LEGACY'
#!/bin/sh
if [ "${1:-}" = "version" ]; then
  echo "e2e-release-before-the-host-root"
  exit 0
fi
echo "Error: unknown command \"${1:-}\" for \"aks-flex-node\"" >&2
exit 1
LEGACY
sudo install -m 0755 /tmp/aks-flex-node-e2e-legacy "${work}/aks-flex-node-linux-amd64"
sudo tar -C "${work}" -czf "${work}/legacy.tar.gz" aks-flex-node-linux-amd64
check_dir="$(mktemp -d)"
sudo tar -C "${check_dir}" -xzf "${work}/failure.tar.gz"
sudo "${check_dir}/aks-flex-node-linux-amd64" version >/dev/null
if sudo "${check_dir}/aks-flex-node-linux-amd64" agent >/dev/null 2>&1; then
  echo 'forced-failure archive unexpectedly starts the daemon command' >&2
  exit 1
fi
rm -rf "${check_dir}"
sudo install -m 0755 /tmp/aks-flex-node-e2e-upgrade-binary "${work}/aks-flex-node-linux-amd64"

sudo systemctl stop aks-flex-node-e2e-upgrade-server.service 2>/dev/null || true
sudo systemd-run --unit=aks-flex-node-e2e-upgrade-server.service \
  --property=WorkingDirectory="${work}" \
  /usr/bin/python3 -m http.server 18080 --bind 127.0.0.1 >/dev/null
for _ in $(seq 1 30); do
  if curl --silent --fail http://127.0.0.1:18080/success.tar.gz >/dev/null; then
    exit 0
  fi
  sleep 1
done
echo 'upgrade archive server did not become ready' >&2
exit 1
REMOTE
}

_agent_upgrade_digest() {
  local vm_ip="$1" archive="$2"
  remote_exec "${vm_ip}" "sha256sum /opt/aks-flex-node-e2e-upgrade/${archive} | awk '{print \$1}'"
}

_agent_upgrade_apply() {
  local operation="$1" vm_name="$2" archive="$3" digest="$4" token="$5" digest_parameter=""
  if [[ -n "${digest}" ]]; then
    digest_parameter="    sha256: ${digest}"
  fi
  cat <<EOF | kubectl apply -f -
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: ${operation}
spec:
  machineRef: ${vm_name}
  operationKind: AgentUpgrade
  parameters:
    downloadURL: http://127.0.0.1:18080/${archive}?sig=${token}
${digest_parameter}
  ttlSecondsAfterFinished: 3600
EOF
}

_agent_upgrade_wait_phase() {
  local operation="$1" phase="$2"
  if kubectl wait "machineoperation/${operation}" \
    --for="jsonpath={.status.phase}=${phase}" \
    --timeout="${E2E_AGENT_UPGRADE_TIMEOUT:-300}s"; then
    return 0
  fi
  log_error "MachineOperation ${operation} did not reach ${phase}"
  kubectl get machineoperation "${operation}" -o yaml || true
  return 1
}

_agent_upgrade_snapshot() {
  local vm_ip="$1"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
current="$(sudo readlink -f /opt/unbounded/lib/aks-flex-node/aks-flex-node-current)"
machine="$(sudo python3 - <<'PY'
import json
with open('/etc/aks-flex-node/daemon-state.json', encoding='utf-8') as stream:
    print(json.load(stream)['activeMachine'])
PY
)"
host_digest="$(sudo sha256sum "${current}" | awk '{print $1}')"
nspawn_digest="$(sudo sha256sum "/var/lib/machines/${machine}/usr/local/bin/aks-flex-node" | awk '{print $1}')"
printf '%s|%s|%s|%s\n' "${current}" "${machine}" "${host_digest}" "${nspawn_digest}"
REMOTE
}

_agent_upgrade_assert_synchronized() {
  local vm_ip="$1" snapshot current machine host_digest nspawn_digest
  snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r current machine host_digest nspawn_digest <<<"${snapshot}"
  if [[ -z "${current}" || -z "${machine}" || "${host_digest}" != "${nspawn_digest}" ]]; then
    log_error "Host/nspawn agent mismatch: ${snapshot}"
    return 1
  fi
  log_info "Agent binary synchronized: slot=${current} machine=${machine} digest=${host_digest}"
}

_agent_upgrade_validate_kubelet_auth() {
  local vm_name="$1" vm_ip="$2" before_renew elapsed=0
  before_renew="$(kubectl get lease "${vm_name}" -n kube-node-lease -o jsonpath='{.spec.renewTime}')"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
machine="$(sudo python3 - <<'PY'
import json
with open('/etc/aks-flex-node/daemon-state.json', encoding='utf-8') as stream:
    print(json.load(stream)['activeMachine'])
PY
)"
sudo systemd-run --machine="${machine}" --quiet --pipe systemctl restart kubelet.service
REMOTE

  while (( elapsed < 120 )); do
    local renew ready
    renew="$(kubectl get lease "${vm_name}" -n kube-node-lease -o jsonpath='{.spec.renewTime}' 2>/dev/null || true)"
    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    if [[ -n "${renew}" && "${renew}" != "${before_renew}" && "${ready}" == "True" ]]; then
      log_success "Kubelet renewed its lease after restart using the synchronized exec credential binary"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done
  log_error "Kubelet did not renew its lease after AgentUpgrade"
  return 1
}

_agent_upgrade_direct_activation() {
  local vm_name="$1" vm_ip="$2" before_snapshot before_slot before_digest after_snapshot after_slot after_digest
  before_snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r before_slot _ before_digest _ <<<"${before_snapshot}"

  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
work=/opt/aks-flex-node-e2e-upgrade
candidate="${work}/aks-flex-node-direct-candidate"
current_link=/opt/unbounded/lib/aks-flex-node/aks-flex-node-current
last_good_link=/opt/unbounded/lib/aks-flex-node/aks-flex-node-last-good
# The unit names the link under the resolved host root.
unit_current="$(readlink -m /opt/unbounded/lib/aks-flex-node)/aks-flex-node-current"
service=/etc/systemd/system/aks-flex-node-agent.service

sudo cp "${work}/aks-flex-node-linux-amd64" "${candidate}"
printf '\nAKS-FLEX-DIRECT-ACTIVATION-E2E\n' | sudo tee -a "${candidate}" >/dev/null
sudo chmod 0755 "${candidate}"
current_before="$(sudo readlink -f "${current_link}")"
last_good_before="$(sudo readlink -f "${last_good_link}")"
unit_before="$(sudo sha256sum "${service}" | awk '{print $1}')"

sudo "${candidate}" agent-upgrade --preflight | tee /tmp/direct-agent-upgrade-preflight.log
[[ "$(sudo readlink -f "${current_link}")" == "${current_before}" ]]
[[ "$(sudo readlink -f "${last_good_link}")" == "${last_good_before}" ]]
[[ "$(sudo sha256sum "${service}" | awk '{print $1}')" == "${unit_before}" ]]
sudo "${candidate}" agent-upgrade | tee /tmp/direct-agent-upgrade.log
current_after="$(sudo readlink -f "${current_link}")"
[[ "${current_after}" != "${current_before}" ]]
[[ "$(sudo readlink -f "${last_good_link}")" == "${current_before}" ]]
[[ "$(sudo readlink -f /opt/unbounded/bin/aks-flex-node)" == "${current_after}" ]]
[[ "$(sudo sha256sum "${candidate}" | awk '{print $1}')" == "$(sudo sha256sum "${current_after}" | awk '{print $1}')" ]]
sudo grep -Fq "ExecStart=${unit_current} agent" "${service}"
sudo systemctl is-active --quiet aks-flex-node-agent.service
pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
[[ "$(sudo readlink -f "/proc/${pid}/exe")" == "${current_after}" ]]
[[ ! -e /etc/aks-flex-node/agent-upgrade-signal.json ]]
REMOTE

  after_snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r after_slot _ after_digest _ <<<"${after_snapshot}"
  if [[ -z "${after_slot}" || "${after_slot}" == "${before_slot}" || "${after_digest}" == "${before_digest}" ]]; then
    log_error "Direct activation did not install a distinct inactive-slot candidate: before=${before_snapshot} after=${after_snapshot}"
    return 1
  fi
  _agent_upgrade_assert_synchronized "${vm_ip}"
  _agent_upgrade_validate_kubelet_auth "${vm_name}" "${vm_ip}"
  validate_node_joined "${vm_name}"
  log_success "Direct host activation preflight, switch, service health, and nspawn synchronization passed"
}

agent_upgrade_e2e() {
  log_section "Managed and Direct AgentUpgrade E2E"
  local vm_name vm_ip suffix success_digest failure_digest before before_slot success_snapshot success_slot success_binary_digest rollback_snapshot rollback_binary_digest retry_snapshot retry_binary_digest
  vm_name="$(state_get token_vm_name)"
  vm_ip="$(state_get token_vm_ip)"
  suffix="$(date +%s)"

  validate_node_joined "${vm_name}"
  _agent_upgrade_ensure_api
  # MachineOperation discovery occurs during daemon startup. Restart after the
  # CRD is established so this focused command also works on previously joined
  # environments where the API was absent.
  remote_exec "${vm_ip}" 'sudo systemctl restart aks-flex-node-agent.service'
  validate_node_joined "${vm_name}"
  _agent_upgrade_prepare_server "${vm_ip}"
  success_digest="$(_agent_upgrade_digest "${vm_ip}" success.tar.gz)"
  failure_digest="$(_agent_upgrade_digest "${vm_ip}" failure.tar.gz)"
  before="$(_agent_upgrade_snapshot "${vm_ip}")"
  before_slot="$(cut -d'|' -f1 <<<"${before}")"
  log_info "Pre-upgrade agent snapshot: ${before}"

  local success_op="agent-upgrade-success-${suffix}"
  _agent_upgrade_apply "${success_op}" "${vm_name}" success.tar.gz "${success_digest}" "success-${suffix}"
  _agent_upgrade_wait_phase "${success_op}" Complete
  validate_node_joined "${vm_name}"
  _agent_upgrade_assert_synchronized "${vm_ip}"
  success_snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r success_slot _ success_binary_digest _ <<<"${success_snapshot}"
  if [[ -z "${success_slot}" || "${success_slot}" == "${before_slot}" ]]; then
    log_error "Successful AgentUpgrade did not switch the active binary slot: before=${before} after=${success_snapshot}"
    return 1
  fi

  # Restart kubelet and require a fresh Lease renewal so this proves the
  # synchronized nspawn exec-credential binary can still authenticate.
  _agent_upgrade_validate_kubelet_auth "${vm_name}" "${vm_ip}"

  local failure_op="agent-upgrade-rollback-${suffix}"
  _agent_upgrade_apply "${failure_op}" "${vm_name}" failure.tar.gz "${failure_digest}" "failure-${suffix}"
  _agent_upgrade_wait_phase "${failure_op}" Failed
  rollback_snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r _ _ rollback_binary_digest _ <<<"${rollback_snapshot}"
  if [[ "${rollback_binary_digest}" != "${success_binary_digest}" ]]; then
    log_error "Rollback did not restore the successful candidate: success=${success_snapshot} rollback=${rollback_snapshot}"
    return 1
  fi
  if kubectl get machineoperation "${failure_op}" -o jsonpath='{.status.message}' | grep -q "failure-${suffix}"; then
    log_error "AgentUpgrade status leaked sensitive URL query data"
    return 1
  fi
  remote_exec "${vm_ip}" 'sudo systemctl is-active --quiet aks-flex-node-agent.service'
  _agent_upgrade_assert_synchronized "${vm_ip}"
  validate_node_joined "${vm_name}"

  local retry_op="agent-upgrade-retry-${suffix}"
  # The digest is optional when the VM-local source and loopback transport
  # provide the trust boundary.
  _agent_upgrade_apply "${retry_op}" "${vm_name}" success.tar.gz "" "retry-${suffix}"
  _agent_upgrade_wait_phase "${retry_op}" Complete
  retry_snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r _ _ retry_binary_digest _ <<<"${retry_snapshot}"
  if [[ "${retry_binary_digest}" != "${success_binary_digest}" ]]; then
    log_error "Retry did not install the expected successful candidate: ${retry_snapshot}"
    return 1
  fi
  _agent_upgrade_assert_synchronized "${vm_ip}"
  validate_node_joined "${vm_name}"

  # A release before the host root would look for its files under /usr/local,
  # where this host has none, so verification refuses it and nothing changes.
  local legacy_op="agent-upgrade-legacy-${suffix}" legacy_digest refused_snapshot refused_binary_digest
  legacy_digest="$(_agent_upgrade_digest "${vm_ip}" legacy.tar.gz)"
  _agent_upgrade_apply "${legacy_op}" "${vm_name}" legacy.tar.gz "${legacy_digest}" "legacy-${suffix}"
  _agent_upgrade_wait_phase "${legacy_op}" Failed
  if ! kubectl get machineoperation "${legacy_op}" -o jsonpath='{.status.message}' | grep -q "predates the host root"; then
    log_error "AgentUpgrade to a release before the host root was not refused by verification"
    kubectl get machineoperation "${legacy_op}" -o yaml || true
    return 1
  fi
  refused_snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r _ _ refused_binary_digest _ <<<"${refused_snapshot}"
  if [[ "${refused_binary_digest}" != "${retry_binary_digest}" ]]; then
    log_error "A refused AgentUpgrade changed the active binary: ${refused_snapshot}"
    return 1
  fi
  remote_exec "${vm_ip}" 'sudo systemctl is-active --quiet aks-flex-node-agent.service'

  _agent_upgrade_direct_activation "${vm_name}" "${vm_ip}"
  smoke_test "${vm_name}" "agent-upgrade"

  log_success "Managed AgentUpgrade success/rollback/retry and direct host activation E2E passed"
}

# ---------------------------------------------------------------------------
# host_root_migration_e2e - Move a host installed by a release before the host
# root to the build under test
# ---------------------------------------------------------------------------
_host_root_migration_uninstall() {
  local vm_ip="$1"
  remote_copy "${REPO_ROOT}/scripts/uninstall.sh" "${vm_ip}" "/tmp/aks-flex-node-uninstall.sh"
  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
sudo bash /tmp/aks-flex-node-uninstall.sh --force
for path in /opt/unbounded /usr/local/bin/aks-flex-node /usr/local/lib/aks-flex-node /etc/aks-flex-node; do
  if [[ -e "${path}" || -L "${path}" ]]; then
    echo "uninstall left ${path}" >&2
    exit 1
  fi
done
REMOTE
}

# _host_root_migration_assert checks the host root. "legacy" is a host the older
# release installed, with nothing at /opt/unbounded. "migrated" is the same host
# once this build has run: /opt/unbounded links to /usr/local, and the unit and
# links the older release wrote are unchanged, so it could still be returned to.
_host_root_migration_assert() {
  local vm_ip="$1" state="$2"
  remote_exec "${vm_ip}" "STATE=${state} bash -s" <<'REMOTE'
set -euo pipefail
service=/etc/systemd/system/aks-flex-node-agent.service
case "${STATE}" in
  legacy)
    if [[ -e /opt/unbounded || -L /opt/unbounded ]]; then
      echo "a release before the host root created /opt/unbounded" >&2
      exit 1
    fi
    ;;
  migrated)
    if [[ ! -L /opt/unbounded || "$(readlink /opt/unbounded)" != /usr/local ]]; then
      echo "/opt/unbounded is not a link to /usr/local: $(ls -ld /opt/unbounded 2>&1)" >&2
      exit 1
    fi
    ;;
esac
[[ "$(readlink /usr/local/bin/aks-flex-node)" == /usr/local/lib/aks-flex-node/aks-flex-node-current ]] ||
  { echo "compatibility link is $(readlink /usr/local/bin/aks-flex-node)" >&2; exit 1; }
sudo grep -Fq "ExecStart=/usr/local/lib/aks-flex-node/aks-flex-node-current agent" "${service}" ||
  { echo "agent unit does not run the legacy current link:" >&2; sudo cat "${service}" >&2; exit 1; }
sudo systemctl is-active --quiet aks-flex-node-agent.service
REMOTE
}

host_root_migration_e2e() {
  local release="${E2E_LEGACY_RELEASE:-v0.2.0}"
  log_section "Host Root Migration from ${release}"
  local vm_name vm_ip suffix success_digest op snapshot slot
  vm_name="$(state_get token_vm_name)"
  vm_ip="$(state_get token_vm_ip)"
  suffix="$(date +%s)"

  # Start over from a host the older release installed.
  node_unjoin_token
  _host_root_migration_uninstall "${vm_ip}"
  node_join_token "${release}"
  validate_node_joined "${vm_name}"
  _host_root_migration_assert "${vm_ip}" legacy

  # The older daemon performs the upgrade. This build's daemon then starts on a
  # host it did not install, and links the host root to the older layout.
  _agent_upgrade_ensure_api
  remote_exec "${vm_ip}" 'sudo systemctl restart aks-flex-node-agent.service'
  validate_node_joined "${vm_name}"
  _agent_upgrade_prepare_server "${vm_ip}"
  success_digest="$(_agent_upgrade_digest "${vm_ip}" success.tar.gz)"
  op="host-root-migration-${suffix}"
  _agent_upgrade_apply "${op}" "${vm_name}" success.tar.gz "${success_digest}" "migration-${suffix}"
  _agent_upgrade_wait_phase "${op}" Complete
  validate_node_joined "${vm_name}"
  _host_root_migration_assert "${vm_ip}" migrated

  snapshot="$(_agent_upgrade_snapshot "${vm_ip}")"
  IFS='|' read -r slot _ _ _ <<<"${snapshot}"
  if [[ "${slot}" != /usr/local/lib/aks-flex-node/* ]]; then
    log_error "The migrated host is not running from the older layout: ${snapshot}"
    return 1
  fi
  _agent_upgrade_assert_synchronized "${vm_ip}"
  _agent_upgrade_validate_kubelet_auth "${vm_name}" "${vm_ip}"
  smoke_test "${vm_name}" "host-root-migration"

  log_success "Host root migration from ${release} passed"
}
