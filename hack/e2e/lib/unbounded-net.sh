#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/unbounded-net.sh - Unbounded-Net CNI deployment for E2E
#
# Functions:
#   ensure_unbounded_net - Install Unbounded-Net and the E2E site config
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_UNBOUNDED_NET_LOADED:-}" ]] && return 0
readonly _E2E_UNBOUNDED_NET_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

ensure_unbounded_net_source() {
  local source_dir="$1"
  local version="${E2E_UNBOUNDED_NET_VERSION}"

  if [[ -d "${source_dir}/.git" ]]; then
    return 0
  fi

  rm -rf "${source_dir}"
  log_info "Fetching Azure/unbounded ${version} for Unbounded-Net manifests..."
  git clone --depth 1 --branch "${version}" https://github.com/Azure/unbounded.git "${source_dir}"
}

render_unbounded_net_manifests() {
  local source_dir="$1"
  local version="${E2E_UNBOUNDED_NET_VERSION}"
  local controller_image="${E2E_UNBOUNDED_NET_CONTROLLER_IMAGE:-ghcr.io/azure/unbounded-net-controller:${version}}"
  local node_image="${E2E_UNBOUNDED_NET_NODE_IMAGE:-ghcr.io/azure/unbounded-net-node:${version}}"

  log_info "Rendering Unbounded-Net manifests (controller=${controller_image}, node=${node_image})..."
  (
    cd "${source_dir}"
    GOPROXY="${E2E_GO_PROXY:-https://proxy.golang.org,direct}" \
    make \
      VERSION="${version}" \
      NET_CONTROLLER_IMAGE="${controller_image}" \
      NET_NODE_IMAGE="${node_image}" \
      net-manifests
  )
}

apply_unbounded_net_site() {
  local site_name="${E2E_UNBOUNDED_NET_SITE_NAME}"
  local node_cidr="${E2E_UNBOUNDED_NET_NODE_CIDR}"
  local pod_cidr="${E2E_UNBOUNDED_NET_POD_CIDR}"

  log_info "Applying Unbounded-Net E2E site '${site_name}' (nodes=${node_cidr}, pods=${pod_cidr})..."
  kubectl apply --server-side --force-conflicts -f - <<EOF
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: ${site_name}
spec:
  nodeCidrs:
  - ${node_cidr}
  podCidrAssignments:
  - assignmentEnabled: true
    cidrBlocks:
    - ${pod_cidr}
  manageCniPlugin: true
EOF
}

ensure_unbounded_net() {
  log_section "Installing Unbounded-Net CNI"

  local source_dir rendered_dir
  source_dir="${E2E_WORK_DIR}/unbounded-${E2E_UNBOUNDED_NET_VERSION}"
  rendered_dir="${source_dir}/deploy/net/rendered"

  ensure_unbounded_net_source "${source_dir}"
  render_unbounded_net_manifests "${source_dir}"

  log_info "Applying Unbounded-Net namespace, config, CRDs, controller, and node agent..."
  kubectl apply --server-side --force-conflicts -f "${rendered_dir}/00-namespace.yaml"
  kubectl apply --server-side --force-conflicts -f "${rendered_dir}/01-configmap.yaml"
  kubectl apply --server-side --force-conflicts -f "${rendered_dir}/crd/"
  kubectl apply --server-side --force-conflicts -f "${rendered_dir}/controller/"
  kubectl apply --server-side --force-conflicts -f "${rendered_dir}/node/"

  log_info "Waiting for Unbounded-Net controller..."
  kubectl -n unbounded-net rollout status deploy/unbounded-net-controller --timeout="${E2E_UNBOUNDED_NET_ROLLOUT_TIMEOUT}s"

  apply_unbounded_net_site

  log_info "Waiting for Unbounded-Net node DaemonSet..."
  kubectl -n unbounded-net rollout status ds/unbounded-net-node --timeout="${E2E_UNBOUNDED_NET_ROLLOUT_TIMEOUT}s"

  log_info "Waiting for current nodes to be Ready after CNI installation..."
  kubectl wait --for=condition=Ready nodes --all --timeout="${E2E_UNBOUNDED_NET_ROLLOUT_TIMEOUT}s"

  kubectl get sites,sitenodeslices -o wide || true
  kubectl get nodes -L net.unbounded-cloud.io/site -o wide || true
  log_success "Unbounded-Net CNI is ready"
}
