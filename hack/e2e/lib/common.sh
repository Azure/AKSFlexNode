#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/common.sh - Shared utilities for AKS Flex Node E2E tests
#
# Provides:
#   - Logging helpers (info, warn, error, section, success)
#   - Prerequisite checks (az, jq, kubectl, ssh, Go)
#   - Configuration loading from environment / .env file
#   - SSH helpers with retry
#   - State file management (persist outputs across stages)
# =============================================================================
set -euo pipefail

# ---------------------------------------------------------------------------
# Guard against double-sourcing
# ---------------------------------------------------------------------------
[[ -n "${_E2E_COMMON_LOADED:-}" ]] && return 0
readonly _E2E_COMMON_LOADED=1

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "${E2E_DIR}/../.." && pwd)"
E2E_LIB_DIR="${E2E_DIR}/lib"
E2E_INFRA_DIR="${E2E_DIR}/infra"

# Working directory for temporary artifacts (state file, configs, logs)
E2E_WORK_DIR="${E2E_WORK_DIR:-/tmp/aks-flex-node-e2e}"

# State file persists deployment outputs across stages
E2E_STATE_FILE="${E2E_WORK_DIR}/state.json"

# Collected logs directory
E2E_LOG_DIR="${E2E_WORK_DIR}/logs"

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
_log() {
  local level="$1"; shift
  local ts
  ts="$(date +"%H:%M:%S")"
  echo "[${ts}] [${level}] $*"
}

log_info()    { _log "INFO"    "$@"; }
log_warn()    { _log "WARN"    "$@" >&2; }
log_error()   { _log "ERROR"   "$@" >&2; }
log_success() { _log "OK"      "$@"; }
log_debug()   {
  [[ "${E2E_DEBUG:-0}" == "1" ]] && _log "DEBUG" "$@"
  return 0
}

_E2E_GHA_GROUP_OPEN=0

gha_end_group() {
  if [[ "${GITHUB_ACTIONS:-}" == "true" && "${_E2E_GHA_GROUP_OPEN}" == "1" ]]; then
    echo "::endgroup::"
    _E2E_GHA_GROUP_OPEN=0
  fi
}

log_section() {
  gha_end_group
  if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    echo "::group::$*"
    _E2E_GHA_GROUP_OPEN=1
  fi
  echo ""
  echo "=================================================================="
  echo "  $*"
  echo "=================================================================="
  echo ""
}

# ---------------------------------------------------------------------------
# Timer helpers
# ---------------------------------------------------------------------------
timer_start() { date +%s; }
timer_elapsed() {
  local start="$1"
  echo $(( $(date +%s) - start ))
}

# is_valid_ipv4 returns success only for canonical dotted-decimal IPv4 literals.
# It intentionally rejects octets with leading zeros (for example 192.168.001.1)
# so E2E fails fast on ambiguous VM output values.
is_valid_ipv4() {
  local ip="$1"
  local IFS='.'
  local -a octets

  [[ "${ip}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
  read -r -a octets <<< "${ip}"

  for octet in "${octets[@]}"; do
    (( octet >= 0 && octet <= 255 )) || return 1
    if [[ "${octet}" != "0" && "${octet#0}" != "${octet}" ]]; then
      return 1
    fi
  done

  return 0
}

# ---------------------------------------------------------------------------
# Prerequisite checks
# ---------------------------------------------------------------------------
require_cmd() {
  local cmd="$1"
  if ! command -v "${cmd}" &>/dev/null; then
    log_error "Required command not found: ${cmd}"
    return 1
  fi
}

check_prerequisites() {
  log_info "Checking prerequisites..."
  local missing=0

  for cmd in az jq kubectl ssh scp openssl; do
    if ! command -v "${cmd}" &>/dev/null; then
      log_error "Missing required tool: ${cmd}"
      missing=1
    fi
  done

  if [[ "${missing}" -eq 1 ]]; then
    log_error "Install missing tools before running E2E tests."
    return 1
  fi

  # Verify Azure CLI authentication
  if ! az account show &>/dev/null; then
    log_error "Azure CLI not authenticated. Run 'az login' first."
    return 1
  fi

  log_success "All prerequisites satisfied"
}

# ---------------------------------------------------------------------------
# Configuration
#
# Variables can be set via:
#   1. Environment variables (highest precedence)
#   2. .env file in repo root
#   3. Defaults below (lowest precedence)
# ---------------------------------------------------------------------------
load_config() {
  # Source .env if present (won't overwrite existing env vars)
  local env_file="${REPO_ROOT}/.env"
  if [[ -f "${env_file}" ]]; then
    log_info "Loading configuration from ${env_file}"
    set -a
    # shellcheck disable=SC1090
    source "${env_file}"
    set +a
  fi

  # Required parameters (must come from env or .env)
  E2E_RESOURCE_GROUP="${E2E_RESOURCE_GROUP:?Set E2E_RESOURCE_GROUP in environment or .env}"
  E2E_LOCATION="${E2E_LOCATION:?Set E2E_LOCATION in environment or .env}"
  AZURE_SUBSCRIPTION_ID="${AZURE_SUBSCRIPTION_ID:-$(az account show --query id -o tsv)}"
  AZURE_TENANT_ID="${AZURE_TENANT_ID:-$(az account show --query tenantId -o tsv)}"

  # Optional: unique suffix (defaults to epoch seconds for uniqueness)
  E2E_NAME_SUFFIX="${E2E_NAME_SUFFIX:-$(date +%s)}"

  # Binary path - build if not provided
  E2E_BINARY="${E2E_BINARY:-}"
  E2E_HELPER_BINARY="${E2E_HELPER_BINARY:-}"

  # Keep E2E runs isolated from stale or corrupt runner-global kubeconfig state.
  E2E_KUBECONFIG="${E2E_KUBECONFIG:-${E2E_WORK_DIR}/kubeconfig}"
  export KUBECONFIG="${E2E_KUBECONFIG}"

  # Skip cleanup for debugging
  E2E_SKIP_CLEANUP="${E2E_SKIP_CLEANUP:-0}"

  # SSH settings
  E2E_SSH_USER="${E2E_SSH_USER:-azureuser}"
  E2E_SSH_OPTS="${E2E_SSH_OPTS:--o StrictHostKeyChecking=no -o ConnectTimeout=10 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR}"

  # Component versions (match the workflow defaults)
  E2E_KUBERNETES_VERSION="${E2E_KUBERNETES_VERSION:-1.35.0}"
  E2E_CONTAINERD_VERSION="${E2E_CONTAINERD_VERSION:-2.0.4}"
  E2E_RUNC_VERSION="${E2E_RUNC_VERSION:-1.1.12}"
  E2E_TARGET_AGENT_POOL_NAME="${E2E_TARGET_AGENT_POOL_NAME:-aksflexnodes}"

  # Timeouts (seconds)
  E2E_SSH_WAIT_TIMEOUT="${E2E_SSH_WAIT_TIMEOUT:-300}"
  E2E_NODE_JOIN_TIMEOUT="${E2E_NODE_JOIN_TIMEOUT:-300}"
  E2E_POD_READY_TIMEOUT="${E2E_POD_READY_TIMEOUT:-120}"
  E2E_BOOTSTRAP_SETTLE_TIME="${E2E_BOOTSTRAP_SETTLE_TIME:-60}"

  log_info "Configuration loaded:"
  log_info "  Resource Group:   ${E2E_RESOURCE_GROUP}"
  log_info "  Location:         ${E2E_LOCATION}"
  log_info "  Subscription:     ${AZURE_SUBSCRIPTION_ID}"
  log_info "  Name Suffix:      ${E2E_NAME_SUFFIX}"
  log_info "  Agent Pool:       ${E2E_TARGET_AGENT_POOL_NAME}"
  log_info "  Kubeconfig:       ${KUBECONFIG}"
  log_info "  Skip Cleanup:     ${E2E_SKIP_CLEANUP}"
}

# Configure ssh/scp to use the private key matching the VM public key.
configure_ssh_identity() {
  if [[ " ${E2E_SSH_OPTS} " == *" -i "* || " ${E2E_SSH_OPTS} " == *" IdentityFile="* ]]; then
    return 0
  fi

  local key_file="${E2E_SSH_PRIVATE_KEY_FILE:-}"

  if [[ -z "${key_file}" && -n "${E2E_SSH_KEY_FILE:-}" && "${E2E_SSH_KEY_FILE}" == *.pub ]]; then
    key_file="${E2E_SSH_KEY_FILE%.pub}"
  fi

  if [[ -z "${key_file}" && -f "${E2E_WORK_DIR}/e2e_ssh_key" ]]; then
    key_file="${E2E_WORK_DIR}/e2e_ssh_key"
  fi

  if [[ -z "${key_file}" ]]; then
    local candidate
    for candidate in "${HOME}/.ssh/id_ed25519" "${HOME}/.ssh/id_rsa" "${HOME}/.ssh/id_ecdsa"; do
      if [[ -f "${candidate}" && -f "${candidate}.pub" ]]; then
        key_file="${candidate}"
        break
      fi
    done
  fi

  if [[ -n "${key_file}" && -f "${key_file}" ]]; then
    export E2E_SSH_IDENTITY_FILE="${key_file}"
    log_debug "Using SSH identity file for e2e VM access"
  fi
}

# ---------------------------------------------------------------------------
# Work directory & state management
# ---------------------------------------------------------------------------
init_work_dir() {
  mkdir -p "${E2E_WORK_DIR}" "${E2E_LOG_DIR}"
  log_debug "Work directory: ${E2E_WORK_DIR}"
}

# Write a key=value into the state file (JSON)
state_set() {
  local key="$1" value="$2"
  local tmp="${E2E_STATE_FILE}.tmp"

  if [[ -f "${E2E_STATE_FILE}" ]]; then
    jq --arg k "${key}" --arg v "${value}" '. + {($k): $v}' "${E2E_STATE_FILE}" > "${tmp}"
  else
    jq -n --arg k "${key}" --arg v "${value}" '{($k): $v}' > "${tmp}"
  fi
  mv "${tmp}" "${E2E_STATE_FILE}"
}

# Read a value from the state file
state_get() {
  local key="$1"
  local default="${2:-}"
  if [[ -f "${E2E_STATE_FILE}" ]]; then
    local val
    val="$(jq -r --arg k "${key}" '.[$k] // empty' "${E2E_STATE_FILE}")"
    echo "${val:-${default}}"
  else
    echo "${default}"
  fi
}

# Dump the state file for debugging
state_dump() {
  if [[ -f "${E2E_STATE_FILE}" ]]; then
    log_info "Current state:"
    jq '.' "${E2E_STATE_FILE}"
  else
    log_info "No state file found"
  fi
}

# ---------------------------------------------------------------------------
# SSH helpers
# ---------------------------------------------------------------------------
_build_ssh_opts() {
  local -n opts_ref="$1"
  # shellcheck disable=SC2206
  opts_ref=(${E2E_SSH_OPTS})

  if [[ -n "${E2E_SSH_IDENTITY_FILE:-}" ]]; then
    opts_ref=(-i "${E2E_SSH_IDENTITY_FILE}" -o IdentitiesOnly=yes "${opts_ref[@]}")
  fi
}

# Wait for SSH to become available on a host
wait_for_ssh() {
  local host="$1"
  local timeout="${2:-${E2E_SSH_WAIT_TIMEOUT}}"
  local elapsed=0

  log_info "Waiting for SSH on ${host} (timeout: ${timeout}s)..."
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    local -a ssh_opts
    _build_ssh_opts ssh_opts
    if ssh "${ssh_opts[@]}" "${E2E_SSH_USER}@${host}" "echo ready" &>/dev/null; then
      log_success "SSH ready on ${host} (${elapsed}s)"
      return 0
    fi
    sleep 10
    elapsed=$((elapsed + 10))
  done

  log_error "SSH not available on ${host} after ${timeout}s"
  return 1
}

# Execute a command on a remote host via SSH
remote_exec() {
  local host="$1"; shift
  local -a ssh_opts
  _build_ssh_opts ssh_opts
  ssh "${ssh_opts[@]}" "${E2E_SSH_USER}@${host}" "$@"
}

# Copy files to a remote host
remote_copy() {
  local src="$1" host="$2" dest="$3"
  local -a ssh_opts
  _build_ssh_opts ssh_opts
  scp "${ssh_opts[@]}" "${src}" "${E2E_SSH_USER}@${host}:${dest}"
}

# ---------------------------------------------------------------------------
# Binary build helper
# ---------------------------------------------------------------------------
ensure_binary() {
  if [[ -n "${E2E_BINARY}" && -f "${E2E_BINARY}" ]]; then
    log_info "Using provided binary: ${E2E_BINARY}"
    return 0
  fi

  log_info "Building aks-flex-node binary for linux/amd64..."
  local start
  start=$(timer_start)

  local version="${VERSION:-dev}"
  local git_commit
  git_commit="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo "unknown")"
  local build_date
  build_date="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  local ldflags="-X github.com/Azure/AKSFlexNode/pkg/cmd/version.Version=${version} -X github.com/Azure/AKSFlexNode/pkg/cmd/version.GitCommit=${git_commit} -X github.com/Azure/AKSFlexNode/pkg/cmd/version.BuildTime=${build_date}"

  E2E_BINARY="${E2E_WORK_DIR}/aks-flex-node"
  E2E_HELPER_BINARY="${E2E_WORK_DIR}/e2ehelper"
  (
    cd "${REPO_ROOT}"
    GOOS=linux GOARCH=amd64 go build -tags local_e2e -ldflags "${ldflags}" -o "${E2E_BINARY}" ./cmd/aks-flex-node
    GOOS=linux GOARCH=amd64 go build -ldflags "${ldflags}" -o "${E2E_HELPER_BINARY}" ./cmd/e2ehelper
  )
  chmod +x "${E2E_BINARY}"
  chmod +x "${E2E_HELPER_BINARY}"

  log_success "Binary built in $(timer_elapsed "${start}")s -> ${E2E_BINARY}"
}

ensure_daemon_csr_approver() {
  local pid
  pid="$(state_get daemon_csr_approver_pid)"
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    return 0
  fi

  local helper_binary="${E2E_HELPER_BINARY:-}"
  if [[ -z "${helper_binary}" || ! -f "${helper_binary}" ]]; then
    helper_binary="${E2E_WORK_DIR}/e2ehelper"
  fi

  log_info "Starting e2e daemon CSR approver..."
  local approver_kubeconfig="${E2E_WORK_DIR}/daemon-csr-approver.kubeconfig"
  kubectl config view --raw --minify > "${approver_kubeconfig}"
  chmod 600 "${approver_kubeconfig}"

  pkill -f 'e2ehelper daemon-csr-approver' 2>/dev/null || true
  "${helper_binary}" daemon-csr-approver \
    --kubeconfig "${approver_kubeconfig}" \
    --daemon-group aks-flex-node-daemons \
    --bootstrap-group system:bootstrappers:aks-flex-node \
    > "${E2E_LOG_DIR}/daemon-csr-approver.log" 2>&1 &
  state_set daemon_csr_approver_pid "$!"
}
