#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/controller.sh - In-cluster controller deployment and machine data
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_CONTROLLER_LOADED:-}" ]] && return 0
readonly _E2E_CONTROLLER_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

readonly E2E_CONTROLLER_NAMESPACE="kube-system"
readonly E2E_CONTROLLER_DEPLOYMENT="aks-flex-controller"
readonly E2E_MACHINE_CONFIGMAP="aks-flex-machines"
readonly E2E_CONTROLLER_BASE_IMAGE="ghcr.io/azure/aks-flex-controller"

_sanitize_image_tag() {
  local raw="$1"
  local tag
  tag="$(echo "${raw}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9._-]+/-/g; s/^-+//; s/-+$//')"
  tag="${tag:0:127}"
  if [[ -z "${tag}" ]]; then
    tag="e2e"
  fi
  echo "${tag}"
}

_build_controller_image() {
  local -n out_image="$1"

  local acr_name acr_login_server
  acr_name="$(state_get acr_name)"
  acr_login_server="$(state_get acr_login_server)"
  if [[ -z "${acr_name}" || -z "${acr_login_server}" ]]; then
    log_error "ACR outputs are missing from E2E state; run ./hack/e2e/run.sh infra first"
    return 1
  fi

  local version git_commit build_time tag image
  version="${VERSION:-dev}"
  git_commit="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  build_time="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  tag="$(_sanitize_image_tag "e2e-${GITHUB_RUN_ID:-local}-${E2E_NAME_SUFFIX}-${git_commit}")"
  image="${acr_login_server}/aks-flex-controller:${tag}"

  log_section "Building AKS Flex Controller Image"
  log_info "Building and pushing controller image with ACR Tasks: ${image}"
  (
    cd "${REPO_ROOT}"
    az acr build \
      --registry "${acr_name}" \
      --image "aks-flex-controller:${tag}" \
      --file Dockerfile.aks-flex-controller \
      --platform linux/amd64 \
      --build-arg "VERSION=${version}" \
      --build-arg "GIT_COMMIT=${git_commit}" \
      --build-arg "BUILD_TIME=${build_time}" \
      .
  )

  out_image="${image}"
}

_controller_image_from_state_or_env() {
  if [[ -n "${E2E_CONTROLLER_IMAGE:-}" ]]; then
    echo "${E2E_CONTROLLER_IMAGE}"
    return 0
  fi

  local image acr_login_server
  image="$(state_get controller_image)"
  acr_login_server="$(state_get acr_login_server)"
  if [[ -n "${image}" && -n "${acr_login_server}" && "${image}" != "${acr_login_server}/"* ]]; then
    # A new infra deployment creates a new per-run ACR. Ignore stale image state
    # that points at an older registry so the controller is rebuilt and pushed.
    echo ""
    return 0
  fi
  echo "${image}"
}

_controller_deployment_uses_image() {
  local image="$1"
  [[ -n "${image}" ]] || return 1

  local current_image
  current_image="$(kubectl -n "${E2E_CONTROLLER_NAMESPACE}" get deployment "${E2E_CONTROLLER_DEPLOYMENT}" -o jsonpath='{.spec.template.spec.containers[?(@.name=="aks-flex-controller")].image}' 2>/dev/null || true)"
  [[ "${current_image}" == "${image}" ]]
}

_deploy_controller_image() {
  local image="$1"
  if [[ -z "${image}" || "${image}" != *:* ]]; then
    log_error "Controller image must include an explicit tag, got '${image}'"
    return 1
  fi

  local image_repo image_tag overlay_dir
  image_repo="${image%:*}"
  image_tag="${image##*:}"
  overlay_dir="${E2E_WORK_DIR}/controller-deployment"
  rm -rf "${overlay_dir}"
  mkdir -p "${overlay_dir}/base"
  cp -R "${REPO_ROOT}/hack/controller-deployment/." "${overlay_dir}/base/"

  cat > "${overlay_dir}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - base
images:
  - name: ${E2E_CONTROLLER_BASE_IMAGE}
    newName: ${image_repo}
    newTag: ${image_tag}
patches:
  - target:
      group: apps
      version: v1
      kind: Deployment
      name: ${E2E_CONTROLLER_DEPLOYMENT}
      namespace: ${E2E_CONTROLLER_NAMESPACE}
    patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${E2E_CONTROLLER_DEPLOYMENT}
        namespace: ${E2E_CONTROLLER_NAMESPACE}
      spec:
        template:
          spec:
            containers:
              - name: aks-flex-controller
                args:
                  - --listen-address=:8080
                  - --machine-configmap-namespace=${E2E_CONTROLLER_NAMESPACE}
                  - --machine-configmap-name=${E2E_MACHINE_CONFIGMAP}
                  - --enable-csr-approver=true
EOF

  log_section "Deploying AKS Flex Controller"
  log_info "Applying controller manifests with image ${image}"
  kubectl apply -k "${overlay_dir}"
  _wait_for_controller_ready
}

_controller_healthz() {
  kubectl get --raw "/api/v1/namespaces/${E2E_CONTROLLER_NAMESPACE}/services/http:${E2E_CONTROLLER_DEPLOYMENT}:80/proxy/healthz" >/dev/null
}

_wait_for_controller_ready() {
  kubectl -n "${E2E_CONTROLLER_NAMESPACE}" rollout status "deployment/${E2E_CONTROLLER_DEPLOYMENT}" --timeout=300s
  kubectl -n "${E2E_CONTROLLER_NAMESPACE}" wait \
    --for=condition=Ready \
    "pod" \
    -l "app.kubernetes.io/name=${E2E_CONTROLLER_DEPLOYMENT}" \
    --timeout=300s

  log_info "Checking controller service-proxy health endpoint"
  _controller_healthz
  log_success "AKS Flex Controller is ready"
}

_ensure_flex_controller_unlocked() {
  local image
  image="$(_controller_image_from_state_or_env)"

  if [[ -z "${image}" ]]; then
    _build_controller_image image
    state_set controller_image "${image}"
  else
    log_info "Using controller image: ${image}"
    state_set controller_image "${image}"
  fi

  if _controller_deployment_uses_image "${image}"; then
    log_debug "Controller deployment already uses ${image}"
    _wait_for_controller_ready
  else
    _deploy_controller_image "${image}"
  fi
}

ensure_flex_controller() {
  local image
  image="$(_controller_image_from_state_or_env)"
  if [[ -n "${image}" ]] && _controller_deployment_uses_image "${image}" && _controller_healthz 2>/dev/null; then
    return 0
  fi
  with_cluster_lock _ensure_flex_controller_unlocked
}

_render_machine_json() {
  local node_name="$1" kubernetes_version="$2" settings_version="$3"
  local cluster_id machine_id
  cluster_id="$(state_get cluster_id)"
  machine_id="${cluster_id}/agentPools/${E2E_TARGET_AGENT_POOL_NAME}/machines/${node_name}"

  jq -n \
    --arg id "${machine_id}" \
    --arg name "${node_name}" \
    --arg kubernetesVersion "${kubernetes_version}" \
    --arg settingsVersion "${settings_version}" \
    '{
      id: $id,
      name: $name,
      type: "Microsoft.ContainerService/managedClusters/agentPools/machines",
      properties: {
        provisioningState: "Succeeded",
        settings: {
          kubernetesVersion: $kubernetesVersion,
          settingsVersion: $settingsVersion,
          maxPods: 110,
          nodeLabels: {
            "kubernetes.azure.com/managed": "false"
          },
          nodeTaints: [],
          kubeletConfig: {
            imageGCHighThreshold: 85,
            imageGCLowThreshold: 80
          }
        }
      }
    }'
}

_machine_configmap_upsert_unlocked() {
  local node_name="$1" kubernetes_version="$2" settings_version="$3"
  local machine_file patch
  machine_file="${E2E_WORK_DIR}/machine-${node_name}.json"

  _render_machine_json "${node_name}" "${kubernetes_version}" "${settings_version}" > "${machine_file}"
  if ! kubectl -n "${E2E_CONTROLLER_NAMESPACE}" get configmap "${E2E_MACHINE_CONFIGMAP}" >/dev/null 2>&1; then
    kubectl -n "${E2E_CONTROLLER_NAMESPACE}" create configmap "${E2E_MACHINE_CONFIGMAP}" >/dev/null
  fi

  patch="$(jq -n --arg key "${node_name}.json" --rawfile machine "${machine_file}" '{data: {($key): $machine}}')"
  kubectl -n "${E2E_CONTROLLER_NAMESPACE}" patch configmap "${E2E_MACHINE_CONFIGMAP}" --type merge -p "${patch}" >/dev/null
  log_info "Published machine goal for ${node_name}: Kubernetes ${kubernetes_version}, settings ${settings_version}"
}

machine_configmap_upsert() {
  local node_name="$1" kubernetes_version="${2:-${E2E_KUBERNETES_VERSION}}" settings_version="${3:-${kubernetes_version}}"
  with_cluster_lock _machine_configmap_upsert_unlocked "${node_name}" "${kubernetes_version}" "${settings_version}"
}

_machine_configmap_delete_unlocked() {
  local node_name="$1"
  local patch
  patch="$(jq -n --arg jsonKey "${node_name}.json" --arg plainKey "${node_name}" '{data: {($jsonKey): null, ($plainKey): null}}')"
  if kubectl -n "${E2E_CONTROLLER_NAMESPACE}" get configmap "${E2E_MACHINE_CONFIGMAP}" >/dev/null 2>&1; then
    kubectl -n "${E2E_CONTROLLER_NAMESPACE}" patch configmap "${E2E_MACHINE_CONFIGMAP}" --type merge -p "${patch}" >/dev/null || true
  fi
  log_info "Removed machine goal for ${node_name}"
}

machine_configmap_delete() {
  local node_name="$1"
  with_cluster_lock _machine_configmap_delete_unlocked "${node_name}"
}
