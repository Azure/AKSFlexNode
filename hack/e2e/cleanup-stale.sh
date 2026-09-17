#!/usr/bin/env bash
# =============================================================================
# hack/e2e/cleanup-stale.sh - Delete stale AKS Flex Node E2E resources
#
# E2E runs name every resource with a shared suffix that is the epoch second of
# the run (for example the cluster "aks-e2e-1787164131"). Interrupted runs, or
# runs started with --skip-cleanup, leave those resources behind. This script
# reclaims them by deleting every E2E resource whose suffix is older than a
# maximum age.
#
# Usage:
#   ./hack/e2e/cleanup-stale.sh [options]
#
# Options:
#   -g, --resource-group  Azure resource group   (or E2E_RESOURCE_GROUP env)
#   -a, --max-age-hours   Age threshold in hours (or E2E_STALE_MAX_AGE_HOURS env,
#                         default 24)
#   -n, --dry-run         Only list stale resources (or E2E_DRY_RUN=1)
#   -h, --help            Show this help message
#
# Examples:
#   # List clusters (and their resources) older than one day
#   E2E_RESOURCE_GROUP=rg-e2e ./hack/e2e/cleanup-stale.sh --dry-run
#
#   # Delete everything older than 6 hours
#   E2E_RESOURCE_GROUP=rg-e2e ./hack/e2e/cleanup-stale.sh --max-age-hours 6
# =============================================================================
set -euo pipefail

# shellcheck disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

E2E_STALE_MAX_AGE_HOURS="${E2E_STALE_MAX_AGE_HOURS:-24}"
E2E_DRY_RUN="${E2E_DRY_RUN:-0}"

usage() {
  sed -n '3,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -g|--resource-group) E2E_RESOURCE_GROUP="$2"; shift 2 ;;
      -a|--max-age-hours)  E2E_STALE_MAX_AGE_HOURS="$2"; shift 2 ;;
      -n|--dry-run)        E2E_DRY_RUN=1; shift ;;
      -h|--help)           usage; exit 0 ;;
      *) log_error "Unknown option: $1"; usage; exit 1 ;;
    esac
  done

  E2E_RESOURCE_GROUP="${E2E_RESOURCE_GROUP:?Set E2E_RESOURCE_GROUP in environment or pass --resource-group}"

  if [[ ! "${E2E_STALE_MAX_AGE_HOURS}" =~ ^[0-9]+$ ]]; then
    log_error "Invalid --max-age-hours: ${E2E_STALE_MAX_AGE_HOURS} (expected a non-negative integer)"
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# _stale_suffixes - Print E2E name suffixes older than the age threshold
#
# Every E2E resource carries the "<kind>-e2e-<suffix>" naming convention where
# the suffix is the epoch second of the run. Suffixes that are not epoch values
# (for example the Bicep uniqueString default) cannot be aged and are reported
# but never deleted.
# ---------------------------------------------------------------------------
_stale_suffixes() {
  local cutoff="$1"
  local names name suffix

  names="$(az resource list --resource-group "${E2E_RESOURCE_GROUP}" \
    --query "[].name" -o tsv)" || return 1

  while read -r name; do
    [[ -n "${name}" ]] || continue
    [[ "${name}" == *"e2e-"* ]] || continue

    # The epoch suffix is a standalone token, for example "aks-e2e-1787164131",
    # "vm-e2e-arc-1787164131-connected", or "vm-e2e-msi-1787164131_OsDisk_1_x".
    if [[ ! "${name}" =~ (^|[-_])([0-9]{9,})([-_]|$) ]]; then
      log_warn "Skipping ${name}: name has no epoch suffix"
      continue
    fi
    suffix="${BASH_REMATCH[2]}"

    if (( suffix < cutoff )); then
      echo "${suffix}"
    fi
  done <<<"${names}" | sort -u
}

# ---------------------------------------------------------------------------
# _resources_for_suffix - Print "<type>\t<name>\t<id>" for one E2E suffix
# ---------------------------------------------------------------------------
_resources_for_suffix() {
  local suffix="$1"

  az resource list --resource-group "${E2E_RESOURCE_GROUP}" \
    --query "[?contains(name, 'e2e-') && contains(name, '${suffix}')].{type:type,name:name,id:id}" \
    -o json |
    jq -r '.[] | [.type, .name, .id] | @tsv'
}

# ---------------------------------------------------------------------------
# _delete_resource - Delete a single resource, honoring dry-run mode
#
# Deletes are best effort: a resource that is still referenced by a pending
# asynchronous delete is picked up by the next scheduled run.
# ---------------------------------------------------------------------------
_delete_resource() {
  local type="$1" name="$2" id="$3"

  if [[ "${E2E_DRY_RUN}" == "1" ]]; then
    log_info "  [dry-run] would delete ${type}/${name}"
    return 0
  fi

  log_info "  Deleting ${type}/${name}..."
  case "${type}" in
    Microsoft.ContainerService/managedClusters)
      az aks delete --resource-group "${E2E_RESOURCE_GROUP}" --name "${name}" \
        --yes --no-wait 2>/dev/null || \
        log_warn "  Failed to delete ${type}/${name}"
      ;;
    Microsoft.Compute/virtualMachines)
      # Wait for VM deletion so the dependent NIC, public IP, and disk deletes
      # below can succeed in the same run.
      az vm delete --ids "${id}" --force-deletion yes --yes 2>/dev/null || \
        log_warn "  Failed to delete ${type}/${name}"
      ;;
    *)
      az resource delete --ids "${id}" 2>/dev/null || \
        log_warn "  Failed to delete ${type}/${name}"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# _delete_suffix - Delete all resources sharing one stale E2E suffix
#
# Resources are deleted in dependency order: Arc machines and AKS clusters
# first, then VMs, then the NICs, public IPs, disks, and network scaffolding
# the VMs referenced.
# ---------------------------------------------------------------------------
_delete_suffix() {
  local suffix="$1" resources="$2"
  local -a ordered_types=(
    "Microsoft.HybridCompute/machines"
    "Microsoft.ContainerService/managedClusters"
    "Microsoft.Compute/virtualMachines"
    "Microsoft.Network/networkInterfaces"
    "Microsoft.Network/publicIPAddresses"
    "Microsoft.Compute/disks"
    "Microsoft.Network/virtualNetworks"
    "Microsoft.Network/networkSecurityGroups"
  )

  local wanted type name id
  for wanted in "${ordered_types[@]}"; do
    while IFS=$'\t' read -r type name id; do
      [[ -n "${id}" ]] || continue
      [[ "${type}" == "${wanted}" ]] || continue
      _delete_resource "${type}" "${name}" "${id}"
    done <<<"${resources}"
  done

  # Anything created outside the known set (for example a future resource type)
  # is still removed so the suffix does not leak.
  local known wanted_type
  while IFS=$'\t' read -r type name id; do
    [[ -n "${id}" ]] || continue
    known=0
    for wanted_type in "${ordered_types[@]}"; do
      if [[ "${type}" == "${wanted_type}" ]]; then
        known=1
        break
      fi
    done
    if (( known == 1 )); then
      continue
    fi
    _delete_resource "${type}" "${name}" "${id}"
  done <<<"${resources}"
}

# ---------------------------------------------------------------------------
# cleanup_stale - Entry point
# ---------------------------------------------------------------------------
cleanup_stale() {
  log_section "Stale E2E Resource Cleanup"

  require_cmd az
  require_cmd jq

  local now cutoff
  now="$(date +%s)"
  cutoff=$(( now - 10#${E2E_STALE_MAX_AGE_HOURS} * 3600 ))

  log_info "Resource Group: ${E2E_RESOURCE_GROUP}"
  log_info "Max Age:        ${E2E_STALE_MAX_AGE_HOURS}h (cutoff epoch ${cutoff})"
  log_info "Mode:           $([[ "${E2E_DRY_RUN}" == "1" ]] && echo "dry-run (list only)" || echo "delete")"

  local resource_group_exists
  resource_group_exists="$(az group exists --name "${E2E_RESOURCE_GROUP}" --output tsv)"
  if [[ "${resource_group_exists}" != "true" ]]; then
    log_warn "Resource group ${E2E_RESOURCE_GROUP} not found; nothing to clean up"
    return 0
  fi

  local suffixes
  suffixes="$(_stale_suffixes "${cutoff}")" || return 1

  if [[ -z "${suffixes}" ]]; then
    log_success "No stale E2E resources found"
    return 0
  fi

  local suffix resources age_hours count=0
  while read -r suffix; do
    [[ -n "${suffix}" ]] || continue
    resources="$(_resources_for_suffix "${suffix}")" || return 1
    [[ -n "${resources}" ]] || continue

    age_hours=$(( (now - suffix) / 3600 ))
    log_info "E2E run ${suffix} (age ${age_hours}h):"
    _delete_suffix "${suffix}" "${resources}"
    count=$(( count + 1 ))
  done <<<"${suffixes}"

  if [[ "${E2E_DRY_RUN}" == "1" ]]; then
    log_success "Found ${count} stale E2E run(s); no resources were deleted (dry-run)"
  else
    log_success "Cleanup initiated for ${count} stale E2E run(s)"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  parse_args "$@"
  cleanup_stale
fi
