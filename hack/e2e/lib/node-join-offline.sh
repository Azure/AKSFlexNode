#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/node-join-offline.sh - Join / unjoin an AKS flex node using
#                                      bootstrap token auth with offline assets
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_NODE_JOIN_OFFLINE_LOADED:-}" ]] && return 0
readonly _E2E_NODE_JOIN_OFFLINE_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

readonly offlineArtifactsSource='oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:e2e-k8s-{{ .KubernetesVersion }}'
readonly offlineOCIImage='ghcr.io/azure/agent-ubuntu2404:v20260619'
readonly offlineNPDVersion='v1.35.1'
readonly offlineContainerdVersion='2.1.8'
readonly offlineRuncVersion='1.5.0'
readonly offlineCNIVersion='1.5.1'

_normalize_kubernetes_version_v() {
  local version="$1"
  if [[ "${version}" == v* ]]; then
    printf '%s\n' "${version}"
  else
    printf 'v%s\n' "${version}"
  fi
}

_crictl_version_for_kubernetes() {
  local version
  version="$(_normalize_kubernetes_version_v "$1")"
  version="${version#v}"
  local major minor _patch
  IFS='.' read -r major minor _patch <<< "${version}"
  printf '%s.%s.0\n' "${major}" "${minor}"
}

_build_offline_artifacts_tarball() {
  local kube_version_v="$1"
  local output_root="${E2E_WORK_DIR}/offline-bootstrap-artifacts"
  local output_dir="${output_root}/${kube_version_v}"
  local manifest_file="${E2E_WORK_DIR}/offline-bootstrap-manifest-${kube_version_v}.json"
  local tarball="${E2E_WORK_DIR}/offline-bootstrap-artifacts-${kube_version_v}.tar.gz"
  local tools_dir="${E2E_WORK_DIR}/tools"
  local builder="${tools_dir}/agent-artifacts-builder"
  local unbounded_version
  local crictl_version

  unbounded_version="$(cd "${REPO_ROOT}" && go list -m -f '{{.Version}}' github.com/Azure/unbounded)"
  crictl_version="$(_crictl_version_for_kubernetes "${kube_version_v}")"

  log_info "Building agent-artifacts-builder from github.com/Azure/unbounded@${unbounded_version}..."
  mkdir -p "${tools_dir}"
  GOBIN="${tools_dir}" go install "github.com/Azure/unbounded/hack/cmd/agent-artifacts-builder@${unbounded_version}"

  rm -rf "${output_dir}"
  mkdir -p "${output_root}"
  cat > "${manifest_file}" <<EOF
{
  "versions": {
    "kubernetes": "${kube_version_v}",
    "containerd": "${offlineContainerdVersion}",
    "runc": "${offlineRuncVersion}",
    "cni": "${offlineCNIVersion}",
    "crictl": "${crictl_version}",
    "npd": "${offlineNPDVersion}"
  },
  "containerImages": []
}
EOF

  log_info "Building offline bootstrap artifacts for ${kube_version_v}..."
  "${builder}" \
    --output-dir "${output_dir}" \
    --manifest "${manifest_file}" \
    --arch amd64

  log_info "Adding node-problem-detector ${offlineNPDVersion} to offline artifacts..."
  mkdir -p "${output_dir}/npd/${offlineNPDVersion}"
  curl -fsSL \
    -o "${output_dir}/npd/${offlineNPDVersion}/node-problem-detector-${offlineNPDVersion}-linux_amd64.tar.gz" \
    "https://github.com/kubernetes/node-problem-detector/releases/download/${offlineNPDVersion}/node-problem-detector-${offlineNPDVersion}-linux_amd64.tar.gz"

  tar -czf "${tarball}" -C "${output_root}" "${kube_version_v}"
  printf '%s\n' "${tarball}"
}

_prepare_remote_offline_registry() {
  local vm_ip="$1"
  local kube_version_v="$2"
  local tarball="$3"
  local remote_ref="127.0.0.1:5000/aks-flex/bootstrap-artifacts:e2e-k8s-${kube_version_v}"

  log_info "Uploading offline bootstrap artifacts to ${vm_ip}..."
  remote_copy "${tarball}" "${vm_ip}" "/tmp/offline-bootstrap-artifacts.tar.gz"

  log_info "Publishing offline bootstrap artifacts to loopback OCI registry on ${vm_ip}..."
  remote_exec "${vm_ip}" "KUBE_VERSION_V=${kube_version_v} REMOTE_REF=${remote_ref} ORAS_VERSION=1.3.2 bash -s" <<'REMOTE'
set -euo pipefail

if command -v apt-get >/dev/null 2>&1; then
  sudo DEBIAN_FRONTEND=noninteractive apt-get update
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y curl podman
fi

if ! command -v oras >/dev/null 2>&1; then
  curl -fsSL -o /tmp/oras.tar.gz "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_linux_amd64.tar.gz"
  tar -C /tmp -xzf /tmp/oras.tar.gz oras
  sudo install -m 0755 /tmp/oras /usr/local/bin/oras
fi

sudo podman rm -f aks-flex-e2e-artifacts-registry >/dev/null 2>&1 || true
sudo podman run -d \
  --name aks-flex-e2e-artifacts-registry \
  --restart=always \
  --network host \
  -e REGISTRY_HTTP_ADDR=127.0.0.1:5000 \
  docker.io/library/registry:2 >/dev/null

for _ in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:5000/v2/ >/dev/null; then
    break
  fi
  sleep 2
done
curl -fsS http://127.0.0.1:5000/v2/ >/dev/null

rm -rf /tmp/offline-bootstrap-artifacts
mkdir -p /tmp/offline-bootstrap-artifacts
tar -xzf /tmp/offline-bootstrap-artifacts.tar.gz -C /tmp/offline-bootstrap-artifacts
cd "/tmp/offline-bootstrap-artifacts/${KUBE_VERSION_V}"

mapfile -t files < <(find . -type f -printf '%P\n' | sort)
oras push \
  --plain-http \
  --artifact-type application/vnd.unbounded.agent.bootstrap-artifacts.v1 \
  "${REMOTE_REF}" \
  "${files[@]}"

oras manifest fetch --plain-http "${REMOTE_REF}" >/dev/null
REMOTE
}

node_join_offline() {
  log_section "Joining Offline Artifacts Node"
  local start
  start=$(timer_start)

  local vm_ip
  vm_ip="$(state_get offline_vm_ip)"
  local vm_private_ip
  vm_private_ip="$(state_get offline_vm_private_ip)"
  local cluster_name
  cluster_name="$(state_get cluster_name)"
  local resource_group
  resource_group="$(state_get resource_group)"
  local subscription_id
  subscription_id="$(state_get subscription_id)"

  if [[ -z "${vm_private_ip}" ]] || ! is_valid_ipv4 "${vm_private_ip}"; then
    log_error "Invalid offline VM private IP in state: '${vm_private_ip}'"
    return 1
  fi

  local kube_version_v offline_artifacts_tarball
  kube_version_v="$(_normalize_kubernetes_version_v "${E2E_KUBERNETES_VERSION}")"
  offline_artifacts_tarball="$(_build_offline_artifacts_tarball "${kube_version_v}" | tail -n 1)"
  _prepare_remote_offline_registry "${vm_ip}" "${kube_version_v}" "${offline_artifacts_tarball}"

  log_info "Setting up bootstrap token RBAC resources..."
  with_cluster_lock "${REPO_ROOT}/scripts/aks-flex-config" setup-node-rbac \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}"

  ensure_daemon_csr_approver

  log_info "Generating offline artifacts config..."
  local config_file="${E2E_WORK_DIR}/config-offline.json"
  with_cluster_lock "${REPO_ROOT}/scripts/aks-flex-config" generate-node-config \
    --resource-group "${resource_group}" \
    --cluster-name "${cluster_name}" \
    --subscription "${subscription_id}" \
    --agent-pool-name "${E2E_TARGET_AGENT_POOL_NAME}" \
    --bootstrap-token \
    --output "${config_file}"

  jq \
    --arg nodeIP "${vm_private_ip}" \
    --arg kubernetesVersion "${E2E_KUBERNETES_VERSION}" \
    --arg offlineArtifactsSource "${offlineArtifactsSource}" \
    --arg ociImage "${offlineOCIImage}" \
    --arg npdVersion "${offlineNPDVersion}" \
    '.agent.logLevel = "debug"
      | .agent.e2eMode = true
      | .node.kubelet.nodeIP = $nodeIP
      | .components = (.components // {})
      | .components.kubernetes = $kubernetesVersion
      | del(.components.containerd, .components.runc, .networking.cniVersion)
      | .bootstrap = (.bootstrap // {})
      | .bootstrap.ociImage = $ociImage
      | .bootstrap.offlineArtifacts.source = $offlineArtifactsSource
      | .npd = (.npd // {})
      | .npd.version = $npdVersion
      | del(.kubernetes, .containerd, .runc)' \
    "${config_file}" > "${config_file}.tmp"
  mv "${config_file}.tmp" "${config_file}"

  jq -e \
    --arg offlineArtifactsSource "${offlineArtifactsSource}" \
    --arg ociImage "${offlineOCIImage}" \
    '.bootstrap.ociImage == $ociImage and .bootstrap.offlineArtifacts.source == $offlineArtifactsSource' \
    "${config_file}" >/dev/null
  log_info "Offline node artifact source: ${offlineArtifactsSource}"
  log_info "Offline node OCI image: ${offlineOCIImage}"
  log_info "Offline node NPD artifact: npd/${offlineNPDVersion}/node-problem-detector-${offlineNPDVersion}-linux_amd64.tar.gz"

  _deploy_and_start_agent "${vm_ip}" "${config_file}" "aks-flex-node-offline"

  log_success "Offline artifacts node joined in $(timer_elapsed "${start}")s"
}

node_unjoin_offline() {
  log_section "Unjoining Offline Artifacts Node"
  local start
  start=$(timer_start)

  local vm_ip vm_name
  vm_ip="$(state_get offline_vm_ip)"
  vm_name="$(state_get offline_vm_name)"

  _rp_delete_unjoin_node "${vm_ip}" "${vm_name}"

  log_success "Offline artifacts node unjoined in $(timer_elapsed "${start}")s"
}
