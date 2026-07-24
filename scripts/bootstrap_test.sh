#!/bin/bash

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    exec sudo -E bash "$0" "$@"
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/bootstrap.sh"
WORK_DIR=$(mktemp -d)
SERVER_PID=""
cleanup() {
    if [[ -n "$SERVER_PID" ]]; then
        kill "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

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
for variable in AKS_FLEX_NODE_AGENT_URL AKS_FLEX_NODE_SP_CLIENT_SECRET AKS_FLEX_NODE_CONFIG_OVERRIDES AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA AKS_FLEX_NODE_AUTHORITY_HOST AKS_FLEX_NODE_IMDS_ENDPOINT AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS AKS_FLEX_NODE_CLUSTER_RESOURCE_ID AKS_FLEX_NODE_AGENT_POOL_NAME AKS_FLEX_NODE_RESOURCE_MANAGER_ENDPOINT AKS_FLEX_NODE_BOOTSTRAP_OCI_IMAGE AKS_FLEX_NODE_BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE; do
    [[ -z "${!variable+x}" ]] || exit 23
done
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
AKS_FLEX_NODE_BOOTSTRAP_OCI_IMAGE='https://environment.example/rootfs.tar.gz' \
AKS_FLEX_NODE_BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE='https://environment.example/bootstrap-k8s-{{ .KubernetesVersion }}.tar.gz' \
AKS_FLEX_NODE_CONFIG_OVERRIDES='{"node":{"labels":{"environment":"true"}}}' \
    bash "$SCRIPT" \
        --auth msi \
        --msi-client-id cli-msi \
        --bootstrap-oci-image 'https://cli.example/rootfs.tar.gz' \
        --config-overrides '{"node":{"labels":{"cli":"true"}},"bootstrap":{"offlineArtifacts":{"source":"https://generic-cli.example/ignored.tar.gz"}}}' \
        --install-dir "$WORK_DIR/msi-bin" \
        --config-path "$WORK_DIR/msi-etc/config.json" >/dev/null

jq -e '
  .azure.managedIdentity.clientId == "cli-msi" and
  (.azure | has("servicePrincipal") | not) and
  .azure.arc.enabled == false and
  .bootstrap.ociImage == "https://cli.example/rootfs.tar.gz" and
  .bootstrap.offlineArtifacts.source == "https://environment.example/bootstrap-k8s-{{ .KubernetesVersion }}.tar.gz" and
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

jq -e --arg secretFile "$WORK_DIR/client-secret" '
  .azure.servicePrincipal == {
    "tenantId":"base-tenant",
    "clientId":"cli-client",
    "clientSecretFile":$secretFile
  } and
  (.azure | has("managedIdentity") | not) and
  .azure.arc.enabled == false
' "$WORK_DIR/sp-etc/config.json" >/dev/null

BOOTSTRAP_TEST_CALLS="$WORK_DIR/sp-inline-calls" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_SP_CLIENT_SECRET='inline-secret' \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --auth service-principal \
        --sp-client-id inline-client \
        --install-dir "$WORK_DIR/sp-inline-bin" \
        --config-path "$WORK_DIR/sp-inline-etc/config.json" >/dev/null
jq -e '
  .azure.servicePrincipal.clientId == "inline-client" and
  .azure.servicePrincipal.clientSecret == "inline-secret" and
  (.azure.servicePrincipal | has("clientSecretFile") | not)
' "$WORK_DIR/sp-inline-etc/config.json" >/dev/null

ln -s "$WORK_DIR/client-secret" "$WORK_DIR/client-secret-link"
if BOOTSTRAP_TEST_CALLS="$WORK_DIR/sp-link-calls" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth service-principal \
            --sp-client-id link-client \
            --sp-client-secret-file "$WORK_DIR/client-secret-link" \
            --install-dir "$WORK_DIR/sp-link-bin" \
            --config-path "$WORK_DIR/sp-link-etc/config.json" \
            >"$WORK_DIR/sp-link.log" 2>&1; then
    fail "symlink client-secret file was accepted"
fi
grep -q 'client-secret file must not be a symlink' "$WORK_DIR/sp-link.log" || \
    fail "symlink client-secret rejection was not reported"

command -v python3 >/dev/null || fail "python3 is required by the bootstrap-data test"
cat > "$WORK_DIR/bootstrap-data-server.py" <<'PY'
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def write_json(self, value):
        data = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path.startswith("/metadata/identity/oauth2/token?"):
            self.write_json({"access_token": "test-arm-token", "expires_in": "3600"})
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path == "/base-tenant/oauth2/v2.0/token":
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length).decode()
            if "client_id=base-sp-client" not in body or "client_secret=base-sp-secret" not in body:
                self.send_error(401)
                return
            self.write_json({"access_token": "test-arm-token", "expires_in": 3600})
            return
        expected = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster/agentPools/aksflexnodes/listBootstrapData?api-version=2026-05-02-preview"
        if self.path != expected:
            self.send_error(404)
            return
        if self.headers.get("Authorization") != "Bearer test-arm-token":
            self.send_error(401)
            return
        self.write_json({
            "azure": {"bootstrapToken": {"token": "fresh1.0123456789abcdef"}},
            "components": {"kubernetes": "1.35.6"},
            "networking": {"dnsServiceIP": "10.0.0.10", "cniVersion": "stale-cni"},
        })

server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
with open(sys.argv[1], "w") as handle:
    handle.write(str(server.server_address[1]))
server.serve_forever()
PY
python3 "$WORK_DIR/bootstrap-data-server.py" "$WORK_DIR/bootstrap-data-port" &
SERVER_PID=$!
for _ in $(seq 1 50); do
    [[ -s "$WORK_DIR/bootstrap-data-port" ]] && break
    sleep 0.1
done
[[ -s "$WORK_DIR/bootstrap-data-port" ]] || fail "bootstrap-data test server did not start"
port=$(<"$WORK_DIR/bootstrap-data-port")
cat > "$WORK_DIR/fetch-base.json" <<JSON
{
  "components": {"kubernetes": "stale", "containerd": "stale-containerd", "runc": "stale-runc"},
  "bootstrap": {"offlineArtifacts": {"source": "https://offline.example/bundle.tar.gz"}},
  "agent": {}
}
JSON
chmod 0600 "$WORK_DIR/fetch-base.json"

BOOTSTRAP_TEST_CALLS="$WORK_DIR/fetch-calls" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/fetch-base.json" \
AKS_FLEX_NODE_IMDS_ENDPOINT="http://127.0.0.1:${port}/metadata/identity/oauth2/token" \
AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
AKS_FLEX_NODE_CLUSTER_RESOURCE_ID='/subscriptions/wrong/resourceGroups/wrong/providers/Microsoft.ContainerService/managedClusters/wrong' \
AKS_FLEX_NODE_AGENT_POOL_NAME=wrongpool \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --fetch-bootstrap-data \
        --auth msi \
        --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
        --agent-pool-name aksflexnodes \
        --resource-manager-endpoint "http://127.0.0.1:${port}" \
        --config-overrides '{"azure":{"targetCluster":{"resourceId":"/subscriptions/wrong/resourceGroups/wrong/providers/Microsoft.ContainerService/managedClusters/wrong"},"targetAgentPoolName":"wrongpool"},"node":{"labels":{"fresh":"true"}}}' \
        --install-dir "$WORK_DIR/fetch-bin" \
        --config-path "$WORK_DIR/fetch-etc/config.json" >/dev/null

jq -e --arg armEndpoint "http://127.0.0.1:${port}" '
  .azure.targetCluster.resourceId == "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster" and
  .azure.targetAgentPoolName == "aksflexnodes" and
  .azure.resourceManagerEndpoint == $armEndpoint and
  .azure.bootstrapToken.token == "fresh1.0123456789abcdef" and
  .azure.managedIdentity == {} and
  .components.kubernetes == "1.35.6" and
  .networking.dnsServiceIP == "10.0.0.10" and
  (.networking | has("cniVersion") | not) and
  (.components | has("containerd") | not) and
  (.components | has("runc") | not) and
  .node.labels.fresh == "true"
' "$WORK_DIR/fetch-etc/config.json" >/dev/null

# The raw repository script can start from an implicit empty object when fresh
# bootstrap data and the target cluster/pool are supplied.
BOOTSTRAP_TEST_CALLS="$WORK_DIR/fetch-empty-base-calls" \
AKS_FLEX_NODE_IMDS_ENDPOINT="http://127.0.0.1:${port}/metadata/identity/oauth2/token" \
AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --fetch-bootstrap-data \
        --auth msi \
        --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
        --agent-pool-name aksflexnodes \
        --resource-manager-endpoint "http://127.0.0.1:${port}" \
        --install-dir "$WORK_DIR/fetch-empty-base-bin" \
        --config-path "$WORK_DIR/fetch-empty-base-etc/config.json" >/dev/null

jq -e --arg armEndpoint "http://127.0.0.1:${port}" '
  .azure.targetCluster.resourceId == "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster" and
  .azure.targetAgentPoolName == "aksflexnodes" and
  .azure.resourceManagerEndpoint == $armEndpoint and
  .azure.bootstrapToken.token == "fresh1.0123456789abcdef" and
  .azure.managedIdentity == {} and
  .components.kubernetes == "1.35.6" and
  (.agent.nodeName | length > 0)
' "$WORK_DIR/fetch-empty-base-etc/config.json" >/dev/null

if BOOTSTRAP_TEST_CALLS="$WORK_DIR/no-base-calls" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth msi \
            --install-dir "$WORK_DIR/no-base-bin" \
            --config-path "$WORK_DIR/no-base-etc/config.json" \
            >"$WORK_DIR/no-base.log" 2>&1; then
    fail "unpopulated embedded config was accepted without bootstrap-data fetch"
fi
grep -q 'embedded base config is not populated' "$WORK_DIR/no-base.log" || \
    fail "missing base config rejection was not reported"

if BOOTSTRAP_TEST_CALLS="$WORK_DIR/insecure-calls" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/fetch-base.json" \
    AKS_FLEX_NODE_IMDS_ENDPOINT="http://127.0.0.1:${port}/metadata/identity/oauth2/token" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --fetch-bootstrap-data --auth msi \
            --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
            --agent-pool-name aksflexnodes \
            --resource-manager-endpoint "http://127.0.0.1:${port}" \
            --install-dir "$WORK_DIR/insecure-bin" \
            --config-path "$WORK_DIR/insecure-etc/config.json" \
            >"$WORK_DIR/insecure.log" 2>&1; then
    fail "HTTP resource manager endpoint was accepted"
fi
grep -q 'resource manager endpoint must use HTTPS' "$WORK_DIR/insecure.log" || \
    fail "HTTP endpoint rejection was not reported"

printf '%s' 'base-sp-secret' > "$WORK_DIR/fetch-sp-secret"
chmod 0600 "$WORK_DIR/fetch-sp-secret"
cat > "$WORK_DIR/fetch-sp-base.json" <<JSON
{
  "azure": {
    "tenantId": "base-tenant",
    "resourceManagerEndpoint": "http://127.0.0.1:${port}",
    "targetAgentPoolName": "aksflexnodes",
    "servicePrincipal": {
      "tenantId": "base-tenant",
      "clientId": "base-sp-client",
      "clientSecretFile": "$WORK_DIR/fetch-sp-secret"
    },
    "arc": {"enabled": false},
    "targetCluster": {
      "resourceId": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
      "location": "region"
    }
  },
  "components": {"kubernetes": "stale"},
  "agent": {}
}
JSON
chmod 0600 "$WORK_DIR/fetch-sp-base.json"

BOOTSTRAP_TEST_CALLS="$WORK_DIR/fetch-sp-calls" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/fetch-sp-base.json" \
AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA=true \
AKS_FLEX_NODE_AUTHORITY_HOST="http://127.0.0.1:${port}" \
AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --install-dir "$WORK_DIR/fetch-sp-bin" \
        --config-path "$WORK_DIR/fetch-sp-etc/config.json" >/dev/null

jq -e --arg secretFile "$WORK_DIR/fetch-sp-secret" '
  .azure.bootstrapToken.token == "fresh1.0123456789abcdef" and
  .azure.servicePrincipal == {
    "tenantId":"base-tenant",
    "clientId":"base-sp-client",
    "clientSecretFile":$secretFile
  } and
  .components.kubernetes == "1.35.6"
' "$WORK_DIR/fetch-sp-etc/config.json" >/dev/null

echo "bootstrap script tests passed"
