#!/bin/bash

set -euo pipefail

fail() {
    printf 'install_test: %s\n' "$*" >&2
    exit 1
}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# sudo can reset PATH even with -E. Build with the caller's Go toolchain before
# entering the privileged phase, as bootstrap_test.sh does.
if [[ "${1:-}" != --run-privileged ]]; then
    command -v go >/dev/null || fail "Go is required to build native test fixtures"
    BUILD_DIR=$(mktemp -d)
    trap 'rm -rf "$BUILD_DIR"' EXIT
    for version in old new; do
        # These fixtures use only the standard library, not the repository's module.
        CGO_ENABLED=0 GO111MODULE=off GOTOOLCHAIN=local go build \
            -ldflags "-X main.version=$version" -o "$BUILD_DIR/$version" \
            "$REPO_ROOT/scripts/install_test_fixture.go"
    done
    if [[ $EUID -eq 0 ]]; then
        bash "$0" --run-privileged "$BUILD_DIR/old" "$BUILD_DIR/new"
    else
        sudo -E bash "$0" --run-privileged "$BUILD_DIR/old" "$BUILD_DIR/new"
    fi
    exit
fi
[[ $EUID -eq 0 && $# -eq 3 ]] || fail "privileged phase requires root and both native fixtures"
running_binary="$2"
replacement_binary="$3"

SCRIPT="$REPO_ROOT/scripts/install.sh"
WORK_DIR=$(mktemp -d)
RUNNING_PID=""
cleanup() {
    if [[ -n "$RUNNING_PID" ]]; then
        kill "$RUNNING_PID" 2>/dev/null || true
        wait "$RUNNING_PID" 2>/dev/null || true
    fi
    umount "$WORK_DIR/readonly/usr/local" 2>/dev/null || true
    umount "$WORK_DIR/readonly-staged/usr/local" 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

bash -n "$SCRIPT"
command -v flock >/dev/null || fail "flock is required"
command -v setpriv >/dev/null || fail "setpriv is required"

# The documented curl | bash invocation must reach main even though BASH_SOURCE is empty.
stdin_output=$(setpriv --reuid=65534 --regid=65534 --clear-groups bash <"$SCRIPT" 2>&1 || true)
grep -q "This script must be run as root" <<<"$stdin_output" || fail "stdin invocation did not reach main"

# shellcheck source=install.sh
source "$SCRIPT"
[[ "$HOST_ROOT" == /opt/unbounded/agent ]] || fail "HOST_ROOT is $HOST_ROOT, not the agent library's hostroot.Path"
INSTALL_DIR="$WORK_DIR/bin"
MANAGED_BINARY_DIR="$WORK_DIR/lib/aks-flex-node"
AGENT_UPGRADE_LOCK_PATH="$WORK_DIR/run/agent-upgrade.lock"
SERVICE_UNIT="aks-flex-node-agent-install-test-$RANDOM.service"
SERVICE_UNIT_PATH="$WORK_DIR/systemd/$SERVICE_UNIT"
mkdir -p "$MANAGED_BINARY_DIR" "$(dirname "$SERVICE_UNIT_PATH")"

# Distinct native executables keep executable-inode replacement coverage without
# depending on a multicall utility's dispatch name or argv[0].
[[ "$("$running_binary" print fixture)" == "old:fixture" ]] || fail "old fixture did not execute"
[[ "$("$replacement_binary" print '%s value with spaces')" == "new:%s value with spaces" ]] || fail "new fixture did not print its literal argument"
for fixture in "$running_binary" "$replacement_binary"; do
    if "$fixture" unexpected >/dev/null 2>&1; then
        fail "fixture accepted an unknown mode"
    fi
done

run_replacement() {
    "$INSTALL_DIR/aks-flex-node" print "$@"
}

# Minimal images can omit an empty /usr/local/bin. The installer owns creation
# of its configured destination before atomically staging the binary there.
[[ ! -e "$INSTALL_DIR" ]] || fail "missing install directory test setup is invalid"
install_binary "$replacement_binary" >/dev/null || fail "installer did not create a missing install directory"
[[ -d "$INSTALL_DIR" && ! -L "$INSTALL_DIR" ]] || fail "installer did not create a regular install directory"
[[ "$(stat -c '%a %U %G' "$INSTALL_DIR")" == "755 root root" ]] || fail "installer created install directory with incorrect ownership or mode"
[[ "$(run_replacement missing-dir)" == "new:missing-dir" ]] || fail "binary installed into newly created directory is not executable"
rm "$INSTALL_DIR/aks-flex-node"

# Existing host-managed directory permissions must not be rewritten.
chmod 0711 "$INSTALL_DIR"
install_binary "$replacement_binary" >/dev/null || fail "installer rejected an existing install directory"
[[ "$(stat -c '%a' "$INSTALL_DIR")" == "711" ]] || fail "installer changed existing install directory permissions"
rm "$INSTALL_DIR/aks-flex-node"
chmod 0755 "$INSTALL_DIR"

cp "$running_binary" "$INSTALL_DIR/aks-flex-node"
mkfifo "$WORK_DIR/ready"
"$INSTALL_DIR/aks-flex-node" wait >"$WORK_DIR/ready" &
RUNNING_PID=$!
read -r -t 10 ready <"$WORK_DIR/ready" || fail "old fixture did not become ready"
[[ "$ready" == "old:ready" ]] || fail "running fixture is not the old binary"
kill -0 "$RUNNING_PID" 2>/dev/null || fail "test binary did not remain running"
[[ "/proc/$RUNNING_PID/exe" -ef "$INSTALL_DIR/aks-flex-node" ]] || fail "old process is not executing the installed native binary"
old_inode=$(stat -Lc '%d:%i' "/proc/$RUNNING_PID/exe")

install_binary "$replacement_binary" >/dev/null || fail "could not replace a running binary"
kill -0 "$RUNNING_PID" 2>/dev/null || fail "replacement stopped the old process"
[[ "$(stat -Lc '%d:%i' "/proc/$RUNNING_PID/exe")" == "$old_inode" ]] || fail "old process changed executable inode"
[[ "$(stat -c '%d:%i' "$INSTALL_DIR/aks-flex-node")" != "$old_inode" ]] || fail "replacement reused the running executable inode"
[[ "$(run_replacement replacement)" == "new:replacement" ]] || fail "new executions did not use the replacement"

assert_no_staged_files() {
    [[ -z "$(find "$INSTALL_DIR" -name '.aks-flex-node.*' -print -quit)" ]] || fail "staged binary was not cleaned up"
}
assert_no_staged_files

# Managed installations must use the upgrade flow; the installer must not detach this link.
mkdir -p "$MANAGED_BINARY_DIR"
cp "$replacement_binary" "$MANAGED_BINARY_DIR/aks-flex-node-blue"
ln -s "$MANAGED_BINARY_DIR/aks-flex-node-blue" "$MANAGED_BINARY_DIR/aks-flex-node-current"
rm "$INSTALL_DIR/aks-flex-node"
ln -s "$MANAGED_BINARY_DIR/aks-flex-node-current" "$INSTALL_DIR/aks-flex-node"
touch "$SERVICE_UNIT_PATH"
if install_binary "$running_binary" >"$WORK_DIR/managed.log" 2>&1; then
    fail "installer replaced a managed binary symlink"
fi
grep -q "reset the node" "$WORK_DIR/managed.log" || fail "managed reinstall guidance omitted reset"
! grep -q "$INSTALL_DIR/aks-flex-node reset" "$WORK_DIR/managed.log" || fail "managed reinstall guidance said to run the link"
[[ -L "$INSTALL_DIR/aks-flex-node" ]] || fail "managed binary symlink was removed"
[[ "$(readlink -f "$INSTALL_DIR/aks-flex-node")" == "$MANAGED_BINARY_DIR/aks-flex-node-blue" ]] || fail "managed activation changed"
assert_no_staged_files

# An earlier release's reset removed the service unit but kept the layout. Reinstalling replaces the
# link with the binary and removes the layout, which an agent from before start removes it would
# point the link back at.
rm "$SERVICE_UNIT_PATH"
install_binary "$running_binary" >"$WORK_DIR/reset-reinstall.log" 2>&1 || fail "reinstall after reset failed"
[[ ! -L "$INSTALL_DIR/aks-flex-node" && -x "$INSTALL_DIR/aks-flex-node" ]] || fail "reset reinstall did not install a direct binary"
cmp -s "$INSTALL_DIR/aks-flex-node" "$running_binary" || fail "reset reinstall did not install the new binary"
[[ ! -e "$MANAGED_BINARY_DIR" ]] || fail "reset reinstall kept the layout an earlier agent would run"
grep -q "layout a reset kept" "$WORK_DIR/reset-reinstall.log" || fail "removing the retained layout was not reported"
assert_no_staged_files

# Any other link at the binary path is replaced too, once no agent is installed, which leaves what it
# led to alone.
rm "$INSTALL_DIR/aks-flex-node"
ln -s "$replacement_binary" "$INSTALL_DIR/aks-flex-node"
install_binary "$running_binary" >/dev/null 2>&1 || fail "reinstall over another link failed"
if [[ -L "$INSTALL_DIR/aks-flex-node" ]] || ! cmp -s "$INSTALL_DIR/aks-flex-node" "$running_binary"; then
    fail "reinstall did not replace another link with the binary"
fi
[[ -x "$replacement_binary" ]] || fail "reinstall changed what another link led to"
assert_no_staged_files

# While an agent is installed, any link is refused, without advising root to run it, as it may not
# be the agent's.
rm "$INSTALL_DIR/aks-flex-node"
ln -s "$replacement_binary" "$INSTALL_DIR/aks-flex-node"
touch "$SERVICE_UNIT_PATH"
if install_binary "$running_binary" >"$WORK_DIR/unexpected.log" 2>&1; then
    fail "installer replaced a binary symlink while the agent is installed"
fi
rm "$SERVICE_UNIT_PATH"
! grep -q "$INSTALL_DIR/aks-flex-node reset" "$WORK_DIR/unexpected.log" || fail "symlink guidance advised running the link"
[[ "$(readlink "$INSTALL_DIR/aks-flex-node")" == "$replacement_binary" ]] || fail "a refused symlink changed"
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
[[ "$(run_replacement replacement)" == "new:replacement" ]] || fail "failed staging replaced the installed binary"
assert_no_staged_files

# Installer and managed activation paths must serialize on the same lock.
(
    exec 8>>"$AGENT_UPGRADE_LOCK_PATH"
    flock -n 8
    if install_binary "$replacement_binary" >/dev/null 2>&1; then
        fail "installer ignored the activation lock"
    fi
)

# The install directories come from the host, never from running the binary. Each case points the
# roots at the work dir.
use_roots() {
    HOST_ROOT="$WORK_DIR/$1/opt/unbounded/agent"
    LEGACY_ROOT="$WORK_DIR/$1/usr/local"
    mkdir -p "$LEGACY_ROOT"
}

assert_install_root() {
    local want="$1" case="$2"
    [[ "$INSTALL_DIR" == "$want/bin" ]] || fail "$case: installs under $INSTALL_DIR, want $want/bin"
    [[ -z "$(find "$LEGACY_ROOT" -name '.aks-flex-node.*' -print -quit 2>/dev/null)" ]] ||
        fail "$case: the writability probe was left behind"
}

# A writable /usr/local/bin is used as earlier releases used it, so one of them finds itself there,
# and is created when missing, as install_binary would.
use_roots fresh
resolve_install_dir
assert_install_root "$LEGACY_ROOT" "a fresh host"
[[ -d "$LEGACY_ROOT/bin" ]] || fail "a missing /usr/local/bin was not created"

# The host root's parent is the host's, and may hold files it staged, such as offline artifacts.
# On its own it is not an installation.
use_roots staged
mkdir -p "$(dirname "$HOST_ROOT")/artifacts"
resolve_install_dir
assert_install_root "$LEGACY_ROOT" "a fresh host with files staged beside the host root"

# A host installed under the host root keeps it: reset leaves the layout there.
use_roots installed
mkdir -p "$HOST_ROOT"
resolve_install_dir
assert_install_root "$HOST_ROOT" "a host installed under the host root"

# A host an earlier release installed has the host root linked to /usr/local, where its files are.
use_roots linked
mkdir -p "$(dirname "$HOST_ROOT")"
ln -s "$LEGACY_ROOT" "$HOST_ROOT"
resolve_install_dir
assert_install_root "$LEGACY_ROOT" "a linked host"

# Where /usr/local is read-only, as on Azure Container Linux, the binary goes under the host root,
# which does not exist before the first install, nor does its parent.
use_roots readonly
mount -t tmpfs -o ro tmpfs "$LEGACY_ROOT" || fail "could not mount a read-only /usr/local"
resolve_install_dir
umount "$LEGACY_ROOT"
assert_install_root "$HOST_ROOT" "a read-only /usr/local"
install_binary "$replacement_binary" >/dev/null || fail "install into a missing host root failed"
[[ -x "$INSTALL_DIR/aks-flex-node" ]] || fail "binary missing under the new host root"
for dir in "$(dirname "$HOST_ROOT")" "$HOST_ROOT" "$INSTALL_DIR"; do
    [[ "$(stat -c %a "$dir")" == "755" ]] || fail "new directory $dir mode is $(stat -c %a "$dir"), want 755"
done

# An existing parent keeps its mode and its files.
use_roots readonly-staged
mkdir -p "$(dirname "$HOST_ROOT")/artifacts"
chmod 0750 "$(dirname "$HOST_ROOT")"
mount -t tmpfs -o ro tmpfs "$LEGACY_ROOT" || fail "could not mount a read-only /usr/local"
resolve_install_dir
umount "$LEGACY_ROOT"
assert_install_root "$HOST_ROOT" "a read-only /usr/local beside staged files"
install_binary "$replacement_binary" >/dev/null || fail "install beside staged files failed"
[[ "$(stat -c %a "$(dirname "$HOST_ROOT")")" == "750" ]] || fail "the mode of the host root's parent was changed"
[[ -d "$(dirname "$HOST_ROOT")/artifacts" ]] || fail "files staged beside the host root were removed"

printf 'install_test: ok\n'
