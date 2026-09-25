#!/bin/bash

set -euo pipefail

if [[ $EUID -ne 0 ]]; then
    exec sudo -E bash "$0" "$@"
fi

# Every case installs a stub agent as root. Run in a private mount namespace so /usr/local and
# /var/lib can be overlaid below: a case that installs an older release then cannot replace a real
# binary on the host, and the staging copy of the agent is not left on the host either.
if [[ -z "${BOOTSTRAP_TEST_ISOLATED:-}" ]]; then
    command -v unshare >/dev/null || { echo "bootstrap_test: unshare is required" >&2; exit 1; }
    exec env BOOTSTRAP_TEST_ISOLATED=1 unshare --mount --propagation private bash "$0" "$@"
fi

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SCRIPT="$REPO_ROOT/scripts/bootstrap.sh"
WORK_DIR=$(mktemp -d)
SERVER_PID=""
cleanup() {
    if [[ -n "$SERVER_PID" ]]; then
        kill "$SERVER_PID" 2>/dev/null || true
    fi
    umount "$WORK_DIR/noexec-tmp" 2>/dev/null || true
    umount /var/lib 2>/dev/null || true
    # Twice: a failed legacy case leaves the read-only bind over the overlay.
    umount /usr/local 2>/dev/null || true
    umount /usr/local 2>/dev/null || true
    rm -rf "$WORK_DIR"
}
trap cleanup EXIT

fail() {
    printf 'bootstrap_test: %s\n' "$*" >&2
    exit 1
}

# Relative paths, such as a rejected relative host root, resolve inside the work dir rather than
# wherever the test was started.
cd "$WORK_DIR"

# Writes to /usr/local land in the overlay's upper dir, which is checked at the end.
USR_LOCAL_WRITES="$WORK_DIR/usr-local/upper"
mkdir -p "$USR_LOCAL_WRITES" "$WORK_DIR/usr-local/work"
mount -t overlay overlay \
    -o "lowerdir=/usr/local,upperdir=$USR_LOCAL_WRITES,workdir=$WORK_DIR/usr-local/work" /usr/local ||
    fail "could not overlay /usr/local"

# bootstrap.sh stages the agent under /var/lib/aks-flex-node to ask it for its host root.
VAR_LIB_WRITES="$WORK_DIR/var-lib/upper"
mkdir -p "$VAR_LIB_WRITES" "$WORK_DIR/var-lib/work"
mount -t overlay overlay \
    -o "lowerdir=/var/lib,upperdir=$VAR_LIB_WRITES,workdir=$WORK_DIR/var-lib/work" /var/lib ||
    fail "could not overlay /var/lib"

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
# host-root is answered before the calls are recorded: bootstrap.sh asks it from a staging copy,
# and the cases below check that every recorded call ran the installed binary.
if [[ "${1:-}" == host-root ]]; then
    printf '%s\n' "$0" >> "${BOOTSTRAP_TEST_CALLS:?}.host-root"
    if [[ -n "${BOOTSTRAP_TEST_LEGACY:-}" ]]; then
        printf 'Error: unknown command "host-root" for "aks-flex-node"\n' >&2
        exit 1
    fi
    printf '%s\n' "${BOOTSTRAP_TEST_HOST_ROOT:?}"
    exit 0
fi
printf '%s\n' "$*" >> "${BOOTSTRAP_TEST_CALLS:?}"
printf '%s\n' "$0" >> "${BOOTSTRAP_TEST_CALLS}.path"
if [[ "${1:-}" == fetch-bootstrap-data ]]; then
    output=""
    while (($# > 0)); do
        if [[ "$1" == --output ]]; then output="$2"; break; fi
        shift
    done
    [[ -n "$output" ]] || exit 24
    if [[ -n "${BOOTSTRAP_TEST_FETCH_RESPONSE:-}" ]]; then
        printf '%s\n' "$BOOTSTRAP_TEST_FETCH_RESPONSE" > "$output"
    else
        cat > "$output" <<'JSON'
{"azure":{"bootstrapToken":{"token":"fresh1.0123456789abcdef"}},"components":{"kubernetes":"1.35.6"},"networking":{"dnsServiceIP":"10.0.0.10","cniVersion":"stale-cni"}}
JSON
    fi
    chmod 0600 "$output"
    exit 0
fi
for variable in AKS_FLEX_NODE_AGENT_URL AKS_FLEX_NODE_SP_CLIENT_SECRET AKS_FLEX_NODE_CONFIG_OVERRIDES AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA AKS_FLEX_NODE_AUTHORITY_HOST AKS_FLEX_NODE_IMDS_ENDPOINT AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS AKS_FLEX_NODE_CLUSTER_RESOURCE_ID AKS_FLEX_NODE_AGENT_POOL_NAME AKS_FLEX_NODE_RESOURCE_MANAGER_ENDPOINT AKS_FLEX_NODE_BOOTSTRAP_OCI_IMAGE AKS_FLEX_NODE_BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE AKS_FLEX_NODE_SP_CLIENT_CERTIFICATE_FILE; do
    [[ -z "${!variable+x}" ]] || exit 23
done
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
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/msi" \
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

BOOTSTRAP_TEST_CALLS="$WORK_DIR/arc-calls" \
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/arc" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --auth arc \
        --fetch-bootstrap-data \
        --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
        --agent-pool-name aksflexnodes \
        --config-path "$WORK_DIR/arc-etc/config.json" >/dev/null

jq -e '
  .azure.arc.enabled == true and
  (.azure | has("managedIdentity") | not) and
  (.azure | has("servicePrincipal") | not) and
  .azure.bootstrapToken.token == "fresh1.0123456789abcdef"
' "$WORK_DIR/arc-etc/config.json" >/dev/null
grep -E '^fetch-bootstrap-data .*--auth arc( |$)' "$WORK_DIR/arc-calls" >/dev/null
# Every command, including the bootstrap-data fetch, runs the installed binary. Running it from
# the temp dir would fail on hosts that mount /tmp noexec.
[[ "$(sort -u "$WORK_DIR/arc-calls.path")" == "$WORK_DIR/arc/bin/aks-flex-node" ]] ||
    fail "agent ran from $(sort -u "$WORK_DIR/arc-calls.path" | tr '\n' ' '), want the installed binary"

printf 's"e\\cret\n' > "$WORK_DIR/client-secret"
chmod 0600 "$WORK_DIR/client-secret"
BOOTSTRAP_TEST_CALLS="$WORK_DIR/sp-calls" \
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/sp" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    bash "$SCRIPT" \
        --auth service-principal \
        --sp-client-id cli-client \
        --sp-client-secret-file "$WORK_DIR/client-secret" \
        --agent-url "$AGENT_URL" \
        --agent-sha256 "$AGENT_SHA256" \
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
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/sp-inline" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_SP_CLIENT_SECRET='inline-secret' \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --auth service-principal \
        --sp-client-id inline-client \
        --config-path "$WORK_DIR/sp-inline-etc/config.json" >/dev/null
jq -e '
  .azure.servicePrincipal.clientId == "inline-client" and
  .azure.servicePrincipal.clientSecret == "inline-secret" and
  (.azure.servicePrincipal | has("clientSecretFile") | not)
' "$WORK_DIR/sp-inline-etc/config.json" >/dev/null

ln -s "$WORK_DIR/client-secret" "$WORK_DIR/client-secret-link"
if BOOTSTRAP_TEST_CALLS="$WORK_DIR/sp-link-calls" \
    BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/sp-link" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth service-principal \
            --sp-client-id link-client \
            --sp-client-secret-file "$WORK_DIR/client-secret-link" \
            --config-path "$WORK_DIR/sp-link-etc/config.json" \
            >"$WORK_DIR/sp-link.log" 2>&1; then
    fail "symlink client-secret file was accepted"
fi
grep -q 'client-secret file must not be a symlink' "$WORK_DIR/sp-link.log" || \
    fail "symlink client-secret rejection was not reported"

command -v python3 >/dev/null || fail "python3 is required by the bootstrap-data test"
cat > "$WORK_DIR/bootstrap-data-server.py" <<'PY'
import base64
import json
import sys
import urllib.parse
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
            parameters = urllib.parse.parse_qs(body)
            secret_auth = parameters.get("client_id") == ["base-sp-client"] and parameters.get("client_secret") == ["base-sp-secret"]
            certificate_auth = False
            if parameters.get("client_id") == ["cert-client"] and "client_assertion" in parameters:
                assertion = parameters["client_assertion"][0]
                parts = assertion.split(".")
                if len(parts) == 3:
                    decode = lambda value: json.loads(base64.urlsafe_b64decode(value + "=" * (-len(value) % 4)))
                    header, payload = decode(parts[0]), decode(parts[1])
                    certificate_auth = (
                        header.get("alg") == "RS256" and
                        bool(header.get("x5t")) and
                        isinstance(header.get("x5c"), list) and
                        len(header["x5c"]) == 2 and
                        all(base64.b64decode(certificate) for certificate in header["x5c"]) and
                        payload.get("iss") == "cert-client" and
                        payload.get("sub") == "cert-client" and
                        payload.get("aud", "").endswith("/base-tenant/oauth2/v2.0/token") and
                        parameters.get("client_assertion_type") == ["urn:ietf:params:oauth:client-assertion-type:jwt-bearer"]
                    )
            if not secret_auth and not certificate_auth:
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
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/fetch" \
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
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/fetch-empty-base" \
AKS_FLEX_NODE_IMDS_ENDPOINT="http://127.0.0.1:${port}/metadata/identity/oauth2/token" \
AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
        --fetch-bootstrap-data \
        --auth msi \
        --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
        --agent-pool-name aksflexnodes \
        --resource-manager-endpoint "http://127.0.0.1:${port}" \
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
    BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/no-base" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth msi \
            --config-path "$WORK_DIR/no-base-etc/config.json" \
            >"$WORK_DIR/no-base.log" 2>&1; then
    fail "unpopulated embedded config was accepted without bootstrap-data fetch"
fi
grep -q 'embedded base config is not populated' "$WORK_DIR/no-base.log" || \
    fail "missing base config rejection was not reported"

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
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/fetch-sp" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/fetch-sp-base.json" \
AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA=true \
AKS_FLEX_NODE_AUTHORITY_HOST="http://127.0.0.1:${port}" \
AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" \
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

command -v openssl >/dev/null || fail "openssl is required by the certificate bootstrap tests"
# Deliberately omit a filename extension: PEM credentials are detected by
    # content, while only binary PKCS#12 credentials require a .pfx suffix.
    openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=bootstrap-test-ca' \
        -keyout "$WORK_DIR/ca-key.pem" -out "$WORK_DIR/ca-public.pem" >/dev/null 2>&1
    openssl req -newkey rsa:2048 -nodes -subj '/CN=bootstrap-test-leaf' \
        -keyout "$WORK_DIR/cert-key.pem" -out "$WORK_DIR/cert.csr" >/dev/null 2>&1
    openssl x509 -req -days 1 -in "$WORK_DIR/cert.csr" \
        -CA "$WORK_DIR/ca-public.pem" -CAkey "$WORK_DIR/ca-key.pem" -CAcreateserial \
        -out "$WORK_DIR/cert-public.pem" >/dev/null 2>&1
    cat "$WORK_DIR/cert-public.pem" "$WORK_DIR/ca-public.pem" "$WORK_DIR/cert-key.pem" > "$WORK_DIR/client-certificate"
    chmod 0600 "$WORK_DIR/client-certificate"

    BOOTSTRAP_TEST_CALLS="$WORK_DIR/fetch-cert-calls" \
    BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/fetch-cert" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/fetch-base.json" \
    AKS_FLEX_NODE_AUTHORITY_HOST="http://127.0.0.1:${port}" \
    AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
    AKS_FLEX_NODE_SP_CLIENT_SECRET='environment-should-be-ignored' \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" \
            --fetch-bootstrap-data \
            --auth service-principal \
            --sp-tenant-id base-tenant \
            --sp-client-id cert-client \
            --sp-client-certificate-file "$WORK_DIR/client-certificate" \
            --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
            --agent-pool-name aksflexnodes \
            --resource-manager-endpoint "http://127.0.0.1:${port}" \
            --config-path "$WORK_DIR/fetch-cert-etc/config.json" >/dev/null

    jq -e --arg certificateFile "$WORK_DIR/client-certificate" '
      .azure.bootstrapToken.token == "fresh1.0123456789abcdef" and
      .azure.servicePrincipal == {
        "tenantId":"base-tenant",
        "clientId":"cert-client",
        "clientSecretFile":$certificateFile
      } and
      .components.kubernetes == "1.35.6"
    ' "$WORK_DIR/fetch-cert-etc/config.json" >/dev/null

    openssl pkcs12 -export -passout pass: -in "$WORK_DIR/cert-public.pem" \
        -inkey "$WORK_DIR/cert-key.pem" -certfile "$WORK_DIR/ca-public.pem" \
        -out "$WORK_DIR/client-certificate.pfx" >/dev/null 2>&1
    chmod 0600 "$WORK_DIR/client-certificate.pfx"
    BOOTSTRAP_TEST_CALLS="$WORK_DIR/fetch-pfx-calls" \
    BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/fetch-pfx" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/fetch-base.json" \
    AKS_FLEX_NODE_AUTHORITY_HOST="http://127.0.0.1:${port}" \
    AKS_FLEX_NODE_ALLOW_INSECURE_TEST_ENDPOINTS=true \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" \
            --fetch-bootstrap-data \
            --auth service-principal \
            --sp-tenant-id base-tenant \
            --sp-client-id cert-client \
            --sp-client-certificate-file "$WORK_DIR/client-certificate.pfx" \
            --cluster-resource-id '/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster' \
            --agent-pool-name aksflexnodes \
            --resource-manager-endpoint "http://127.0.0.1:${port}" \
            --config-path "$WORK_DIR/fetch-pfx-etc/config.json" >/dev/null
    jq -e --arg certificateFile "$WORK_DIR/client-certificate.pfx" '
      .azure.servicePrincipal.clientSecretFile == $certificateFile and
      .azure.bootstrapToken.token == "fresh1.0123456789abcdef"
    ' "$WORK_DIR/fetch-pfx-etc/config.json" >/dev/null

# The binary goes under the host root the agent reports, in directories created 0755 even under the
# script's umask, so systemd and the agent can reach it.
BOOTSTRAP_TEST_CALLS="$WORK_DIR/root-calls" \
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/root/opt/unbounded" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" --auth arc --config-path "$WORK_DIR/root-etc/config.json" >/dev/null
[[ -x "$WORK_DIR/root/opt/unbounded/bin/aks-flex-node" ]] || fail "binary was not installed under the host root"
for dir in opt opt/unbounded opt/unbounded/bin; do
    [[ $(stat -c '%a' "$WORK_DIR/root/$dir") == 755 ]] || fail "new directory $dir is not 0755"
done
[[ "$(sort -u "$WORK_DIR/root-calls.path")" == "$WORK_DIR/root/opt/unbounded/bin/aks-flex-node" ]] ||
    fail "agent ran from $(sort -u "$WORK_DIR/root-calls.path" | tr '\n' ' '), want the installed binary"

# The host root is asked of a copy under /var/lib, not of the download in the temp dir: on a host
# that mounts /tmp noexec that could not run, and would be taken for an older release.
[[ "$(cat "$WORK_DIR/root-calls.host-root")" == /var/lib/aks-flex-node/.bootstrap.*/aks-flex-node ]] ||
    fail "host-root ran from $(cat "$WORK_DIR/root-calls.host-root"), want the staging copy"
mkdir -p "$WORK_DIR/noexec-tmp"
mount -t tmpfs -o noexec,mode=0700 tmpfs "$WORK_DIR/noexec-tmp" || fail "could not mount a noexec temp dir"
TMPDIR="$WORK_DIR/noexec-tmp" \
BOOTSTRAP_TEST_CALLS="$WORK_DIR/noexec-calls" \
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/noexec" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" --auth arc --config-path "$WORK_DIR/noexec-etc/config.json" >/dev/null ||
    fail "bootstrap failed with a noexec temp dir"
[[ -x "$WORK_DIR/noexec/bin/aks-flex-node" ]] || fail "a noexec temp dir sent the binary elsewhere"
umount "$WORK_DIR/noexec-tmp"

# The staging copy does not outlive the run.
if compgen -G "$VAR_LIB_WRITES/aks-flex-node/.bootstrap.*" >/dev/null; then
    fail "staging copies were left in /var/lib/aks-flex-node"
fi

# --install-dir is deprecated. It is accepted only when it matches the directory the agent reports,
# since the agent cannot find a binary installed anywhere else.
BOOTSTRAP_TEST_CALLS="$WORK_DIR/matching-calls" \
BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/matching" \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" --auth arc \
        --install-dir "$WORK_DIR/matching/bin/" \
        --config-path "$WORK_DIR/matching-etc/config.json" >"$WORK_DIR/matching.log" 2>&1 ||
    fail "a matching --install-dir was rejected"
grep -q "deprecated" "$WORK_DIR/matching.log" || fail "--install-dir did not warn that it is deprecated"

if BOOTSTRAP_TEST_CALLS="$WORK_DIR/mismatch-calls" \
    BOOTSTRAP_TEST_HOST_ROOT="$WORK_DIR/mismatch" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth arc \
            --install-dir "$WORK_DIR/elsewhere" \
            --config-path "$WORK_DIR/mismatch-etc/config.json" >"$WORK_DIR/mismatch.log" 2>&1; then
    fail "an --install-dir that disagrees with the agent was accepted"
fi
grep -q "where this agent looks for its binary" "$WORK_DIR/mismatch.log" || fail "mismatch was not explained"
[[ ! -e "$WORK_DIR/elsewhere/aks-flex-node" && ! -e "$WORK_DIR/mismatch/bin/aks-flex-node" ]] ||
    fail "a binary was installed despite the mismatch"
[[ ! -s "$WORK_DIR/mismatch-calls" ]] || fail "the agent was run despite the mismatch"

# A host root that is not an absolute path is refused before anything is installed.
if BOOTSTRAP_TEST_CALLS="$WORK_DIR/relative-calls" \
    BOOTSTRAP_TEST_HOST_ROOT="relative/root" \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth arc --config-path "$WORK_DIR/relative-etc/config.json" >"$WORK_DIR/relative.log" 2>&1; then
    fail "a relative host root was accepted"
fi
grep -q "invalid host root" "$WORK_DIR/relative.log" || fail "relative host root was not reported"
[[ ! -e "$WORK_DIR/relative-calls" && ! -e "$WORK_DIR/relative" ]] || fail "the agent was installed under a relative host root"

# Every case above installs under the work dir, so nothing may have been written to /usr/local.
if [[ -n "$(ls -A "$USR_LOCAL_WRITES")" ]]; then
    fail "a case wrote to /usr/local: $(cd "$USR_LOCAL_WRITES" && find . -mindepth 1 | tr '\n' ' ')"
fi

# A release before the host root has no host-root command and looks for its binary in /usr/local/bin,
# so that is where it goes. These cases write to /usr/local, so they run last and only on the overlay.
[[ "$(findmnt -n -o FSTYPE /usr/local)" == overlay ]] || fail "/usr/local is not overlaid; refusing the legacy cases"

# On a host where /usr/local is read-only, such as Azure Container Linux, an older release cannot be
# installed at all, and the operator is told to use a newer one.
# A read-only bind over the overlay, since overlayfs cannot be remounted read-only.
{ mount --bind /usr/local /usr/local && mount -o remount,bind,ro /usr/local; } || fail "could not make /usr/local read-only"
if BOOTSTRAP_TEST_CALLS="$WORK_DIR/legacy-ro-calls" \
    BOOTSTRAP_TEST_LEGACY=1 \
    AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
    AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
        bash "$SCRIPT" --auth arc --config-path "$WORK_DIR/legacy-ro-etc/config.json" >"$WORK_DIR/legacy-ro.log" 2>&1; then
    fail "an older release was installed on a read-only /usr/local"
fi
umount /usr/local || fail "could not make /usr/local writable again"
grep -q "predates /opt/unbounded" "$WORK_DIR/legacy-ro.log" || fail "a read-only /usr/local was not explained"
[[ ! -s "$WORK_DIR/legacy-ro-calls" ]] || fail "the agent was run despite a failed install"

# /usr/local/bin belongs to the image, so its mode is left as the image set it.
legacy_bin_mode=$(stat -c '%a' /usr/local/bin)
chmod 0750 /usr/local/bin
BOOTSTRAP_TEST_CALLS="$WORK_DIR/legacy-calls" \
BOOTSTRAP_TEST_LEGACY=1 \
AKS_FLEX_NODE_BASE_CONFIG_FILE="$WORK_DIR/base.json" \
AKS_FLEX_NODE_AGENT_URL="$AGENT_URL" \
    bash "$SCRIPT" --auth arc --config-path "$WORK_DIR/legacy-etc/config.json" >/dev/null
grep -q BOOTSTRAP_TEST_CALLS "$USR_LOCAL_WRITES/bin/aks-flex-node" 2>/dev/null ||
    fail "an older release was not installed at /usr/local/bin"
[[ "$(sort -u "$WORK_DIR/legacy-calls.path")" == /usr/local/bin/aks-flex-node ]] ||
    fail "an older release ran from $(sort -u "$WORK_DIR/legacy-calls.path" | tr '\n' ' ')"
[[ $(stat -c '%a' /usr/local/bin) == 750 ]] || fail "the mode of an existing /usr/local/bin was changed"
chmod "$legacy_bin_mode" /usr/local/bin

echo "bootstrap script tests passed"
