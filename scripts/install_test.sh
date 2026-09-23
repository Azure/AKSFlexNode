#!/bin/bash

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    exec sudo -E bash "$0" "$@"
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/install.sh"
WORK_DIR=$(mktemp -d)
RUNNING_PID=""
cleanup() {
    [[ -z "$RUNNING_PID" ]] || kill "$RUNNING_PID" 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

fail() {
    printf 'install_test: %s\n' "$*" >&2
    exit 1
}

bash -n "$SCRIPT"
command -v flock >/dev/null || fail "flock is required"
command -v setpriv >/dev/null || fail "setpriv is required"

# The documented curl | bash invocation must reach main even though BASH_SOURCE is empty.
stdin_output=$(setpriv --reuid=65534 --regid=65534 --clear-groups bash <"$SCRIPT" 2>&1 || true)
grep -q "This script must be run as root" <<<"$stdin_output" || fail "stdin invocation did not reach main"

source "$SCRIPT"
INSTALL_DIR="$WORK_DIR/bin"
MANAGED_BINARY_DIR="$WORK_DIR/lib/aks-flex-node"
AGENT_UPGRADE_LOCK_PATH="$WORK_DIR/run/agent-upgrade.lock"
SERVICE_UNIT="aks-flex-node-agent-install-test-$RANDOM.service"
SERVICE_UNIT_PATH="$WORK_DIR/systemd/$SERVICE_UNIT"
mkdir -p "$MANAGED_BINARY_DIR" "$(dirname "$SERVICE_UNIT_PATH")"

running_binary=$(type -P sleep)
replacement_binary=$(type -P printf)

# Minimal images can omit an empty /usr/local/bin. The installer owns creation
# of its configured destination before atomically staging the binary there.
[[ ! -e "$INSTALL_DIR" ]] || fail "missing install directory test setup is invalid"
install_binary "$replacement_binary" >/dev/null || fail "installer did not create a missing install directory"
[[ -d "$INSTALL_DIR" && ! -L "$INSTALL_DIR" ]] || fail "installer did not create a regular install directory"
[[ "$(stat -c '%a %U %G' "$INSTALL_DIR")" == "755 root root" ]] || fail "installer created install directory with incorrect ownership or mode"
[[ "$($INSTALL_DIR/aks-flex-node missing-dir)" == "missing-dir" ]] || fail "binary installed into newly created directory is not executable"
rm "$INSTALL_DIR/aks-flex-node"

# Existing host-managed directory permissions must not be rewritten.
chmod 0711 "$INSTALL_DIR"
install_binary "$replacement_binary" >/dev/null || fail "installer rejected an existing install directory"
[[ "$(stat -c '%a' "$INSTALL_DIR")" == "711" ]] || fail "installer changed existing install directory permissions"
rm "$INSTALL_DIR/aks-flex-node"
chmod 0755 "$INSTALL_DIR"

cp "$running_binary" "$INSTALL_DIR/aks-flex-node"
"$INSTALL_DIR/aks-flex-node" 30 &
RUNNING_PID=$!
sleep 0.1
kill -0 "$RUNNING_PID" 2>/dev/null || fail "test binary did not remain running"

install_binary "$replacement_binary" >/dev/null || fail "could not replace a running binary"
kill -0 "$RUNNING_PID" 2>/dev/null || fail "replacement stopped the old process"
[[ "$("$INSTALL_DIR/aks-flex-node" replacement)" == "replacement" ]] || fail "new executions did not use the replacement"

assert_no_staged_files() {
    [[ -z "$(find "$INSTALL_DIR" -name '.aks-flex-node.*' -print -quit)" ]] || fail "staged binary was not cleaned up"
}
assert_no_staged_files

# Managed installations must use the upgrade flow; the installer must not detach this link.
cp "$replacement_binary" "$MANAGED_BINARY_DIR/aks-flex-node-blue"
ln -s "$MANAGED_BINARY_DIR/aks-flex-node-blue" "$MANAGED_BINARY_DIR/aks-flex-node-current"
rm "$INSTALL_DIR/aks-flex-node"
ln -s "$MANAGED_BINARY_DIR/aks-flex-node-current" "$INSTALL_DIR/aks-flex-node"
touch "$SERVICE_UNIT_PATH"
if install_binary "$running_binary" >"$WORK_DIR/managed.log" 2>&1; then
    fail "installer replaced a managed binary symlink"
fi
grep -q "aks-flex-node reset" "$WORK_DIR/managed.log" || fail "managed reinstall guidance omitted reset"
[[ -L "$INSTALL_DIR/aks-flex-node" ]] || fail "managed binary symlink was removed"
[[ "$(readlink -f "$INSTALL_DIR/aks-flex-node")" == "$MANAGED_BINARY_DIR/aks-flex-node-blue" ]] || fail "managed activation changed"
assert_no_staged_files

# Reset removes the service unit; reinstall must clean the retained layout and install directly.
rm "$SERVICE_UNIT_PATH"
install_binary "$running_binary" >"$WORK_DIR/reset-reinstall.log" 2>&1 || fail "reinstall after reset failed"
grep -q "layout retained after reset" "$WORK_DIR/reset-reinstall.log" || fail "retained layout cleanup was not reported"
[[ ! -L "$INSTALL_DIR/aks-flex-node" && -x "$INSTALL_DIR/aks-flex-node" ]] || fail "reset reinstall did not install a direct binary"
[[ ! -e "$MANAGED_BINARY_DIR" ]] || fail "reset reinstall retained the managed layout"
assert_no_staged_files

# Unexpected links must be rejected without advising root to execute their targets.
rm "$INSTALL_DIR/aks-flex-node"
ln -s "$replacement_binary" "$INSTALL_DIR/aks-flex-node"
if install_binary "$running_binary" >"$WORK_DIR/unexpected.log" 2>&1; then
    fail "installer replaced an unexpected binary symlink"
fi
if grep -q "aks-flex-node reset" "$WORK_DIR/unexpected.log"; then
    fail "unexpected symlink guidance advised executing the link"
fi
grep -q "do not execute" "$WORK_DIR/unexpected.log" || fail "unexpected symlink safety guidance was omitted"
[[ "$(readlink "$INSTALL_DIR/aks-flex-node")" == "$replacement_binary" ]] || fail "unexpected symlink changed"
assert_no_staged_files

# A malformed directory target must fail rather than receive the staged binary.
rm "$INSTALL_DIR/aks-flex-node"
mkdir "$INSTALL_DIR/aks-flex-node"
if install_binary "$replacement_binary" >/dev/null 2>&1; then
    fail "installer accepted a directory target"
fi
[[ -z "$(find "$INSTALL_DIR/aks-flex-node" -mindepth 1 -print -quit)" ]] || fail "installer moved the staged binary into a directory"
rmdir "$INSTALL_DIR/aks-flex-node"
assert_no_staged_files

# Failed staging must leave the previous installation intact.
install_binary "$replacement_binary" >/dev/null
if install_binary "$WORK_DIR/missing" >/dev/null 2>&1; then
    fail "installer accepted a missing source"
fi
[[ "$("$INSTALL_DIR/aks-flex-node" replacement)" == "replacement" ]] || fail "failed staging replaced the installed binary"
assert_no_staged_files

# Installer and managed activation paths must serialize on the same lock.
(
    exec 8>>"$AGENT_UPGRADE_LOCK_PATH"
    flock -n 8
    if install_binary "$replacement_binary" >/dev/null 2>&1; then
        fail "installer ignored the activation lock"
    fi
)

# Host prefix resolution. The agent resolves its binaries from agent.hostPrefix, so the installer
# has to install under the same prefix.
CONFIG_DIR="$WORK_DIR/etc"
mkdir -p "$CONFIG_DIR"

AKS_FLEX_NODE_HOST_PREFIX=""
resolve_host_prefix || fail "resolving the default prefix failed"
[[ "$HOST_PREFIX" == "/usr/local" && "$INSTALL_DIR" == "/usr/local/bin" &&
   "$MANAGED_BINARY_DIR" == "/usr/local/lib/aks-flex-node" ]] || fail "default prefix changed the layout"

AKS_FLEX_NODE_HOST_PREFIX="/opt/aks-flex-node/"
resolve_host_prefix || fail "resolving the env prefix failed"
[[ "$INSTALL_DIR" == "/opt/aks-flex-node/bin" &&
   "$MANAGED_BINARY_DIR" == "/opt/aks-flex-node/lib/aks-flex-node" ]] || fail "env prefix was not applied: $INSTALL_DIR"

if command -v jq >/dev/null; then
    printf '{"agent":{"hostPrefix":"/opt/from-config"}}' >"$CONFIG_DIR/config.json"
    AKS_FLEX_NODE_HOST_PREFIX=""
    resolve_host_prefix || fail "resolving the config prefix failed"
    [[ "$INSTALL_DIR" == "/opt/from-config/bin" ]] || fail "config prefix was not applied: $INSTALL_DIR"

    AKS_FLEX_NODE_HOST_PREFIX="/opt/other"
    if resolve_host_prefix >"$WORK_DIR/conflict.log" 2>&1; then
        fail "a prefix that disagrees with agent.hostPrefix was accepted"
    fi
    grep -q "does not match agent.hostPrefix" "$WORK_DIR/conflict.log" || fail "conflict was not explained"
    rm "$CONFIG_DIR/config.json"
fi

for bad in "relative/path" "/opt/with space"; do
    AKS_FLEX_NODE_HOST_PREFIX="$bad"
    if resolve_host_prefix >/dev/null 2>&1; then
        fail "invalid prefix accepted: $bad"
    fi
done
AKS_FLEX_NODE_HOST_PREFIX=""

# A custom prefix does not exist before the first install.
INSTALL_DIR="$WORK_DIR/prefix/bin"
MANAGED_BINARY_DIR="$WORK_DIR/prefix/lib/aks-flex-node"
install_binary "$replacement_binary" >/dev/null || fail "install into a missing prefix failed"
[[ -x "$INSTALL_DIR/aks-flex-node" ]] || fail "binary missing under the new prefix"
[[ "$(stat -c %a "$INSTALL_DIR")" == "755" ]] || fail "prefix bin dir mode is $(stat -c %a "$INSTALL_DIR"), want 755"

# Azure Container Linux reports ID=azurelinux 3.x like Azure Linux 3, but /usr is read-only, so it
# must not be accepted with the default prefix.
write_os_release() {
    OS_RELEASE_PATH="$WORK_DIR/os-release"
    printf '%s\n' "$@" >"$OS_RELEASE_PATH"
}

write_os_release 'ID=azurelinux' 'ID_LIKE="flatcar"' 'VARIANT_ID=azurecontainerlinux' 'VERSION_ID=3.0.20260918'
HOST_PREFIX="/usr/local"
if (check_linux_distribution) >"$WORK_DIR/acl-default.log" 2>&1; then
    fail "Azure Container Linux was accepted with the default prefix"
fi
grep -q "AKS_FLEX_NODE_HOST_PREFIX" "$WORK_DIR/acl-default.log" || fail "ACL refusal did not say what to set"

HOST_PREFIX="/opt/aks-flex-node"
(check_linux_distribution) >"$WORK_DIR/acl-prefix.log" 2>&1 || fail "Azure Container Linux with a prefix was refused"
grep -q "Detected Azure Container Linux" "$WORK_DIR/acl-prefix.log" || fail "ACL was not identified"

# ID_LIKE=flatcar alone still identifies it, in case an image drops VARIANT_ID.
write_os_release 'ID=azurelinux' 'ID_LIKE="flatcar"' 'VERSION_ID=3.0.20260918'
HOST_PREFIX="/usr/local"
if (check_linux_distribution) >/dev/null 2>&1; then
    fail "ACL without VARIANT_ID was treated as Azure Linux 3"
fi

# Plain Azure Linux 3 is unaffected.
write_os_release 'ID=azurelinux' 'VERSION_ID=3.0.20250101'
(check_linux_distribution) >"$WORK_DIR/azl.log" 2>&1 || fail "Azure Linux 3 was refused"
grep -q "Detected Azure Linux" "$WORK_DIR/azl.log" || fail "Azure Linux 3 was not identified"

printf 'install_test: ok\n'
