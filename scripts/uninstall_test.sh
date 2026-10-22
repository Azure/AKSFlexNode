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
[[ "$HOST_ROOT" == /opt/unbounded/agent ]] || fail "HOST_ROOT is $HOST_ROOT, not the agent library's hostroot.Path"
HOST_ROOT="$WORK_DIR/opt/unbounded/agent"
HOST_ROOT_PARENT="$(dirname "$HOST_ROOT")"
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

# A host installed under the host root: its binaries go, and the host root with them once empty. Its
# parent belongs to the host and stays, even when empty.
populate "$HOST_ROOT"
mkdir -p "$HOST_ROOT/libexec"
remove_binary >/dev/null
assert_removed "$HOST_ROOT"
[[ ! -e "$HOST_ROOT" ]] || fail "empty host root was left: $(find "$HOST_ROOT" | tr '\n' ' ')"
[[ -d "$HOST_ROOT_PARENT" ]] || fail "the host root's parent was removed"

# Files the host staged beside the host root, and the parent's mode, are left alone.
populate "$HOST_ROOT"
mkdir -p "$HOST_ROOT_PARENT/artifacts"
touch "$HOST_ROOT_PARENT/artifacts/manifest.json"
chmod 0750 "$HOST_ROOT_PARENT"
remove_binary >/dev/null
assert_removed "$HOST_ROOT"
[[ ! -e "$HOST_ROOT" ]] || fail "empty host root was left beside the host's files"
[[ -f "$HOST_ROOT_PARENT/artifacts/manifest.json" ]] || fail "a file the host staged beside the host root was removed"
[[ $(stat -c '%a' "$HOST_ROOT_PARENT") == 750 ]] || fail "the mode of the host root's parent was changed"
chmod 0755 "$HOST_ROOT_PARENT"
rm -rf "$HOST_ROOT_PARENT/artifacts"

# A move to the host root that was interrupted leaves its copy beside it and its marker in it, and
# neither keeps the host root.
populate "$HOST_ROOT"
touch "$HOST_ROOT/.moving"
mkdir -p "$HOST_ROOT.staging/bin"
touch "$HOST_ROOT.staging/bin/aks-flex-node"
remove_binary >/dev/null
assert_removed "$HOST_ROOT"
[[ ! -e "$HOST_ROOT" ]] || fail "an unfinished move kept the host root: $(find "$HOST_ROOT" | tr '\n' ' ')"
[[ ! -e "$HOST_ROOT.staging" ]] || fail "an unfinished move's copy was left"

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
# and then the link, but not the parent the link is in.
populate "$LEGACY_ROOT"
mkdir -p "$HOST_ROOT_PARENT"
ln -s "$LEGACY_ROOT" "$HOST_ROOT"
remove_binary >/dev/null
assert_removed "$LEGACY_ROOT"
[[ ! -e "$HOST_ROOT" && ! -L "$HOST_ROOT" ]] || fail "the host root link was left"
[[ -d "$HOST_ROOT_PARENT" ]] || fail "the parent of the host root link was removed"

# A link to anywhere else is not the agent's.
mkdir -p "$WORK_DIR/elsewhere"
ln -s "$WORK_DIR/elsewhere" "$HOST_ROOT"
remove_binary >/dev/null
[[ -L "$HOST_ROOT" ]] || fail "a host root link to somewhere else was removed"
rm "$HOST_ROOT"

# A second run over a clean host succeeds.
remove_binary >/dev/null || fail "remove_binary is not idempotent"

# Whatever reset did, remove_units removes the agent's units, including the first-boot unit from an
# Ignition install, and the units that run the agent library's helpers.
SYSTEMCTL_CALLS="$WORK_DIR/systemctl-calls"
systemctl() { printf '%s\n' "$*" >>"$SYSTEMCTL_CALLS"; }
SERVICE_UNIT_PATH="$WORK_DIR/systemd/$SERVICE_UNIT"
RECOVERY_UNIT_PATH="$WORK_DIR/systemd/aks-flex-node-agent-recovery.service"
FIRST_BOOT_UNIT_PATH="$WORK_DIR/systemd/$FIRST_BOOT_UNIT"
HELPER_UNIT_DIR="$WORK_DIR/systemd"

# populate_units lays out the units reset would have removed: the agent's, the first-boot unit, and
# the helpers' units, with a drop-in of someone else's beside a machine's.
populate_units() {
    mkdir -p "$HELPER_UNIT_DIR/systemd-nspawn@kube1.service.d" "$HELPER_UNIT_DIR/systemd-nspawn@kube2.service.d"
    touch "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH" "$FIRST_BOOT_UNIT_PATH" \
        "$HELPER_UNIT_DIR/$LOCALDNS_UNIT" \
        "$HELPER_UNIT_DIR/systemd-nspawn@kube1.service.d/override.conf" \
        "$HELPER_UNIT_DIR/systemd-nspawn@kube2.service.d/override.conf" \
        "$HELPER_UNIT_DIR/systemd-nspawn@kube2.service.d/10-someone-else.conf" \
        "$HELPER_UNIT_DIR/unbounded-agent-regenerate-config@kube1.service" \
        "$HELPER_UNIT_DIR/unbounded-agent-regenerate-config@kube2.service"
}

assert_units_removed() {
    local unit_path
    for unit_path in "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH" "$FIRST_BOOT_UNIT_PATH" \
        "$HELPER_UNIT_DIR/$LOCALDNS_UNIT" \
        "$HELPER_UNIT_DIR/systemd-nspawn@kube1.service.d" \
        "$HELPER_UNIT_DIR/systemd-nspawn@kube2.service.d/override.conf" \
        "$HELPER_UNIT_DIR/unbounded-agent-regenerate-config@kube1.service" \
        "$HELPER_UNIT_DIR/unbounded-agent-regenerate-config@kube2.service"; do
        [[ ! -e "$unit_path" ]] || fail "unit was left: $unit_path"
    done
    [[ -f "$HELPER_UNIT_DIR/systemd-nspawn@kube2.service.d/10-someone-else.conf" ]] ||
        fail "a machine's drop-in that isn't the agent's was removed"
    grep -Fxq "disable --now aks-flex-node-bootstrap.service" "$SYSTEMCTL_CALLS" ||
        fail "first-boot unit was not stopped: $(tr '\n' ';' <"$SYSTEMCTL_CALLS")"
    grep -Fxq "disable --now $LOCALDNS_UNIT" "$SYSTEMCTL_CALLS" ||
        fail "the LocalDNS unit was not stopped: $(tr '\n' ';' <"$SYSTEMCTL_CALLS")"
}

# A reset that fails leaves the units behind, enabled and naming binaries that are about to go.
mkdir -p "$WORK_DIR/failing-reset/bin"
printf '#!/bin/sh\nexit 1\n' > "$WORK_DIR/failing-reset/bin/aks-flex-node"
chmod 0755 "$WORK_DIR/failing-reset/bin/aks-flex-node"
INSTALL_DIR="$WORK_DIR/failing-reset/bin"
: >"$SYSTEMCTL_CALLS"
populate_units
run_reset >/dev/null
[[ "$RESET_RESULT" == failed ]] || fail "a failed reset was recorded as $RESET_RESULT"
remove_units >/dev/null
assert_units_removed

# Without a binary to run reset, the same units go, and the helpers under both roots with the
# binaries, so the host root goes too.
INSTALL_DIR="$WORK_DIR/no-binary/bin"
: >"$SYSTEMCTL_CALLS"
populate_units
run_reset >/dev/null
[[ "$RESET_RESULT" == skipped ]] || fail "a reset that didn't run was recorded as $RESET_RESULT"
remove_units >/dev/null
assert_units_removed
for root in "$HOST_ROOT" "$LEGACY_ROOT"; do
    mkdir -p "$root/bin" "$root/libexec"
    touch "$root/bin/unbounded-agent-nspawn-lifecycle" "$root/libexec/unbounded-localdns-network"
done
HOST_ROOT_KEPT=false
remove_binary >/dev/null
for root in "$HOST_ROOT" "$LEGACY_ROOT"; do
    [[ ! -e "$root/bin/unbounded-agent-nspawn-lifecycle" && ! -e "$root/libexec/unbounded-localdns-network" ]] ||
        fail "a helper was left under $root"
done
[[ ! -e "$HOST_ROOT" ]] || fail "the helpers kept the host root: $(find "$HOST_ROOT" | tr '\n' ' ')"
[[ -d "$HOST_ROOT_PARENT" ]] || fail "the host root's parent was removed"

# A host not provisioned by Ignition has no first-boot unit, and nothing is done about one.
: >"$SYSTEMCTL_CALLS"
touch "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH"
output=$(remove_units)
for unit_path in "$SERVICE_UNIT_PATH" "$RECOVERY_UNIT_PATH"; do
    [[ ! -e "$unit_path" ]] || fail "unit was left: $unit_path"
done
if grep -q aks-flex-node-bootstrap "$SYSTEMCTL_CALLS" || grep -q aks-flex-node-bootstrap <<<"$output"; then
    fail "a host without the first-boot unit was told about it: $(tr '\n' ';' <"$SYSTEMCTL_CALLS") $output"
fi
unset -f systemctl

# The completion message claims the host's pre-installation state only when reset ran and the host
# root went.
RESET_RESULT=completed HOST_ROOT_KEPT=false
grep -q "pre-installation state" <<<"$(show_completion_message)" ||
    fail "a complete uninstall didn't say so"
for state in "skipped false" "failed false" "completed true"; do
    read -r RESET_RESULT HOST_ROOT_KEPT <<<"$state"
    output=$(show_completion_message)
    if grep -q "pre-installation state" <<<"$output"; then
        fail "reset $RESET_RESULT, host root kept $HOST_ROOT_KEPT, claimed the pre-installation state: $output"
    fi
done
[[ "$(RESET_RESULT=completed HOST_ROOT_KEPT=true show_completion_message)" == *"$HOST_ROOT, which still holds files"* ]] ||
    fail "a kept host root was not reported"

printf 'uninstall_test: ok\n'
