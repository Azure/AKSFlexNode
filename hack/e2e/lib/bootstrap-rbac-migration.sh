#!/usr/bin/env bash
# =============================================================================
# Real-node migration test for the legacy bootstrap-group system:node binding.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_BOOTSTRAP_RBAC_MIGRATION_LOADED:-}" ]] && return 0
readonly _E2E_BOOTSTRAP_RBAC_MIGRATION_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

readonly historicalReleaseTag="v0.1.0"
readonly historicalCommit="65d8d3896371adf2eb13248c3e73f0e83fd418ef"
readonly historicalArchiveName="aks-flex-node-linux-amd64.tar.gz"
readonly historicalBinaryName="aks-flex-node-linux-amd64"
readonly historicalArchiveURL="https://github.com/Azure/AKSFlexNode/releases/download/${historicalReleaseTag}/${historicalArchiveName}"
readonly historicalHelperURL="https://raw.githubusercontent.com/Azure/AKSFlexNode/${historicalCommit}/scripts/aks-flex-config"
readonly historicalInstallerURL="https://raw.githubusercontent.com/Azure/AKSFlexNode/${historicalCommit}/scripts/install.sh"
readonly historicalArchiveSHA256="50922e15999b2fd9c19a298c3c3d9cb5a0a375858258dc42b92699a4427876e6"
readonly historicalBinarySHA256="50b4a62daeb30e635cc8ea5e18c6a204e54510e164eba75fe0322a8a8d56fdc7"
readonly historicalHelperSHA256="8ae38209e1a63f1b3c9d1fb41423644de5de032c3a978cedef100689fe1f19f7"
readonly historicalInstallerSHA256="c2f7cfc92e62c3a9fb96697b3a3ca170bb8c28d11af8f45d0f26f6fce016e9b4"
# v0.1.0 embeds Unbounded v0.1.8, whose non-GPU default resolves to this
# immutable release tag. The old Flex config schema cannot override OCIImage.
readonly historicalRootFS="ghcr.io/azure/agent-ubuntu2404:v20260427"
readonly legacyNodeRoleBinding="aks-flex-node-role"
readonly flexNodeBootstrapGroup="system:bootstrappers:aks-flex-node"
# client-go's rotating FileStore writes the issued certificate and private key
# into this combined PEM. client.crt/client.key are legacy read-only fallbacks.
readonly daemonCredentialPath="/etc/aks-flex-node/daemon-credentials/daemon-controller-current.pem"

_historical_artifact_dir() {
  echo "${E2E_WORK_DIR}/historical-${historicalReleaseTag}"
}

_verify_sha256() {
  local path="$1"
  local expected="$2"
  local description="$3"
  local actual

  actual="$(sha256sum "${path}" | awk '{print $1}')"
  if [[ "${actual}" != "${expected}" ]]; then
    log_error "${description} SHA-256 mismatch: got ${actual}, want ${expected}"
    return 1
  fi
}

_download_verified_artifact() {
  local url="$1"
  local path="$2"
  local expected="$3"
  local description="$4"
  local partial="${path}.download"

  if [[ -f "${path}" ]] && _verify_sha256 "${path}" "${expected}" "${description}"; then
    log_info "Using cached, verified ${description}: ${path}"
    return 0
  fi

  rm -f "${path}" "${partial}"
  log_info "Downloading pinned ${description}"
  if ! curl --fail --location --proto '=https' --retry 5 --retry-all-errors \
    --silent --show-error --output "${partial}" "${url}"; then
    rm -f "${partial}"
    return 1
  fi
  if ! _verify_sha256 "${partial}" "${expected}" "${description}"; then
    rm -f "${partial}"
    return 1
  fi
  mv "${partial}" "${path}"
}

_prepare_historical_artifacts() {
  require_cmd curl
  require_cmd sha256sum
  require_cmd tar

  local artifact_dir archive helper installer binary archive_members version_output
  artifact_dir="$(_historical_artifact_dir)"
  archive="${artifact_dir}/${historicalArchiveName}"
  helper="${artifact_dir}/aks-flex-config"
  installer="${artifact_dir}/install.sh"
  binary="${artifact_dir}/${historicalBinaryName}"
  mkdir -p "${artifact_dir}"

  _download_verified_artifact \
    "${historicalArchiveURL}" "${archive}" "${historicalArchiveSHA256}" \
    "${historicalReleaseTag} release archive"
  _download_verified_artifact \
    "${historicalHelperURL}" "${helper}" "${historicalHelperSHA256}" \
    "${historicalReleaseTag} aks-flex-config helper"
  _download_verified_artifact \
    "${historicalInstallerURL}" "${installer}" "${historicalInstallerSHA256}" \
    "${historicalReleaseTag} installer"

  archive_members="$(tar -tzf "${archive}")"
  if [[ "${archive_members}" != "${historicalBinaryName}" ]]; then
    log_error "${historicalReleaseTag} archive has unexpected members: ${archive_members}"
    return 1
  fi
  rm -f "${binary}"
  tar --extract --gzip --file "${archive}" --directory "${artifact_dir}" \
    --no-same-owner --no-same-permissions "${historicalBinaryName}"
  _verify_sha256 "${binary}" "${historicalBinarySHA256}" \
    "${historicalReleaseTag} extracted binary"
  chmod 0755 "${binary}" "${helper}" "${installer}"

  version_output="$("${binary}" version)"
  if [[ "${version_output}" != *"Version: ${historicalReleaseTag}"* || \
        "${version_output}" != *"Git Commit: ${historicalCommit:0:7}"* ]]; then
    log_error "Pinned historical binary reports unexpected build metadata: ${version_output}"
    return 1
  fi

  log_success "Verified official ${historicalReleaseTag} archive, binary, helper, and installer"
  log_info "Historical binary's pinned default rootfs: ${historicalRootFS}"
}

_historical_config_path() {
  echo "${E2E_WORK_DIR}/config-token-${historicalReleaseTag}.json"
}

_head_legacy_config_path() {
  echo "${E2E_WORK_DIR}/config-token-${historicalReleaseTag}-head.json"
}

_historical_token_id() {
  local config_file="$1"
  jq -er \
    '.azure.bootstrapToken.token | capture("^(?<id>[a-z0-9]{6})\\.[a-z0-9]{16}$").id' \
    "${config_file}"
}

_generate_historical_config() {
  local artifact_dir helper config_file
  local vm_name vm_private_ip cluster_name resource_group subscription_id
  artifact_dir="$(_historical_artifact_dir)"
  helper="${artifact_dir}/aks-flex-config"
  config_file="$(_historical_config_path)"
  vm_name="$(state_get token_vm_name)"
  vm_private_ip="$(state_get token_vm_private_ip)"
  cluster_name="$(state_get cluster_name)"
  resource_group="$(state_get resource_group)"
  subscription_id="$(state_get subscription_id)"

  if [[ -z "${vm_private_ip}" ]] || ! is_valid_ipv4 "${vm_private_ip}"; then
    log_error "Invalid token VM private IP in state: '${vm_private_ip}'"
    return 1
  fi
  if kubectl get clusterrolebinding "${legacyNodeRoleBinding}" >/dev/null 2>&1; then
    log_error "Historical scenario requires a fresh cluster without ${legacyNodeRoleBinding}"
    return 1
  fi

  log_info "Applying the real ${historicalReleaseTag} cluster-side RBAC"
  with_cluster_lock python3 "${helper}" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}"

  log_info "Generating a real non-expiring ${historicalReleaseTag} bootstrap token and config"
  with_cluster_lock python3 "${helper}" generate-node-config \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" \
    --bootstrap-token \
    --output "${config_file}"

  jq \
    --arg nodeName "${vm_name}" \
    --arg nodeIP "${vm_private_ip}" \
    --arg kubernetesVersion "${E2E_KUBERNETES_VERSION}" \
    --arg containerdVersion "${E2E_CONTAINERD_VERSION}" \
    --arg runcVersion "${E2E_RUNC_VERSION}" \
    '.agent.logLevel = "debug"
      | .agent.nodeName = $nodeName
      | .node.kubelet.nodeIP = $nodeIP
      | .kubernetes.version = $kubernetesVersion
      | .containerd.version = $containerdVersion
      | .runc.version = $runcVersion' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"
  chmod 0600 "${config_file}"

  if ! jq -e \
    --arg nodeName "${vm_name}" \
    --arg nodeIP "${vm_private_ip}" \
    '.agent.nodeName == $nodeName
      and (.agent | has("e2eMode") | not)
      and (.agent | has("machineOperationMode") | not)
      and .node.kubelet.nodeIP == $nodeIP
      and (.node.kubelet.serverURL | length > 0)
      and (.node.kubelet.caCertData | length > 0)
      and (.kubernetes.version | length > 0)
      and (has("components") | not)' \
    "${config_file}" >/dev/null; then
    log_error "Historical helper did not produce the expected legacy config shape"
    return 1
  fi
}

_require_historical_cluster_state() {
  local config_file="$1"
  local token_id secret
  token_id="$(_historical_token_id "${config_file}")"

  if ! kubectl get clusterrolebinding "${legacyNodeRoleBinding}" -o json | jq -e \
    --arg group "${flexNodeBootstrapGroup}" \
    '.roleRef.apiGroup == "rbac.authorization.k8s.io"
      and .roleRef.kind == "ClusterRole"
      and .roleRef.name == "system:node"
      and any(.subjects[]?;
        .apiGroup == "rbac.authorization.k8s.io"
        and .kind == "Group"
        and .name == $group)' >/dev/null; then
    log_error "${historicalReleaseTag} helper did not create the expected legacy node-role binding"
    return 1
  fi

  secret="$(kubectl -n kube-system get secret "bootstrap-token-${token_id}" -o json)"
  if ! jq -e \
    '(.data.expiration // "") == ""
      and (.metadata.labels["kubernetes.azure.com/managedby"] // "") == ""' \
    <<<"${secret}" >/dev/null; then
    log_error "Historical bootstrap Secret unexpectedly has expiration or AKS ownership metadata"
    return 1
  fi
  log_success "Verified ${historicalReleaseTag} legacy RBAC and non-expiring, unmanaged token state"
}

_install_and_start_historical_node() {
  local vm_ip="$1"
  local artifact_dir config_file
  artifact_dir="$(_historical_artifact_dir)"
  config_file="$(_historical_config_path)"

  remote_copy "${artifact_dir}/${historicalArchiveName}" "${vm_ip}" "/tmp/${historicalArchiveName}"
  remote_copy "${artifact_dir}/aks-flex-config" "${vm_ip}" "/tmp/aks-flex-config-${historicalReleaseTag}"
  remote_copy "${artifact_dir}/install.sh" "${vm_ip}" "/tmp/aks-flex-node-install-${historicalReleaseTag}.sh"
  remote_copy "${config_file}" "${vm_ip}" "/tmp/config-${historicalReleaseTag}.json"

  remote_exec "${vm_ip}" \
    "HISTORICAL_TAG=${historicalReleaseTag} HISTORICAL_COMMIT=${historicalCommit:0:7} HISTORICAL_ARCHIVE_SHA256=${historicalArchiveSHA256} HISTORICAL_BINARY_SHA256=${historicalBinarySHA256} HISTORICAL_HELPER_SHA256=${historicalHelperSHA256} HISTORICAL_INSTALLER_SHA256=${historicalInstallerSHA256} E2E_NODE_JOIN_TIMEOUT=${E2E_NODE_JOIN_TIMEOUT} DAEMON_CREDENTIAL_PATH=${daemonCredentialPath} bash -s" <<'REMOTE'
set -euo pipefail

archive=/tmp/aks-flex-node-linux-amd64.tar.gz
helper="/tmp/aks-flex-config-${HISTORICAL_TAG}"
installer="/tmp/aks-flex-node-install-${HISTORICAL_TAG}.sh"
config="/tmp/config-${HISTORICAL_TAG}.json"
extract_dir="/tmp/aks-flex-node-${HISTORICAL_TAG}"
binary="${extract_dir}/aks-flex-node-linux-amd64"

printf '%s  %s\n' "${HISTORICAL_ARCHIVE_SHA256}" "${archive}" | sha256sum --check --strict -
printf '%s  %s\n' "${HISTORICAL_HELPER_SHA256}" "${helper}" | sha256sum --check --strict -
printf '%s  %s\n' "${HISTORICAL_INSTALLER_SHA256}" "${installer}" | sha256sum --check --strict -

archive_members="$(tar -tzf "${archive}")"
if [[ "${archive_members}" != "aks-flex-node-linux-amd64" ]]; then
  echo "historical archive has unexpected members: ${archive_members}" >&2
  exit 1
fi
sudo rm -rf "${extract_dir}"
mkdir -p "${extract_dir}"
tar --extract --gzip --file "${archive}" --directory "${extract_dir}" \
  --no-same-owner --no-same-permissions aks-flex-node-linux-amd64
printf '%s  %s\n' "${HISTORICAL_BINARY_SHA256}" "${binary}" | sha256sum --check --strict -
chmod 0755 "${binary}" "${helper}" "${installer}"

version_output="$("${binary}" version)"
grep -Fq "Version: ${HISTORICAL_TAG}" <<<"${version_output}"
grep -Fq "Git Commit: ${HISTORICAL_COMMIT}" <<<"${version_output}"

if sudo test -e /usr/local/lib/aks-flex-node/aks-flex-node-current || \
  sudo test -L /usr/local/lib/aks-flex-node/aks-flex-node-current; then
  echo "historical test VM already has a managed agent layout" >&2
  exit 1
fi

if command -v apt-get >/dev/null 2>&1; then
  packages_installed=0
  for attempt in $(seq 1 5); do
    if sudo DEBIAN_FRONTEND=noninteractive apt-get \
        -o Acquire::Retries=5 -o Acquire::http::Timeout=30 update &&
      sudo DEBIAN_FRONTEND=noninteractive apt-get \
        -o Acquire::Retries=5 -o Acquire::http::Timeout=30 \
        install -y --fix-missing ca-certificates curl nftables systemd-container util-linux; then
      packages_installed=1
      break
    fi
    echo "Host package installation failed; retrying (${attempt}/5)..."
    sleep 10
  done
  if (( packages_installed != 1 )); then
    echo "Host package installation failed after retries" >&2
    exit 1
  fi
fi

sudo AKS_FLEX_NODE_LOCAL_BINARY="${binary}" \
  AKS_FLEX_NODE_VERSION="${HISTORICAL_TAG}" \
  SKIP_AZCLI=true \
  bash "${installer}" --yes
sudo install -m 0600 "${config}" /etc/aks-flex-node/config.json

if [[ -L /usr/local/bin/aks-flex-node ]]; then
  echo "${HISTORICAL_TAG} installer unexpectedly created a managed compatibility symlink" >&2
  exit 1
fi
printf '%s  %s\n' "${HISTORICAL_BINARY_SHA256}" /usr/local/bin/aks-flex-node | sudo sha256sum --check --strict -
if sudo test -e "${DAEMON_CREDENTIAL_PATH}"; then
  echo "daemon certificate existed before historical bootstrap" >&2
  exit 1
fi

unit=aks-flex-node-historical-bootstrap
sudo systemctl stop "${unit}.service" 2>/dev/null || true
sudo systemctl reset-failed "${unit}.service" 2>/dev/null || true
sudo systemd-run \
  --unit="${unit}" \
  --description="AKS Flex Node ${HISTORICAL_TAG} E2E" \
  --remain-after-exit \
  /usr/local/bin/aks-flex-node bootstrap --config /etc/aks-flex-node/config.json

deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
while ! sudo systemctl is-active --quiet aks-flex-node-agent.service; do
  if sudo systemctl is-failed --quiet "${unit}.service"; then
    sudo systemctl status "${unit}.service" --no-pager -l >&2 || true
    sudo journalctl -u "${unit}.service" -n 100 --no-pager >&2 || true
    exit 1
  fi
  if (( SECONDS >= deadline )); then
    echo "timed out waiting for historical daemon service" >&2
    sudo systemctl status "${unit}.service" --no-pager -l >&2 || true
    sudo systemctl status aks-flex-node-agent.service --no-pager -l >&2 || true
    sudo journalctl -u "${unit}.service" -n 100 --no-pager >&2 || true
    sudo journalctl -u aks-flex-node-agent.service -n 100 --no-pager >&2 || true
    exit 1
  fi
  sleep 5
done

sleep 5
sudo systemctl is-active --quiet aks-flex-node-agent.service
sudo grep -Fq 'ExecStart=/usr/local/bin/aks-flex-node agent' \
  /etc/systemd/system/aks-flex-node-agent.service
historical_logs="$(sudo journalctl -u aks-flex-node-agent.service --no-pager)"
grep -Fq 'production agent daemon requires AKS RP machine client implementation' \
  <<<"${historical_logs}"
if grep -Fq 'running agent daemon in e2e mode' <<<"${historical_logs}"; then
  echo "${HISTORICAL_TAG} daemon unexpectedly started in E2E mode" >&2
  exit 1
fi
if sudo test -e "${DAEMON_CREDENTIAL_PATH}"; then
  echo "${HISTORICAL_TAG} unexpectedly issued a daemon certificate" >&2
  exit 1
fi
REMOTE
}

_bootstrap_token_api_probe() {
  local config_file="$1"
  local probe="$2"

  python3 - "${config_file}" "${probe}" <<'PY'
import base64
import json
import ssl
import sys
import urllib.error
import urllib.request

config_path, probe = sys.argv[1:]
with open(config_path, encoding="utf-8") as stream:
    config = json.load(stream)

token = config["azure"]["bootstrapToken"]["token"]
kubelet = config["node"]["kubelet"]
server = kubelet.get("serverURL")
if not server:
    cluster_fqdn = kubelet["clusterFQDN"]
    server = cluster_fqdn if "://" in cluster_fqdn else "https://" + cluster_fqdn
ca_pem = base64.b64decode(kubelet["caCertData"], validate=True).decode("ascii")
context = ssl.create_default_context(cadata=ca_pem)
headers = {"Authorization": "Bearer " + token}

if probe == "list-nodes":
    request = urllib.request.Request(server.rstrip("/") + "/api/v1/nodes?limit=1", headers=headers)
elif probe == "create-csr":
    body = json.dumps(
        {
            "apiVersion": "authorization.k8s.io/v1",
            "kind": "SelfSubjectAccessReview",
            "spec": {
                "resourceAttributes": {
                    "group": "certificates.k8s.io",
                    "resource": "certificatesigningrequests",
                    "verb": "create",
                }
            },
        }
    ).encode("utf-8")
    request = urllib.request.Request(
        server.rstrip("/") + "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
        data=body,
        headers={**headers, "Content-Type": "application/json"},
        method="POST",
    )
else:
    raise SystemExit("unsupported probe: " + probe)

try:
    with urllib.request.urlopen(request, context=context, timeout=30) as response:
        response_body = response.read()
        status = response.status
except urllib.error.HTTPError as error:
    print("http:" + str(error.code))
    raise SystemExit(0)

if probe == "create-csr":
    review = json.loads(response_body)
    print("allowed" if review.get("status", {}).get("allowed") is True else "denied")
else:
    print("http:" + str(status))
PY
}

_wait_for_bootstrap_token_probe() {
  local expected="$1"
  local config_file="$2"
  local probe="$3"
  local elapsed=0
  local actual=""

  while (( elapsed < E2E_NODE_JOIN_TIMEOUT )); do
    if actual="$(_bootstrap_token_api_probe "${config_file}" "${probe}")" && \
      [[ "${actual}" == "${expected}" ]]; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done

  log_error "Bootstrap token probe ${probe} returned '${actual}', want '${expected}' after ${E2E_NODE_JOIN_TIMEOUT}s"
  return 1
}

_kubelet_client_identity() {
  local vm_ip="$1"

  remote_exec "${vm_ip}" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
machine="$(python3 - <<'PY'
import json
with open('/etc/aks-flex-node/daemon-state.json', encoding='utf-8') as stream:
    print(json.load(stream)['activeMachine'])
PY
)"
systemd-run --machine="${machine}" --quiet --pipe --wait \
  openssl x509 \
    -in /var/lib/kubelet/pki/kubelet-client-current.pem \
    -noout \
    -subject \
    -nameopt RFC2253
REMOTE
}

_daemon_client_identity() {
  local vm_ip="$1"
  remote_exec "${vm_ip}" \
    "sudo openssl x509 -in ${daemonCredentialPath} -noout -subject -nameopt RFC2253"
}

_require_daemon_certificate_access() {
  local vm_ip="$1"
  local server_url="$2"
  local quoted_server quoted_credential
  printf -v quoted_server '%q' "${server_url}"
  printf -v quoted_credential '%q' "${daemonCredentialPath}"

  remote_exec "${vm_ip}" \
    "SERVER_URL=${quoted_server} DAEMON_CREDENTIAL_PATH=${quoted_credential} bash -s" <<'REMOTE'
set -euo pipefail
ca_file="$(mktemp)"
trap 'rm -f "${ca_file}"' EXIT
sudo python3 - "${ca_file}" <<'PY'
import base64
import json
import sys

with open('/etc/aks-flex-node/config.json', encoding='utf-8') as stream:
    ca_data = json.load(stream)['node']['kubelet']['caCertData']
with open(sys.argv[1], 'wb') as stream:
    stream.write(base64.b64decode(ca_data, validate=True))
PY
status="$(sudo curl --silent --show-error \
  --cert "${DAEMON_CREDENTIAL_PATH}" \
  --key "${DAEMON_CREDENTIAL_PATH}" \
  --cacert "${ca_file}" \
  --output /dev/null \
  --write-out '%{http_code}' \
  "${SERVER_URL}/api/v1/nodes?limit=1")"
if [[ "${status}" != "200" ]]; then
  echo "daemon client certificate LIST nodes returned HTTP ${status}, want 200" >&2
  exit 1
fi
REMOTE
}

_require_old_node_survives_guard() {
  local vm_name="$1"
  local vm_ip="$2"

  remote_exec "${vm_ip}" \
    "DAEMON_CREDENTIAL_PATH=${daemonCredentialPath} bash -s" <<'REMOTE'
set -euo pipefail
sudo systemctl restart aks-flex-node-agent.service
for _ in $(seq 1 30); do
  if sudo systemctl is-active --quiet aks-flex-node-agent.service; then
    first_pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
    sleep 5
    second_pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
    if sudo systemctl is-active --quiet aks-flex-node-agent.service &&
      [[ "${first_pid}" =~ ^[1-9][0-9]*$ ]] && [[ "${second_pid}" == "${first_pid}" ]]; then
      break
    fi
  fi
  sleep 2
done
if ! sudo systemctl is-active --quiet aks-flex-node-agent.service ||
  [[ "${second_pid:-}" != "${first_pid:-}" ]]; then
  sudo systemctl status aks-flex-node-agent.service --no-pager -l >&2 || true
  exit 1
fi
if sudo test -e "${DAEMON_CREDENTIAL_PATH}"; then
  echo "historical daemon unexpectedly created a daemon certificate" >&2
  exit 1
fi
REMOTE
  validate_node_joined "${vm_name}"
}

_prepare_head_legacy_config() {
  local source_config="$1"
  local target_config="$2"

  jq \
    --arg machineEndpointURL "${E2E_CONTROLLER_SERVICE_PROXY_PATH}" \
    --arg agentPoolName "${E2E_TARGET_AGENT_POOL_NAME}" \
    --arg ociImage "${historicalRootFS}" \
    '.agent.machineClient.mode = "in-cluster"
      | .agent.machineClient.endpointUrl = $machineEndpointURL
      | .agent.requireMachineRegistration = true
      | .agent.machineOperationMode = "disable"
      | .azure.targetAgentPoolName = $agentPoolName
      | .bootstrap.ociImage = $ociImage' \
    "${source_config}" > "${target_config}"
  chmod 0600 "${target_config}"

  if ! jq -e \
    --arg endpoint "${E2E_CONTROLLER_SERVICE_PROXY_PATH}" \
    --arg ociImage "${historicalRootFS}" \
    '.agent.machineClient.mode == "in-cluster"
      and .agent.machineClient.endpointUrl == $endpoint
      and .agent.requireMachineRegistration == true
      and .agent.machineOperationMode == "disable"
      and .bootstrap.ociImage == $ociImage
      and (.node.kubelet.serverURL | length > 0)
      and (.kubernetes.version | length > 0)
      and (has("components") | not)' \
    "${target_config}" >/dev/null; then
    log_error "HEAD upgrade config no longer preserves the historical config fields"
    return 1
  fi
}

_upgrade_historical_node_to_head() {
  local vm_ip="$1"
  local config_file="$2"
  local head_sha
  head_sha="$(sha256sum "${E2E_BINARY}" | awk '{print $1}')"

  remote_copy "${E2E_BINARY}" "${vm_ip}" /tmp/aks-flex-node-head
  remote_copy "${config_file}" "${vm_ip}" /tmp/config-v0.1.0-head.json

  remote_exec "${vm_ip}" \
    "HISTORICAL_BINARY_SHA256=${historicalBinarySHA256} HEAD_BINARY_SHA256=${head_sha} E2E_NODE_JOIN_TIMEOUT=${E2E_NODE_JOIN_TIMEOUT} DAEMON_CREDENTIAL_PATH=${daemonCredentialPath} bash -s" <<'REMOTE'
set -euo pipefail
candidate=/tmp/aks-flex-node-head
current_link=/usr/local/lib/aks-flex-node/aks-flex-node-current
last_good_link=/usr/local/lib/aks-flex-node/aks-flex-node-last-good
service=/etc/systemd/system/aks-flex-node-agent.service

chmod 0755 "${candidate}"
printf '%s  %s\n' "${HEAD_BINARY_SHA256}" "${candidate}" | sha256sum --check --strict -
printf '%s  %s\n' "${HISTORICAL_BINARY_SHA256}" /usr/local/bin/aks-flex-node | sudo sha256sum --check --strict -
if sudo test -e "${current_link}" || sudo test -L "${current_link}"; then
  echo "managed layout existed before migration preflight" >&2
  exit 1
fi

sudo install -m 0600 /tmp/config-v0.1.0-head.json /etc/aks-flex-node/config.json
sudo "${candidate}" agent-upgrade --preflight | sudo tee /tmp/historical-agent-upgrade-preflight.log

# Preflight must not mutate the direct v0.1.0 installation.
if sudo test -e "${current_link}" || sudo test -L "${current_link}" || \
  sudo test -L /usr/local/bin/aks-flex-node; then
  echo "agent-upgrade preflight mutated the legacy binary layout" >&2
  exit 1
fi
printf '%s  %s\n' "${HISTORICAL_BINARY_SHA256}" /usr/local/bin/aks-flex-node | sudo sha256sum --check --strict -
sudo systemctl is-active --quiet aks-flex-node-agent.service

sudo "${candidate}" agent-upgrade | sudo tee /tmp/historical-agent-upgrade.log

deadline=$((SECONDS + E2E_NODE_JOIN_TIMEOUT))
while true; do
  if sudo systemctl is-active --quiet aks-flex-node-agent.service &&
    sudo test -s "${DAEMON_CREDENTIAL_PATH}"; then
    first_pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
    sleep 5
    second_pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
    if sudo systemctl is-active --quiet aks-flex-node-agent.service &&
      [[ "${first_pid}" =~ ^[1-9][0-9]*$ ]] && [[ "${second_pid}" == "${first_pid}" ]]; then
      break
    fi
  fi
  if (( SECONDS >= deadline )); then
    echo "HEAD daemon did not become stable with daemon credentials" >&2
    sudo systemctl status aks-flex-node-agent.service --no-pager -l >&2 || true
    sudo journalctl -u aks-flex-node-agent.service -n 150 --no-pager >&2 || true
    exit 1
  fi
  sleep 2
done

for link in /usr/local/bin/aks-flex-node "${current_link}" "${last_good_link}"; do
  if ! sudo test -L "${link}"; then
    echo "managed binary link missing after upgrade: ${link}" >&2
    exit 1
  fi
done
active="$(sudo readlink -f "${current_link}")"
last_good="$(sudo readlink -f "${last_good_link}")"
printf '%s  %s\n' "${HEAD_BINARY_SHA256}" "${active}" | sudo sha256sum --check --strict -
printf '%s  %s\n' "${HISTORICAL_BINARY_SHA256}" "${last_good}" | sudo sha256sum --check --strict -
if [[ "$(sudo readlink -f /usr/local/bin/aks-flex-node)" != "${active}" ]]; then
  echo "compatibility path does not resolve to the active managed binary" >&2
  exit 1
fi
sudo grep -Fq "ExecStart=${current_link} agent" "${service}"
pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
if [[ "$(sudo readlink -f "/proc/${pid}/exe")" != "${active}" ]]; then
  echo "daemon is not executing the activated HEAD binary" >&2
  exit 1
fi

cert_pub="$(sudo openssl x509 -in "${DAEMON_CREDENTIAL_PATH}" -pubkey -noout | sha256sum | awk '{print $1}')"
key_pub="$(sudo openssl pkey -in "${DAEMON_CREDENTIAL_PATH}" -pubout | sha256sum | awk '{print $1}')"
if [[ "${cert_pub}" != "${key_pub}" ]]; then
  echo "daemon certificate and private key do not match" >&2
  exit 1
fi
REMOTE
}

_revoke_historical_bootstrap_token() {
  local config_file="$1"
  local token_id
  token_id="$(_historical_token_id "${config_file}")"
  kubectl delete secret "bootstrap-token-${token_id}" -n kube-system
  log_info "Revoked the historical bootstrap token after certificate migration"
}

_restart_daemon_and_require_certificate_access() {
  local vm_ip="$1"
  local server_url="$2"

  remote_exec "${vm_ip}" 'bash -s' <<'REMOTE'
set -euo pipefail
sudo systemctl restart aks-flex-node-agent.service
for _ in $(seq 1 30); do
  if sudo systemctl is-active --quiet aks-flex-node-agent.service; then
    first_pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
    sleep 5
    second_pid="$(sudo systemctl show --property MainPID --value aks-flex-node-agent.service)"
    if sudo systemctl is-active --quiet aks-flex-node-agent.service &&
      [[ "${first_pid}" =~ ^[1-9][0-9]*$ ]] && [[ "${second_pid}" == "${first_pid}" ]]; then
      exit 0
    fi
  fi
  sleep 2
done
sudo systemctl status aks-flex-node-agent.service --no-pager -l >&2 || true
sudo journalctl -u aks-flex-node-agent.service -n 100 --no-pager >&2 || true
exit 1
REMOTE
  _require_daemon_certificate_access "${vm_ip}" "${server_url}"
}

_restart_kubelet_and_require_lease_renewal() {
  local vm_name="$1"
  local vm_ip="$2"
  local node_uid="$3"
  local before_renew
  local elapsed=0

  before_renew="$(kubectl get lease "${vm_name}" -n kube-node-lease -o jsonpath='{.spec.renewTime}')"
  remote_exec "${vm_ip}" 'sudo bash -s' <<'REMOTE'
set -euo pipefail
machine="$(python3 - <<'PY'
import json
with open('/etc/aks-flex-node/daemon-state.json', encoding='utf-8') as stream:
    print(json.load(stream)['activeMachine'])
PY
)"
systemd-run --machine="${machine}" --quiet --pipe --wait systemctl restart kubelet.service
REMOTE

  while (( elapsed < E2E_NODE_JOIN_TIMEOUT )); do
    local current_uid renew ready
    current_uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
    renew="$(kubectl get lease "${vm_name}" -n kube-node-lease -o jsonpath='{.spec.renewTime}' 2>/dev/null || true)"
    ready="$(kubectl get node "${vm_name}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
    if [[ "${current_uid}" == "${node_uid}" && -n "${renew}" && \
      "${renew}" != "${before_renew}" && "${ready}" == "True" ]]; then
      log_success "Historical node renewed its Lease and stayed Ready after token revocation"
      return 0
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  log_error "Historical node did not reauthenticate after token revocation"
  kubectl get node "${vm_name}" -o wide 2>&1 || true
  kubectl describe node "${vm_name}" 2>&1 || true
  return 1
}

historical_rbac_migration_e2e() {
  log_section "Historical ${historicalReleaseTag} Node and Bootstrap RBAC Migration"

  local config_file head_config vm_name vm_ip cluster_name resource_group subscription_id
  local server_url node_uid identity guard_output token token_id
  config_file="$(_historical_config_path)"
  head_config="$(_head_legacy_config_path)"
  vm_name="$(state_get token_vm_name)"
  vm_ip="$(state_get token_vm_ip)"
  cluster_name="$(state_get cluster_name)"
  resource_group="$(state_get resource_group)"
  subscription_id="$(state_get subscription_id)"
  server_url="$(state_get server_url)"

  _prepare_historical_artifacts
  _generate_historical_config
  _require_historical_cluster_state "${config_file}"
  _install_and_start_historical_node "${vm_ip}"
  validate_node_joined "${vm_name}"

  node_uid="$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')"
  identity="$(_kubelet_client_identity "${vm_ip}")"
  if [[ "${identity}" != *"CN=system:node:${vm_name}"* ]]; then
    log_error "${historicalReleaseTag} kubelet has unexpected client identity: ${identity}"
    return 1
  fi
  _wait_for_bootstrap_token_probe http:200 "${config_file}" list-nodes
  log_success "Official ${historicalReleaseTag} node is Ready with legacy RBAC and an issued kubelet certificate"

  # HEAD must converge the safe CSR bindings but preserve the old daemon until
  # an operator explicitly confirms the migration.
  if guard_output="$(with_cluster_lock "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" 2>&1)"; then
    log_error "HEAD RBAC setup accepted a legacy binding without explicit migration"
    return 1
  fi
  if [[ "${guard_output}" != *"--remove-legacy-node-role-binding"* ]]; then
    log_error "HEAD compatibility guard did not explain the explicit migration path"
    return 1
  fi
  if ! kubectl get clusterrolebinding "${legacyNodeRoleBinding}" >/dev/null 2>&1; then
    log_error "HEAD compatibility guard removed the legacy binding"
    return 1
  fi
  _wait_for_bootstrap_token_probe http:200 "${config_file}" list-nodes
  _require_old_node_survives_guard "${vm_name}" "${vm_ip}"
  log_success "HEAD fail-closed guard preserved the running historical node"

  # Helpers before v0.1.1 did not add the ownership label required by the
  # production AKS managed CSR approver. Adopt this known E2E token explicitly
  # before the HEAD daemon requests its dedicated certificate.
  token="$(jq -er '.azure.bootstrapToken.token' "${config_file}")"
  token_id="$(_historical_token_id "${config_file}")"
  with_cluster_lock mark_e2e_bootstrap_token_aks_managed "${token}"
  unset token
  if [[ "$(kubectl -n kube-system get secret "bootstrap-token-${token_id}" \
    -o jsonpath='{.metadata.labels.kubernetes\.azure\.com/managedby}')" != "aks" ]]; then
    log_error "Historical token adoption label was not applied"
    return 1
  fi

  machine_configmap_upsert "${vm_name}" "${E2E_KUBERNETES_VERSION}" "${E2E_KUBERNETES_VERSION}"
  _prepare_head_legacy_config "${config_file}" "${head_config}"
  _upgrade_historical_node_to_head "${vm_ip}" "${head_config}"
  validate_node_joined "${vm_name}"

  identity="$(_daemon_client_identity "${vm_ip}")"
  if [[ "${identity}" != *"CN=system:node:${vm_name}"* || \
        "${identity}" != *"O=aks-flex-node-daemons"* ]]; then
    log_error "Upgraded daemon has unexpected issued client identity: ${identity}"
    return 1
  fi
  _require_daemon_certificate_access "${vm_ip}" "${server_url}"
  log_success "Same host upgraded from direct ${historicalReleaseTag} layout to HEAD and obtained daemon credentials"

  # The first run removes the canonical historical object. The second verifies
  # that the explicitly requested migration is idempotent.
  with_cluster_lock "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" \
    --remove-legacy-node-role-binding
  with_cluster_lock "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" \
    --remove-legacy-node-role-binding

  if kubectl get clusterrolebinding "${legacyNodeRoleBinding}" >/dev/null 2>&1; then
    log_error "Legacy binding '${legacyNodeRoleBinding}' still exists after migration"
    return 1
  fi
  _wait_for_bootstrap_token_probe http:403 "${config_file}" list-nodes
  _wait_for_bootstrap_token_probe allowed "${config_file}" create-csr
  _require_daemon_certificate_access "${vm_ip}" "${server_url}"

  # Revocation proves the subsequent restarts cannot silently fall back to the
  # bootstrap credential.
  with_cluster_lock _revoke_historical_bootstrap_token "${config_file}"
  _wait_for_bootstrap_token_probe http:401 "${config_file}" list-nodes

  _restart_kubelet_and_require_lease_renewal "${vm_name}" "${vm_ip}" "${node_uid}"
  _restart_daemon_and_require_certificate_access "${vm_ip}" "${server_url}"
  validate_node_joined "${vm_name}"

  identity="$(_kubelet_client_identity "${vm_ip}")"
  if [[ "${identity}" != *"CN=system:node:${vm_name}"* ]]; then
    log_error "Kubelet lost its node client identity after migration: ${identity}"
    return 1
  fi
  identity="$(_daemon_client_identity "${vm_ip}")"
  if [[ "${identity}" != *"CN=system:node:${vm_name}"* || \
        "${identity}" != *"O=aks-flex-node-daemons"* ]]; then
    log_error "Daemon lost its issued client identity after migration: ${identity}"
    return 1
  fi
  if [[ "$(kubectl get node "${vm_name}" -o jsonpath='{.metadata.uid}')" != "${node_uid}" ]]; then
    log_error "Historical node object was replaced during migration"
    return 1
  fi

  log_success "Historical ${historicalReleaseTag} node upgraded in place, migrated RBAC twice, revoked its token, and survived kubelet/daemon restarts"
}
