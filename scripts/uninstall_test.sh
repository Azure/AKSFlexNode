#!/bin/bash

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/uninstall.sh"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

fail() {
    printf 'uninstall_test: %s\n' "$*" >&2
    exit 1
}

bash -n "$SCRIPT"

# shellcheck source=scripts/uninstall.sh
source "$SCRIPT"
CONFIG_DIR="$WORK_DIR/etc"
mkdir -p "$CONFIG_DIR"

# The prefix has to be known before reset removes the config that records it.
AKS_FLEX_NODE_HOST_PREFIX=""
resolve_host_prefix
[[ "$INSTALL_DIR" == "/usr/local/bin" && "$MANAGED_BINARY_DIR" == "/usr/local/lib/aks-flex-node" ]] ||
    fail "default prefix changed the layout: $INSTALL_DIR"

AKS_FLEX_NODE_HOST_PREFIX="/opt/aks-flex-node/"
resolve_host_prefix
[[ "$INSTALL_DIR" == "/opt/aks-flex-node/bin" ]] || fail "env prefix was not applied: $INSTALL_DIR"

if command -v jq >/dev/null; then
    printf '{"agent":{"hostPrefix":"/opt/from-config"}}' >"$CONFIG_DIR/config.json"
    AKS_FLEX_NODE_HOST_PREFIX=""
    resolve_host_prefix
    [[ "$INSTALL_DIR" == "/opt/from-config/bin" ]] || fail "config prefix was not applied: $INSTALL_DIR"
fi

# remove_binary removes the compatibility link, even when dangling, and the managed layout that
# reset keeps.
INSTALL_DIR="$WORK_DIR/prefix/bin"
MANAGED_BINARY_DIR="$WORK_DIR/prefix/lib/aks-flex-node"
mkdir -p "$INSTALL_DIR" "$MANAGED_BINARY_DIR"
ln -s "$MANAGED_BINARY_DIR/aks-flex-node-current" "$INSTALL_DIR/aks-flex-node"
touch "$MANAGED_BINARY_DIR/aks-flex-node-blue"

remove_binary >/dev/null
[[ ! -e "$INSTALL_DIR/aks-flex-node" && ! -L "$INSTALL_DIR/aks-flex-node" ]] || fail "dangling binary link was left"
[[ ! -e "$MANAGED_BINARY_DIR" ]] || fail "managed binary layout was left"

# A second run over a clean prefix succeeds.
remove_binary >/dev/null || fail "remove_binary is not idempotent"

printf 'uninstall_test: ok\n'
