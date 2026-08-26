# AKS Flex Config Helper

`scripts/aks-flex-config` is a workstation-side helper for generating AKS Flex Node config files from AKS cluster metadata.

The helper does not install anything on the target host. It uses Azure CLI and, for bootstrap-token mode, `kubectl` to prepare cluster-side bootstrap material and render a config that can be copied to the host.

## Prerequisites

- Azure CLI authenticated to the subscription that contains the AKS cluster.
- `python3` on the workstation.
- `kubectl` on the workstation for `setup-node-rbac` and `--bootstrap-token` config generation.
- Permission to run `az aks get-credentials --admin`, create Kubernetes `ClusterRoleBinding` and bootstrap token `Secret` objects, and remove the obsolete `aks-flex-node-role` binding when present.

## Save The Helper

```bash
curl -fsSLo ./aks-flex-config https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/aks-flex-config
chmod +x ./aks-flex-config
```

## Shared Cluster Arguments

Most commands use the same AKS cluster selectors:

```bash
RESOURCE_GROUP="<resource-group>"
CLUSTER_NAME="<cluster-name>"
SUBSCRIPTION_ID="<subscription-id>"
AGENT_POOL_NAME="${AGENT_POOL_NAME:-aksflexnodes}"
```

| Flag | Required | Description |
|------|----------|-------------|
| `--resource-group` | yes | Resource group that contains the AKS cluster. |
| `--cluster-name` | yes | AKS cluster name. |
| `--subscription` | no | Azure subscription ID or name. Defaults to the current Azure CLI account subscription. |

## Setup Node RBAC

Run this once per cluster for bootstrap-token joins:

```bash
./aks-flex-config setup-node-rbac \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID"
```

This applies only the CSR creation and approval `ClusterRoleBinding` objects for
the `system:bootstrappers:aks-flex-node` group. Kubernetes automatically places
every bootstrap token in `system:bootstrappers`; the token's
`auth-extra-groups` value adds the Flex-specific group. If any binding still
grants either group the obsolete `system:node` role, the command stops after
applying the safe bindings and explains how to migrate. It does not silently
remove the binding because older and development-mode agents may still use
their bootstrap token after joining.

`v0.1.1` introduced a separate daemon client certificate. Before removing the
legacy binding, upgrade every bootstrap-token agent to `v0.1.1` or later
(preferably the latest release). Then run the following on every host. Set
`EXPECTED_VERSION` to the exact release you deployed; the check fails if the
live process is not that binary, restarts during the stability window, has the
wrong certificate identity, or cannot read its exact Node with that certificate.
The host needs `curl`, `jq`, and `openssl`.

Run this as root so the protected config and private key never need broader
permissions. The command extracts only the cluster CA, not the bootstrap token,
into a root-only temporary directory. It removes the file immediately after the
probe, and also on any earlier exit:

```bash
sudo bash <<'EOF'
set -eu
umask 077

EXPECTED_VERSION="v0.1.1" # Change to the exact v0.1.1-or-later release deployed.
SERVICE="aks-flex-node-agent.service"
CONFIG="/etc/aks-flex-node/config.json"
CERT="/etc/aks-flex-node/daemon-credentials/daemon-controller-current.pem"
CURRENT_LINK="/usr/local/lib/aks-flex-node/aks-flex-node-current"
DIRECT_BINARY="/usr/local/bin/aks-flex-node"

systemctl restart "$SERVICE"
systemctl is-active --quiet "$SERVICE"

PID_BEFORE="$(systemctl show --property MainPID --value "$SERVICE")"
test "$PID_BEFORE" -gt 0
LIVE_EXE="$(readlink -f "/proc/$PID_BEFORE/exe")"
if [ -e "$CURRENT_LINK" ]; then
  INSTALLED_EXE="$(readlink -f "$CURRENT_LINK")"
else
  INSTALLED_EXE="$(readlink -f "$DIRECT_BINARY")"
fi
test "$LIVE_EXE" = "$INSTALLED_EXE"

LIVE_VERSION="$("/proc/$PID_BEFORE/exe" version | awk -F ': ' '$1 == "Version" {print $2; exit}')"
printf 'live binary: %s\nlive version: %s\n' "$LIVE_EXE" "$LIVE_VERSION"
test "$LIVE_VERSION" = "$EXPECTED_VERSION"

NODE_NAME="$(jq -er '(.agent.nodeName // "") | gsub("^\\s+|\\s+$"; "")' "$CONFIG")"
if [ -z "$NODE_NAME" ]; then
  NODE_NAME="$(hostname | tr '[:upper:]' '[:lower:]')"
fi

test -s "$CERT"
openssl x509 -in "$CERT" -noout -enddate -checkend 0
SUBJECT="$(openssl x509 -in "$CERT" -noout -subject -nameopt RFC2253)"
SUBJECT="${SUBJECT#subject=}"
# RFC2253 uses ',' between RDNs and '+' inside Go's multi-valued O RDN.
ACTUAL_ATTRIBUTES="$(printf '%s\n' "$SUBJECT" | tr ',+' '\n' | LC_ALL=C sort)"
EXPECTED_ATTRIBUTES="$(printf '%s\n' \
  "CN=system:node:$NODE_NAME" \
  'O=system:nodes' \
  'O=aks-flex-node-daemons' | LC_ALL=C sort)"
printf 'daemon certificate subject: %s\n' "$SUBJECT"
test "$ACTUAL_ATTRIBUTES" = "$EXPECTED_ATTRIBUTES"

API_SERVER="$(jq -er '
  (.node.kubelet.clusterFQDN // .node.kubelet.serverURL)
  | strings
  | gsub("^\\s+|\\s+$"; "")
  | select(length > 0)
' "$CONFIG")"
case "$API_SERVER" in
  https://*) ;;
  *://*) printf 'unsupported API server URL: %s\n' "$API_SERVER" >&2; exit 1 ;;
  *:*) API_SERVER="https://$API_SERVER" ;;
  *) API_SERVER="https://$API_SERVER:443" ;;
esac

CHECK_DIR="$(mktemp -d /run/aks-flex-node-rbac-check.XXXXXX)"
CA_FILE="$CHECK_DIR/cluster-ca.pem"
cleanup() {
  rm -f -- "$CA_FILE"
  rmdir -- "$CHECK_DIR"
}
trap cleanup EXIT
jq -er '.node.kubelet.caCertData | strings | select(length > 0)' "$CONFIG" \
  | base64 --decode >"$CA_FILE"
chmod 0600 "$CA_FILE"
test -s "$CA_FILE"

HTTP_CODE="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --connect-timeout 10 --max-time 30 \
  --cacert "$CA_FILE" --cert "$CERT" --key "$CERT" \
  "${API_SERVER%/}/api/v1/nodes/$NODE_NAME")"
if [ "$HTTP_CODE" != "200" ]; then
  printf 'daemon certificate Node GET returned HTTP %s, expected 200\n' "$HTTP_CODE" >&2
  exit 1
fi
cleanup
trap - EXIT

sleep 30
systemctl is-active --quiet "$SERVICE"
PID_AFTER="$(systemctl show --property MainPID --value "$SERVICE")"
test "$PID_AFTER" = "$PID_BEFORE"
test "$(readlink -f "/proc/$PID_AFTER/exe")" = "$LIVE_EXE"
printf 'verified stable certificate-backed access for Node %s\n' "$NODE_NAME"
EOF
```

An HTTP `200` proves baseline certificate authentication and authorization to
read that daemon's own Node. It does not prove feature-specific authorization
for `MachineOperation` resources or an in-cluster machine client's Kubernetes
service-proxy endpoint. If those features are enabled, verify their
`aks-flex-node-daemons` group RBAC and exercise those paths separately before
migration.

Then explicitly remove the obsolete binding:

```bash
./aks-flex-config setup-node-rbac \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --remove-legacy-node-role-binding
```

This migration is idempotent. It automatically deletes only the plain,
canonical `ClusterRoleBinding/aks-flex-node-role` created by older helpers. It
refuses automatic deletion if that object has extra subjects, ownership or
lifecycle metadata, or custom labels or annotations. Any other unsafe
`ClusterRoleBinding` is reported for manual review. A namespaced `RoleBinding`
can also reference the `system:node` ClusterRole; the helper reports these but
never deletes them automatically. Inspect its owners and other subjects, then
remove only the unsafe bootstrap-token-group edge through the owning deployment
or a careful manual edit. Bootstrap-token config generation refuses to create a
token while any direct unsafe binding remains.

To verify neither bootstrap-token group is still bound to `system:node`, run:

```bash
kubectl get clusterrolebindings,rolebindings --all-namespaces -o json | jq -r '
  .items[]
  | select(
      .roleRef.apiGroup == "rbac.authorization.k8s.io"
      and .roleRef.kind == "ClusterRole"
      and .roleRef.name == "system:node"
    )
  | select([
      .subjects[]?
      | select(
          .apiGroup == "rbac.authorization.k8s.io"
          and .kind == "Group"
          and (
            .name == "system:bootstrappers"
            or .name == "system:bootstrappers:aks-flex-node"
          )
        )
    ] | length > 0)
  | if .kind == "RoleBinding" then
      "RoleBinding/\(.metadata.namespace)/\(.metadata.name)"
    else
      "ClusterRoleBinding/\(.metadata.name)"
    end'
```

The expected result is no output. This is intentionally a narrow audit for a
direct bootstrap-token-group-to-`system:node` binding. It is not a full
effective authorization review and does not analyze aggregated or custom
ClusterRoles or other indirect authorization paths. The canonical
`aks-flex-node-role` object is deleted; a safe, repurposed object with that name
is preserved. Once the checks above pass, both the kubelet and long-running Flex
daemon use issued
client certificates, so removing the unsafe binding does not interrupt joined
nodes. New and in-progress joins retain the CSR permissions installed above.

Do not roll back a migrated host to an older or development-mode agent that still uses the bootstrap token for ordinary Kubernetes API requests. After this binding is removed, those requests correctly receive `403 Forbidden`. Restore a supported certificate-using agent instead of restoring the broad binding.

Finally, delete bootstrap-token Secrets that are no longer needed. In particular, tokens made by helpers before `v0.1.1` had no expiration. Removing the broad binding limits them to bootstrap permissions, but does not revoke them; do not delete a token that is still being used by an in-progress join.

## Generate Node Config

`generate-node-config` fetches AKS metadata and renders a config file. It requires exactly one auth mode.

Use `--output <path>` to write a config file with mode `0600`. If omitted, the config is written to stdout.
The helper writes `azure.resourceManagerEndpoint` with the public ARM endpoint and writes the selected agent pool name to `azure.targetAgentPoolName`. If `--agent-pool-name` is omitted, it defaults to the historical `aksflexnodes` pool name.

| Flag | Required | Description |
|------|----------|-------------|
| `--agent-pool-name` | no | FlexNode agent pool name written into the generated config. Defaults to `aksflexnodes`. |

### Bootstrap Token

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --agent-pool-name "$AGENT_POOL_NAME" \
  --bootstrap-token \
  --output ./aks-flex-node-config.json
```

Bootstrap-token mode creates a Kubernetes bootstrap token `Secret`, reads the AKS API server and CA data from kubeconfig, and includes those values plus the AKS DNS service IP in the generated config.

### Managed Identity

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --agent-pool-name "$AGENT_POOL_NAME" \
  --identity \
  --output ./aks-flex-node-config.json
```

For user-assigned managed identity, pass the client ID with `--username`:

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --agent-pool-name "$AGENT_POOL_NAME" \
  --identity \
  --username "<managed-identity-client-id>" \
  --output ./aks-flex-node-config.json
```

### Service Principal

Service principal flags follow the `az login --service-principal` convention:

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --agent-pool-name "$AGENT_POOL_NAME" \
  --service-principal \
  --username "<client-id>" \
  --password "<client-secret>" \
  --tenant "<tenant-id>" \
  --output ./aks-flex-node-config.json
```

`--tenant` defaults to the current Azure CLI tenant when omitted.

### Azure Arc

Arc bootstrap data must be fetched on the connected host, so this workstation-side helper does not render an Arc runtime config. Use `scripts/bootstrap.sh --auth arc --fetch-bootstrap-data` on the host so its Arc identity obtains fresh kubelet join settings from AKS RP. Passing `--arc` to this helper prints the same guidance and exits without writing a config.

## Copy To Host

After generating the config, copy it to the target host and place it under `/etc/aks-flex-node/config.json` with restrictive permissions:

```bash
TARGET_HOST="<user>@<host>"

scp ./aks-flex-node-config.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

On the target host:

```bash
sudo su
install -d -m 0755 /etc/aks-flex-node
install -m 0600 /tmp/aks-flex-node-config.json /etc/aks-flex-node/config.json
```

Then start AKS Flex Node with a standard `022` umask. The config file stays `0600`, while bootstrap-created nspawn rootfs paths remain traversable by non-root service users such as `dbus`:

```bash
umask 022
aks-flex-node start --config /etc/aks-flex-node/config.json
```
