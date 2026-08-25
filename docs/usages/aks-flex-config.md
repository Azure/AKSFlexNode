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

This applies only the CSR creation and approval `ClusterRoleBinding` objects for the `system:bootstrappers:aks-flex-node` group. If any binding still grants that group the obsolete `system:node` role, the command stops after applying the safe bindings and explains how to migrate. It does not silently remove the binding because older and development-mode agents may still use their bootstrap token after joining.

`v0.1.1` introduced a separate daemon client certificate, but the version alone does not prove that certificate was issued successfully. Upgrade every bootstrap-token agent to `v0.1.1` or later (preferably the latest release), and on every host verify that the certificate exists, is unexpired, and the agent remains healthy after a restart:

```bash
sudo test -s /etc/aks-flex-node/daemon-credentials/daemon-controller-current.pem
sudo openssl x509 \
  -in /etc/aks-flex-node/daemon-credentials/daemon-controller-current.pem \
  -noout -subject -enddate -checkend 0
sudo systemctl restart aks-flex-node-agent.service
sudo systemctl is-active aks-flex-node-agent.service
```

Then explicitly remove the obsolete binding:

```bash
./aks-flex-config setup-node-rbac \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --remove-legacy-node-role-binding
```

This migration is idempotent. It automatically deletes only the canonical `aks-flex-node-role` object created by older helpers. If another binding grants the same unsafe edge, or that object has extra subjects, the helper refuses to guess and identifies the objects for manual review. Bootstrap-token config generation refuses to create a token while any such binding exists, rather than either issuing an over-privileged token or unexpectedly breaking an old daemon.

To verify no binding still grants the bootstrap group `system:node`, run:

```bash
kubectl get clusterrolebinding -o json | jq -r '
  .items[]
  | select(.roleRef.kind == "ClusterRole" and .roleRef.name == "system:node")
  | .metadata.name as $binding
  | .subjects[]?
  | select(.kind == "Group" and .name == "system:bootstrappers:aks-flex-node")
  | $binding'
```

The expected result is no output. The canonical `aks-flex-node-role` object is
deleted; a safe, repurposed object with that name is preserved. Once certificate
issuance has been verified, both the kubelet and long-running Flex daemon use
issued client certificates, so removing the unsafe binding does not interrupt
joined nodes. New and in-progress joins retain the CSR permissions installed
above.

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
