#!/bin/bash

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/install.sh"
WORK_DIR=$(mktemp -d)
RUNNING_PID=""
cleanup() {
    if [[ -n "$RUNNING_PID" ]]; then
        kill "$RUNNING_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

fail() {
    printf 'install_test: %s\n' "$*" >&2
    exit 1
}

command -v go >/dev/null || fail "go is required"
bash -n "$SCRIPT"
source "$SCRIPT"
# install.sh only assigns defaults at source time; override after sourcing to keep this test isolated.
INSTALL_DIR="$WORK_DIR/bin"
export GOCACHE="$WORK_DIR/gocache"
mkdir -p "$GOCACHE"

cat > "$WORK_DIR/running.go" <<'GO'
package main

import (
	"os"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		if err := os.WriteFile(os.Args[1], []byte("ready"), 0600); err != nil {
			os.Exit(1)
		}
	}
	time.Sleep(30 * time.Second)
}
GO

cat > "$WORK_DIR/replacement.go" <<'GO'
package main

import "fmt"

func main() {
	fmt.Println("replacement")
}
GO

GO111MODULE=off go build -o "$WORK_DIR/running" "$WORK_DIR/running.go"
GO111MODULE=off go build -o "$WORK_DIR/replacement" "$WORK_DIR/replacement.go"

mkdir -p "$INSTALL_DIR"
cp "$WORK_DIR/running" "$INSTALL_DIR/aks-flex-node"
chmod 0755 "$INSTALL_DIR/aks-flex-node"

"$INSTALL_DIR/aks-flex-node" "$WORK_DIR/ready" &
RUNNING_PID=$!
for _ in {1..50}; do
    [[ -f "$WORK_DIR/ready" ]] && break
    sleep 0.1
done
[[ -f "$WORK_DIR/ready" ]] || fail "running binary did not signal readiness"

install_binary "$WORK_DIR/replacement" >"$WORK_DIR/install.log" 2>&1 || {
    cat "$WORK_DIR/install.log" >&2
    fail "installer failed while replacing running binary"
}
if [[ $EUID -ne 0 ]]; then
    grep -q "Installing binary as $(id -un); run the installer with sudo to make it root-owned" "$WORK_DIR/install.log" || \
        fail "non-root ownership warning was not reported"
fi

kill -0 "$RUNNING_PID" 2>/dev/null || fail "old running process exited unexpectedly"
[[ "$("$INSTALL_DIR/aks-flex-node")" == "replacement" ]] || fail "installed binary was not replaced"
staged_file=$(find "$INSTALL_DIR" -name '.aks-flex-node.*' -print -quit)
if [[ -n "$staged_file" ]]; then
    fail "staged binary was not cleaned up"
fi

if install_binary "$WORK_DIR/missing" >"$WORK_DIR/missing.log" 2>&1; then
    fail "installer succeeded with missing source binary"
fi
[[ "$("$INSTALL_DIR/aks-flex-node")" == "replacement" ]] || fail "failed install clobbered installed binary"
staged_file=$(find "$INSTALL_DIR" -name '.aks-flex-node.*' -print -quit)
if [[ -n "$staged_file" ]]; then
    fail "staged binary was not cleaned up after failed install"
fi

printf 'install_test: ok\n'
