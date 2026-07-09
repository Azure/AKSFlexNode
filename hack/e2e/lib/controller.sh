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
readonly E2E_CONTROLLER_REGISTRY="aks-flex-controller-registry"
readonly E2E_MACHINE_CONFIGMAP="aks-flex-machines"
readonly E2E_CONTROLLER_SERVICE_PROXY_PATH="/api/v1/namespaces/${E2E_CONTROLLER_NAMESPACE}/services/http:${E2E_CONTROLLER_DEPLOYMENT}:80/proxy"
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

_controller_registry_hostport() {
  echo "${E2E_CONTROLLER_REGISTRY_HOSTPORT:-5000}"
}

_controller_registry_local_port() {
  echo "${E2E_CONTROLLER_REGISTRY_LOCAL_PORT:-5001}"
}

_wait_for_tcp_port() {
  local port="$1"
  local elapsed=0

  while [[ "${elapsed}" -lt 60 ]]; do
    if python3 - "${port}" <<'PY' >/dev/null 2>&1
import socket
import sys

port = int(sys.argv[1])
with socket.create_connection(("127.0.0.1", port), timeout=1):
    pass
PY
    then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done

  return 1
}

_ensure_local_registry_unlocked() {
  local host_port registry_image
  host_port="$(_controller_registry_hostport)"
  registry_image="${E2E_CONTROLLER_REGISTRY_IMAGE:-registry:2}"

  log_section "Deploying Local Controller Image Registry"
  log_info "Ensuring registry ${E2E_CONTROLLER_REGISTRY} on node localhost:${host_port}"
  kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${E2E_CONTROLLER_REGISTRY}
  namespace: ${E2E_CONTROLLER_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${E2E_CONTROLLER_REGISTRY}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: ${E2E_CONTROLLER_REGISTRY}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ${E2E_CONTROLLER_REGISTRY}
    spec:
      containers:
        - name: registry
          image: ${registry_image}
          imagePullPolicy: IfNotPresent
          env:
            - name: REGISTRY_STORAGE_FILESYSTEM_ROOTDIRECTORY
              value: /var/lib/registry
          ports:
            - name: registry
              containerPort: 5000
              hostPort: ${host_port}
          readinessProbe:
            httpGet:
              path: /v2/
              port: registry
            initialDelaySeconds: 2
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /v2/
              port: registry
            initialDelaySeconds: 5
            periodSeconds: 20
          volumeMounts:
            - name: registry-data
              mountPath: /var/lib/registry
      volumes:
        - name: registry-data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ${E2E_CONTROLLER_REGISTRY}
  namespace: ${E2E_CONTROLLER_NAMESPACE}
  labels:
    app.kubernetes.io/name: ${E2E_CONTROLLER_REGISTRY}
spec:
  selector:
    app.kubernetes.io/name: ${E2E_CONTROLLER_REGISTRY}
  ports:
    - name: registry
      port: 5000
      targetPort: registry
EOF

  kubectl -n "${E2E_CONTROLLER_NAMESPACE}" rollout status "deployment/${E2E_CONTROLLER_REGISTRY}" --timeout=300s
  local registry_node
  registry_node="$(kubectl -n "${E2E_CONTROLLER_NAMESPACE}" get pod \
    -l "app.kubernetes.io/name=${E2E_CONTROLLER_REGISTRY}" \
    -o jsonpath='{.items[0].spec.nodeName}')"
  if [[ -z "${registry_node}" ]]; then
    log_error "Failed to resolve local registry node"
    return 1
  fi
  state_set controller_registry_node "${registry_node}"
  log_success "Local registry is ready on node ${registry_node}"
}

_start_registry_port_forward() {
  local -n out_pid="$1"
  local local_port="$2"
  local log_file="${E2E_LOG_DIR}/controller-registry-port-forward.log"

  kubectl -n "${E2E_CONTROLLER_NAMESPACE}" port-forward \
    "service/${E2E_CONTROLLER_REGISTRY}" \
    "${local_port}:5000" \
    > "${log_file}" 2>&1 &
  out_pid=$!

  if ! _wait_for_tcp_port "${local_port}"; then
    log_error "Timed out waiting for registry port-forward on localhost:${local_port}; see ${log_file}"
    kill "${out_pid}" 2>/dev/null || true
    wait "${out_pid}" 2>/dev/null || true
    return 1
  fi
}

_stop_registry_port_forward() {
  local pid="${1:-}"
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
}

_build_controller_image() {
  local -n out_image="$1"

  _ensure_local_registry_unlocked

  local version git_commit build_time tag local_port host_port local_image cluster_image pf_pid
  version="${VERSION:-dev}"
  git_commit="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  build_time="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  tag="$(_sanitize_image_tag "e2e-${GITHUB_RUN_ID:-local}-${E2E_NAME_SUFFIX}-${git_commit}")"
  local_port="$(_controller_registry_local_port)"
  host_port="$(_controller_registry_hostport)"
  local_image="localhost:${local_port}/aks-flex-controller:${tag}"
  cluster_image="localhost:${host_port}/aks-flex-controller:${tag}"

  log_section "Building AKS Flex Controller Image"
  log_info "Building controller image ${local_image} and pushing to in-cluster local registry"
  pf_pid=""
  _start_registry_port_forward pf_pid "${local_port}"

  if ! (
    cd "${REPO_ROOT}"
    DOCKER_BUILDKIT=1 docker build \
      --platform linux/amd64 \
      --file Dockerfile.aks-flex-controller \
      --build-arg "VERSION=${version}" \
      --build-arg "GIT_COMMIT=${git_commit}" \
      --build-arg "BUILD_TIME=${build_time}" \
      --tag "${local_image}" \
      .
    docker push "${local_image}"
  ); then
    _stop_registry_port_forward "${pf_pid}"
    return 1
  fi

  _stop_registry_port_forward "${pf_pid}"
  docker image rm "${local_image}" >/dev/null 2>&1 || true
  out_image="${cluster_image}"
}

_controller_image_from_state_or_env() {
  if [[ -n "${E2E_CONTROLLER_IMAGE:-}" ]]; then
    echo "${E2E_CONTROLLER_IMAGE}"
    return 0
  fi
  state_get controller_image
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

  local image_repo image_tag overlay_dir registry_node node_patch host_port
  image_repo="${image%:*}"
  image_tag="${image##*:}"
  overlay_dir="${E2E_WORK_DIR}/controller-deployment"
  rm -rf "${overlay_dir}"
  mkdir -p "${overlay_dir}/base"
  cp -R "${REPO_ROOT}/hack/controller-deployment/." "${overlay_dir}/base/"

  node_patch=""
  host_port="$(_controller_registry_hostport)"
  if [[ "${image}" == "localhost:${host_port}/"* ]]; then
    registry_node="$(state_get controller_registry_node)"
    if [[ -z "${registry_node}" ]]; then
      log_error "Local controller image requires controller_registry_node in E2E state"
      return 1
    fi
    node_patch="            nodeName: ${registry_node}"
  fi

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
${node_patch}
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
  kubectl get --raw "${E2E_CONTROLLER_SERVICE_PROXY_PATH}/healthz" >/dev/null
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

  if [[ -n "${E2E_CONTROLLER_IMAGE:-}" ]]; then
    log_info "Using provided controller image: ${image}"
  else
    # The local registry is cluster-local and empty after each infra deployment,
    # so rebuild/push whenever the current controller is not already healthy.
    _build_controller_image image
  fi
  state_set controller_image "${image}"

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
