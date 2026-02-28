#!/usr/bin/env bash
# =============================================================================
# hack/e2e/run.sh - AKS Flex Node E2E Test Orchestrator
#
# Usage:
#   ./hack/e2e/run.sh [command] [options]
#
# Commands:
#   all        Run the full E2E flow (default): build, infra, join, validate, cleanup
#   infra      Deploy infrastructure only (Bicep: AKS + 2 VMs)
#   join       Join both nodes to the cluster (requires prior infra)
#   join-msi   Join only the MSI node
#   join-token Join only the token node
#   validate   Verify nodes joined + run smoke tests
#   smoke      Run smoke tests only (pods on flex nodes)
#   logs       Collect logs from VMs
#   cleanup    Tear down Azure resources
#   status     Show current state (deployment outputs)
#
# Options:
#   -g, --resource-group  Azure resource group      (or E2E_RESOURCE_GROUP env)
#   -l, --location        Azure region              (or E2E_LOCATION env)
#   -b, --binary          Path to pre-built binary  (or E2E_BINARY env)
#   -s, --suffix          Name suffix for resources (or E2E_NAME_SUFFIX env)
#       --skip-cleanup    Keep resources after test  (or E2E_SKIP_CLEANUP=1)
#       --skip-build      Don't build the binary (use existing or --binary)
#       --debug           Enable debug logging       (or E2E_DEBUG=1)
#   -h, --help            Show this help message
#
# Environment Variables:
#   E2E_RESOURCE_GROUP      Azure resource group for test resources
#   E2E_LOCATION            Azure region (e.g. westus2)
#   E2E_BINARY              Path to pre-built aks-flex-node binary
#   E2E_NAME_SUFFIX         Unique suffix for resource names
#   E2E_SKIP_CLEANUP        Set to 1 to keep resources after tests
#   E2E_SKIP_BUILD          Set to 1 to skip building the binary
#   E2E_DEBUG               Set to 1 for verbose logging
#   E2E_SSH_KEY_FILE        SSH public key file for VM access
#   E2E_WORK_DIR            Working directory for state/artifacts
#   E2E_KUBERNETES_VERSION  Kubernetes version (default: 1.35.0)
#   E2E_CONTAINERD_VERSION  Containerd version (default: 2.0.4)
#   E2E_RUNC_VERSION        Runc version (default: 1.1.12)
#   AZURE_SUBSCRIPTION_ID   Azure subscription (auto-detected if not set)
#   AZURE_TENANT_ID         Azure tenant (auto-detected if not set)
#
# Examples:
#   # Full E2E test (requires E2E_RESOURCE_GROUP and E2E_LOCATION)
#   E2E_RESOURCE_GROUP=rg-e2e E2E_LOCATION=westus2 ./hack/e2e/run.sh
#
#   # With a .env file
#   cp .env.example .env && vim .env
#   ./hack/e2e/run.sh
#
#   # Deploy infra only, then iterate on join/validate
#   ./hack/e2e/run.sh infra
#   ./hack/e2e/run.sh join
#   ./hack/e2e/run.sh validate
#
#   # Use a pre-built binary
#   ./hack/e2e/run.sh --binary ./aks-flex-node all
#
#   # Keep resources for debugging
#   ./hack/e2e/run.sh --skip-cleanup all
#
#   # Clean up from a previous run
#   ./hack/e2e/run.sh cleanup
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Source library modules
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib/common.sh"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib/infra.sh"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib/node-join.sh"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib/validate.sh"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib/cleanup.sh"

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------
COMMAND="all"
SKIP_BUILD="${E2E_SKIP_BUILD:-0}"

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------
usage() {
  sed -n '/^# Usage:/,/^# ====/{ /^# ====/d; s/^# \{0,1\}//; p }' "${BASH_SOURCE[0]}"
  exit 0
}

# ---------------------------------------------------------------------------
# Parse CLI arguments
# ---------------------------------------------------------------------------
parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      all|infra|join|join-msi|join-token|validate|smoke|logs|cleanup|status)
        COMMAND="$1"; shift ;;
      -g|--resource-group) export E2E_RESOURCE_GROUP="$2"; shift 2 ;;
      -l|--location)       export E2E_LOCATION="$2"; shift 2 ;;
      -b|--binary)         export E2E_BINARY="$2"; shift 2 ;;
      -s|--suffix)         export E2E_NAME_SUFFIX="$2"; shift 2 ;;
      --skip-cleanup)      export E2E_SKIP_CLEANUP=1; shift ;;
      --skip-build)        SKIP_BUILD=1; shift ;;
      --debug)             export E2E_DEBUG=1; shift ;;
      -h|--help)           usage ;;
      *)
        log_error "Unknown argument: $1"
        echo "Run '$0 --help' for usage."
        exit 1
        ;;
    esac
  done
}

# ---------------------------------------------------------------------------
# Command: all (full E2E flow)
# ---------------------------------------------------------------------------
cmd_all() {
  local overall_start
  overall_start=$(timer_start)
  local exit_code=0

  log_section "AKS Flex Node E2E Test - Full Run"

  # Build
  if [[ "${SKIP_BUILD}" != "1" ]]; then
    ensure_binary
  else
    if [[ -z "${E2E_BINARY:-}" || ! -f "${E2E_BINARY:-}" ]]; then
      log_error "--skip-build requires --binary <path> or E2E_BINARY set to an existing file"
      return 1
    fi
    log_info "Skipping build, using: ${E2E_BINARY}"
  fi

  # Infrastructure
  infra_deploy

  # Join nodes
  node_join_all

  # Validate + smoke tests
  validate_all_nodes
  smoke_test_all || exit_code=1

  # Collect logs (always, even if tests fail)
  collect_logs || true

  # Cleanup
  cleanup || true

  echo ""
  log_section "E2E Test Complete"
  local duration
  duration=$(timer_elapsed "${overall_start}")

  if [[ "${exit_code}" -eq 0 ]]; then
    log_success "All tests PASSED (${duration}s)"
  else
    log_error "Some tests FAILED (${duration}s)"
    log_info "Logs available in: ${E2E_LOG_DIR}/"
  fi

  return ${exit_code}
}

# ---------------------------------------------------------------------------
# Command: status
# ---------------------------------------------------------------------------
cmd_status() {
  log_section "E2E State"
  state_dump
}

# ---------------------------------------------------------------------------
# Main dispatch
# ---------------------------------------------------------------------------
main() {
  parse_args "$@"

  # Common initialization
  check_prerequisites
  load_config
  init_work_dir

  case "${COMMAND}" in
    all)
      cmd_all
      ;;
    infra)
      if [[ "${SKIP_BUILD}" != "1" ]]; then
        ensure_binary
      fi
      infra_deploy
      ;;
    join)
      ensure_binary
      node_join_all
      ;;
    join-msi)
      ensure_binary
      node_join_msi
      ;;
    join-token)
      ensure_binary
      node_join_token
      ;;
    validate)
      validate_all_nodes
      smoke_test_all
      ;;
    smoke)
      smoke_test_all
      ;;
    logs)
      collect_logs
      ;;
    cleanup)
      collect_logs || true
      cleanup
      ;;
    status)
      cmd_status
      ;;
    *)
      log_error "Unknown command: ${COMMAND}"
      usage
      ;;
  esac
}

main "$@"
