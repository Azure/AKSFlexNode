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

# Without a binary to run reset, run_reset removes the units itself, including the first-boot unit
# from an Ignition install.
SYSTEMCTL_CALLS="$WORK_DIR/systemctl-calls"
systemctl() { printf '%s\n' "$*" >>"$SYSTEMCTL_CALLS"; }
INSTALL_DIR="$WORK_DIR/no-binary/bin"
SERVICE_UNIT_PATH="$WORK_DIR/systemd/$SERVICE_UNIT"
RECOVERY_UNIT_PATH="$WORK_DIR/systemd/aks-flex-node-agent-recovery.service"
FIRST_BOOT_UNIT_PATH="$WORK_DIR/systemd/$FIRST_BOOT_UNIT"
mkdir -p "$WORK_DIR/systemd"
touch "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH" "$FIRST_BOOT_UNIT_PATH"

run_reset >/dev/null
for unit_path in "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH" "$FIRST_BOOT_UNIT_PATH"; do
    [[ ! -e "$unit_path" ]] || fail "unit was left: $unit_path"
done
grep -Fxq "disable --now aks-flex-node-bootstrap.service" "$SYSTEMCTL_CALLS" ||
    fail "first-boot unit was not stopped: $(tr '\n' ';' <"$SYSTEMCTL_CALLS")"
unset -f systemctl

printf 'uninstall_test: ok\n'
