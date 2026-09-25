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
HOST_ROOT="$WORK_DIR/opt/unbounded"
LEGACY_ROOT="$WORK_DIR/usr/local"

# populate lays out a release's binaries under a root: the compatibility link, dangling as reset
# can leave it, and the managed layout reset keeps.
populate() {
    local root="$1"
    mkdir -p "$root/bin" "$root/lib/aks-flex-node"
    ln -s "$root/lib/aks-flex-node/aks-flex-node-current" "$root/bin/aks-flex-node"
    touch "$root/lib/aks-flex-node/aks-flex-node-blue"
}

assert_removed() {
    local root="$1"
    [[ ! -e "$root/bin/aks-flex-node" && ! -L "$root/bin/aks-flex-node" ]] || fail "binary link was left under $root"
    [[ ! -e "$root/lib/aks-flex-node" ]] || fail "managed binary layout was left under $root"
}

# reset runs the binary from whichever root holds it.
populate "$LEGACY_ROOT"
printf '#!/bin/sh\n' > "$LEGACY_ROOT/lib/aks-flex-node/aks-flex-node-current"
chmod 0755 "$LEGACY_ROOT/lib/aks-flex-node/aks-flex-node-current"
find_install_dir
[[ "$INSTALL_DIR" == "$LEGACY_ROOT/bin" ]] || fail "reset would not run the binary under the legacy root: $INSTALL_DIR"
rm -rf "$LEGACY_ROOT"

# A host installed under the host root: its binaries go, and the host root with them once empty.
populate "$HOST_ROOT"
mkdir -p "$HOST_ROOT/libexec"
remove_binary >/dev/null
assert_removed "$HOST_ROOT"
[[ ! -e "$HOST_ROOT" ]] || fail "empty host root was left: $(find "$HOST_ROOT" | tr '\n' ' ')"

# The legacy root is swept as well, because uninstalling does not depend on the host root having
# been migrated, and a host root that still holds something else is kept.
populate "$LEGACY_ROOT"
mkdir -p "$HOST_ROOT/bin"
touch "$HOST_ROOT/bin/someone-else"
remove_binary >/dev/null
assert_removed "$LEGACY_ROOT"
[[ -e "$HOST_ROOT/bin/someone-else" ]] || fail "a host root holding other files was removed"
rm -rf "$HOST_ROOT"

# On a migrated host the host root is a link to the legacy root. The files are removed through it,
# and then the link.
populate "$LEGACY_ROOT"
mkdir -p "$(dirname "$HOST_ROOT")"
ln -s "$LEGACY_ROOT" "$HOST_ROOT"
remove_binary >/dev/null
assert_removed "$LEGACY_ROOT"
[[ ! -e "$HOST_ROOT" && ! -L "$HOST_ROOT" ]] || fail "the host root link was left"

# A link to anywhere else is not the agent's.
mkdir -p "$WORK_DIR/elsewhere"
ln -s "$WORK_DIR/elsewhere" "$HOST_ROOT"
remove_binary >/dev/null
[[ -L "$HOST_ROOT" ]] || fail "a host root link to somewhere else was removed"
rm "$HOST_ROOT"

# A second run over a clean host succeeds.
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
