# AKS Flex Config Helper

`scripts/aks-flex-config` is a workstation-side helper for generating AKS Flex Node config files from AKS cluster metadata.

The helper does not install anything on the target host. It uses Azure CLI and, for bootstrap-token mode, `kubectl` to prepare cluster-side bootstrap material and render a config that can be copied to the host.

## Prerequisites

- Azure CLI authenticated to the subscription that contains the AKS cluster.
- `python3` on the workstation.
- `kubectl` on the workstation for `setup-node-rbac` and `--bootstrap-token` config generation.
- Permission to run `az aks get-credentials --admin` and create Kubernetes `ClusterRoleBinding` and bootstrap token `Secret` objects.

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

This applies the bootstrap-related `ClusterRoleBinding` objects for the `system:bootstrappers:aks-flex-node` group.

## Generate Node Config

`generate-node-config` fetches AKS metadata and renders a config file. It requires exactly one auth mode.

Use `--output <path>` to write a config file with mode `0600`. If omitted, the config is written to stdout.

### Bootstrap Token

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --bootstrap-token \
  --output ./aks-flex-node-config.json
```

Bootstrap-token mode creates a Kubernetes bootstrap token `Secret`, reads the AKS API server and CA data from kubeconfig, and includes those values in the generated config.

### Managed Identity

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --identity \
  --output ./aks-flex-node-config.json
```

For user-assigned managed identity, pass the client ID with `--username`:

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
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
  --service-principal \
  --username "<client-id>" \
  --password "<client-secret>" \
  --tenant "<tenant-id>" \
  --output ./aks-flex-node-config.json
```

`--tenant` defaults to the current Azure CLI tenant when omitted.

### Azure Arc

```bash
./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --arc \
  --arc-machine-name "<arc-machine-name>" \
  --arc-resource-group "<arc-resource-group>" \
  --arc-location "<arc-location>" \
  --output ./aks-flex-node-config.json
```

## Copy To Host

After generating the config, copy it to the target host and place it under `/etc/aks-flex-node/config.json` with restrictive permissions:

```bash
TARGET_HOST="<user>@<host>"

scp ./aks-flex-node-config.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

On the target host:

```bash
sudo su
umask 077
mkdir -p /etc/aks-flex-node
cp /tmp/aks-flex-node-config.json /etc/aks-flex-node/config.json
chmod 600 /etc/aks-flex-node/config.json
```

Then start AKS Flex Node:

```bash
aks-flex-node start --config /etc/aks-flex-node/config.json
```
