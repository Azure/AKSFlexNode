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

log_section() {
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

  # Skip cleanup for debugging
  E2E_SKIP_CLEANUP="${E2E_SKIP_CLEANUP:-0}"

  # SSH settings
  E2E_SSH_USER="${E2E_SSH_USER:-azureuser}"
  E2E_SSH_OPTS="${E2E_SSH_OPTS:--o StrictHostKeyChecking=no -o ConnectTimeout=10 -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR}"

  # Component versions (match the workflow defaults)
  E2E_KUBERNETES_VERSION="${E2E_KUBERNETES_VERSION:-1.35.0}"
  E2E_CONTAINERD_VERSION="${E2E_CONTAINERD_VERSION:-2.0.4}"
  E2E_RUNC_VERSION="${E2E_RUNC_VERSION:-1.1.12}"

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
  log_info "  Skip Cleanup:     ${E2E_SKIP_CLEANUP}"
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
# Wait for SSH to become available on a host
wait_for_ssh() {
  local host="$1"
  local timeout="${2:-${E2E_SSH_WAIT_TIMEOUT}}"
  local elapsed=0

  log_info "Waiting for SSH on ${host} (timeout: ${timeout}s)..."
  while [[ "${elapsed}" -lt "${timeout}" ]]; do
    # shellcheck disable=SC2086
    if ssh ${E2E_SSH_OPTS} "${E2E_SSH_USER}@${host}" "echo ready" &>/dev/null; then
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
  # shellcheck disable=SC2086
  ssh ${E2E_SSH_OPTS} "${E2E_SSH_USER}@${host}" "$@"
}

# Copy files to a remote host
remote_copy() {
  local src="$1" host="$2" dest="$3"
  # shellcheck disable=SC2086
  scp ${E2E_SSH_OPTS} "${src}" "${E2E_SSH_USER}@${host}:${dest}"
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
  local ldflags="-X main.Version=${version} -X main.GitCommit=${git_commit} -X main.BuildTime=${build_date}"

  E2E_BINARY="${E2E_WORK_DIR}/aks-flex-node"
  (
    cd "${REPO_ROOT}"
    GOOS=linux GOARCH=amd64 go build -ldflags "${ldflags}" -o "${E2E_BINARY}" .
  )
  chmod +x "${E2E_BINARY}"

  log_success "Binary built in $(timer_elapsed "${start}")s -> ${E2E_BINARY}"
}
