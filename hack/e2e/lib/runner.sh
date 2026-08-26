#!/usr/bin/env bash
# =============================================================================
# hack/e2e/lib/runner.sh - Self-hosted runner maintenance helpers
#
# Provides local runner hygiene for the GitHub Actions host.
# =============================================================================
set -euo pipefail

[[ -n "${_E2E_RUNNER_LOADED:-}" ]] && return 0
readonly _E2E_RUNNER_LOADED=1

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# ---------------------------------------------------------------------------
# cleanup_runner_workspace - Reclaim local disk on the self-hosted runner
# ---------------------------------------------------------------------------
cleanup_runner_workspace() {
  log_section "Cleaning Up Runner Workspace"

  if [[ "${E2E_SKIP_RUNNER_CLEANUP:-0}" == "1" ]]; then
    log_warn "Runner workspace cleanup skipped (E2E_SKIP_RUNNER_CLEANUP=1)"
    return 0
  fi

  local can_sudo=0
  if command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
    can_sudo=1
  fi

  log_info "Disk usage before runner cleanup:"
  df -h / 2>/dev/null || true

  local removed=0
  local work_dir="${E2E_WORK_DIR:-}"
  if [[ ! "${work_dir}" =~ ^/tmp/aks-flex-node-e2e(-[A-Za-z0-9][A-Za-z0-9._-]*)?$ ]]; then
    log_error "Refusing to clean unexpected E2E work directory '${work_dir}'"
    return 1
  fi
  if [[ -d "${work_dir}" ]]; then
    log_info "Removing ${work_dir}"
    if [[ "${can_sudo}" == "1" ]]; then
      sudo rm -rf -- "${work_dir}" || { log_warn "Failed to remove ${work_dir}"; return 1; }
    else
      rm -rf -- "${work_dir}" || { log_warn "Failed to remove ${work_dir}"; return 1; }
    fi
    removed=1
  fi

  log_info "Removed ${removed} temporary directory"

  if command -v go >/dev/null 2>&1; then
    log_info "Cleaning Go build cache"
    go clean -cache 2>/dev/null || true
  fi

  if [[ "${can_sudo}" == "1" ]]; then
    log_info "Cleaning apt cache"
    sudo apt-get clean 2>/dev/null || true
    sudo rm -rf /var/cache/apt/archives/* 2>/dev/null || true

    log_info "Trimming systemd journal"
    sudo journalctl --vacuum-size="${E2E_JOURNAL_MAX_SIZE:-200M}" >/dev/null 2>&1 || true
  else
    log_warn "Passwordless sudo is unavailable; skipping system cache cleanup"
  fi

  local runner_work_root=""
  if [[ -n "${RUNNER_WORKSPACE:-}" ]]; then
    runner_work_root="$(dirname "${RUNNER_WORKSPACE}")"
  fi
  if [[ "${runner_work_root}" == */_work && -d "${runner_work_root}/_update" ]]; then
    log_info "Cleaning old runner update payloads"
    if [[ "${can_sudo}" == "1" ]]; then
      sudo find "${runner_work_root}/_update" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + 2>/dev/null || true
    else
      find "${runner_work_root}/_update" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + 2>/dev/null || true
    fi
  fi

  log_info "Disk usage after runner cleanup:"
  df -h / 2>/dev/null || true
  log_success "Runner workspace cleanup complete"
}
