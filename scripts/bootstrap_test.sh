#!/bin/bash

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    exec sudo -E bash "$0" "$@"
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/bootstrap.sh"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

fail() {
    printf 'bootstrap_test: %s\n' "$*" >&2
    exit 1
}

command -v jq >/dev/null || fail "jq is required"
bash -n "$SCRIPT"

case "$(uname -m)" in
    x86_64) ARCH=amd64 ;;
    aarch64) ARCH=arm64 ;;
    *) fail "unsupported test architecture" ;;
esac

make_agent_archive() {
    local dir="$1"
    mkdir -p "$dir"
    cat > "$dir/aks-flex-node-linux-$ARCH" <<'AGENT'
#!/bin/bash
printf '%s\n' "$*" >> "${BOOTSTRAP_TEST_CALLS:?}"
AGENT
    chmod 0755 "$dir/aks-flex-node-linux-$ARCH"
    tar -C "$dir" -czf "$dir/agent.tar.gz" "aks-flex-node-linux-$ARCH"
}

make_agent_archive "$WORK_DIR/agent"
AGENT_URL="file://$WORK_DIR/agent/agent.tar.gz"
AGENT_SHA256=$(sha256sum "$WORK_DIR/agent/agent.tar.gz" | awk '{print $1}')

cat > "$WORK_DIR/base.json" <<'JSON'
{
  "azure": {
    "tenantId": "base-tenant",
    "servicePrincipal": {
      "tenantId": "old",
      "clientId": "old",
      "clientSecret": "old"
    },
    "arc": {"enabled": true}
  },
  "agent": {},
  "node": {"labels": {"base": "true"}}
}
JSON
chmod 0600 "$WORK_DIR/base.json"

BOOTSTRAP_TEST_CALLS="$WORK_DIR/msi-calls" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_AUTH=service-principal \
AKS_FLEX_NODE_SP_CLIENT_ID=environment-client \
AKS_FLEX_NODE_SP_CLIENT_SECRET=environment-secret \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
AKS_FLEX_NODE_AGENT_SHA256="$AGENT_SHA256" \
AKS_FLEX_NODE_CONFIG_OVERRIDES='{"node":{"labels":{"environment":"true"}}}' \
    bash "$SCRIPT" \
        --auth msi \
        --msi-client-id cli-msi \
        --config-overrides '{"node":{"labels":{"cli":"true"}}}' \
        --install-dir "$WORK_DIR/msi-bin" \
        --config-path "$WORK_DIR/msi-etc/config.json" >/dev/null

jq -e '
  .azure.managedIdentity.clientId == "cli-msi" and
  (.azure | has("servicePrincipal") | not) and
  .azure.arc.enabled == false and
  .node.labels == {"base":"true", "environment":"true", "cli":"true"} and
  (.agent.nodeName | length > 0)
' "$WORK_DIR/msi-etc/config.json" >/dev/null
[[ $(stat -c '%a' "$WORK_DIR/msi-etc/config.json") == 600 ]] || fail "MSI config mode is not 0600"
grep -Fx "preflight --config $WORK_DIR/msi-etc/config.json --output text" "$WORK_DIR/msi-calls" >/dev/null
grep -Fx "start --config $WORK_DIR/msi-etc/config.json" "$WORK_DIR/msi-calls" >/dev/null

printf 's"e\\cret\n' > "$WORK_DIR/client-secret"
chmod 0600 "$WORK_DIR/client-secret"
BOOTSTRAP_TEST_CALLS="$WORK_DIR/sp-calls" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    bash "$SCRIPT" \
        --auth service-principal \
        --sp-client-id cli-client \
        --sp-client-secret-file "$WORK_DIR/client-secret" \
        --agent-url "$AGENT_URL" \
        --agent-sha256 "$AGENT_SHA256" \
        --install-dir "$WORK_DIR/sp-bin" \
        --config-path "$WORK_DIR/sp-etc/config.json" >/dev/null

jq -e '
  .azure.servicePrincipal == {
    "tenantId":"base-tenant",
    "clientId":"cli-client",
    "clientSecret":"s\"e\\cret"
  } and
  (.azure | has("managedIdentity") | not) and
  .azure.arc.enabled == false
' "$WORK_DIR/sp-etc/config.json" >/dev/null

echo "bootstrap script tests passed"
