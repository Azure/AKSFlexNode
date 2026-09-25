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
    umount "$WORK_DIR/ro" 2>/dev/null || true
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

# The install directories come from the downloaded binary. It is asked from a copy under DATA_DIR,
# because the download directory may be on a noexec /tmp.
DATA_DIR="$WORK_DIR/data"
HOST_ROOT_CALLS="$WORK_DIR/host-root-calls"

# make_stub writes a binary that answers host-root with $2, or, with no $2, a release from before the
# host root, which has no such command.
make_stub() {
    local path="$1" root="${2:-}"
    {
        printf '#!/bin/bash\n'
        printf 'if [[ "${1:-}" == host-root ]]; then\n'
        printf '    printf "%%s\\n" "$0" >> %q\n' "$HOST_ROOT_CALLS"
        if [[ -n "$root" ]]; then
            printf '    printf "%%s\\n" %q\n    exit 0\n' "$root"
        else
            printf '    echo "Error: unknown command \\"host-root\\"" >&2\n    exit 1\n'
        fi
        printf 'fi\nprintf "%%s\\n" "$*"\n'
    } > "$path"
    chmod 0755 "$path"
}

make_stub "$WORK_DIR/current-agent" "$WORK_DIR/root/opt/unbounded/"
LEGACY_AGENT=false
resolve_install_dir "$WORK_DIR/current-agent" >/dev/null || fail "resolving a current release failed"
[[ "$INSTALL_DIR" == "$WORK_DIR/root/opt/unbounded/bin" &&
   "$MANAGED_BINARY_DIR" == "$WORK_DIR/root/opt/unbounded/lib/aks-flex-node" ]] ||
    fail "a current release was not installed under its host root: $INSTALL_DIR, $MANAGED_BINARY_DIR"
[[ "$LEGACY_AGENT" == false ]] || fail "a current release was taken for an older one"
[[ "$(tail -1 "$HOST_ROOT_CALLS")" == "$DATA_DIR"/.install.*/aks-flex-node ]] ||
    fail "host-root ran from $(tail -1 "$HOST_ROOT_CALLS"), want the staging copy"
if compgen -G "$DATA_DIR/.install.*" >/dev/null; then
    fail "the staging copy was left in $DATA_DIR"
fi

# The host root does not exist before the first install.
install_binary "$replacement_binary" >/dev/null || fail "install into a missing host root failed"
[[ -x "$INSTALL_DIR/aks-flex-node" ]] || fail "binary missing under the new host root"
[[ "$(stat -c %a "$INSTALL_DIR")" == "755" ]] || fail "host root bin dir mode is $(stat -c %a "$INSTALL_DIR"), want 755"

make_stub "$WORK_DIR/relative-agent" "relative/root"
if resolve_install_dir "$WORK_DIR/relative-agent" >/dev/null 2>&1; then
    fail "a relative host root was accepted"
fi

make_stub "$WORK_DIR/legacy-agent"
resolve_install_dir "$WORK_DIR/legacy-agent" >"$WORK_DIR/legacy.log" 2>&1 || fail "resolving an older release failed"
[[ "$INSTALL_DIR" == "/usr/local/bin" && "$MANAGED_BINARY_DIR" == "/usr/local/lib/aks-flex-node" ]] ||
    fail "an older release was not installed under /usr/local: $INSTALL_DIR"
[[ "$LEGACY_AGENT" == true ]] || fail "an older release was not recorded as one"
grep -q "predates /opt/unbounded" "$WORK_DIR/legacy.log" || fail "an older release was not reported"

# Where that directory is read-only, as on Azure Container Linux, the operator is told why.
mkdir -p "$WORK_DIR/ro"
mount -t tmpfs -o ro tmpfs "$WORK_DIR/ro" || fail "could not mount a read-only directory"
INSTALL_DIR="$WORK_DIR/ro"
if install_binary "$replacement_binary" >"$WORK_DIR/ro.log" 2>&1; then
    fail "installed into a read-only directory"
fi
umount "$WORK_DIR/ro"
grep -q "predates /opt/unbounded and needs a writable" "$WORK_DIR/ro.log" || fail "a read-only install directory was not explained"

printf 'install_test: ok\n'
