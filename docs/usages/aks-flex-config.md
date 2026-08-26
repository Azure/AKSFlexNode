# AKS Flex Config Helper

`aks-flex-config` is a workstation-side helper for generating AKS Flex Node config files from AKS cluster metadata. It is available as a Python script and as native Windows AMD64 and macOS AMD64/ARM64 executables in tagged releases.

The helper does not install anything on the target host. It uses Azure CLI and, for bootstrap-token mode, `kubectl` to prepare cluster-side bootstrap material and render a config that can be copied to the host.

## Prerequisites

- Azure CLI authenticated to the subscription that contains the AKS cluster.
- `kubectl` on the workstation for `setup-node-rbac` and `--bootstrap-token` config generation.
- Permission to run `az aks get-credentials --admin` and create Kubernetes `ClusterRoleBinding` and bootstrap token `Secret` objects.
- Linux, or macOS using the script: `python3` and `curl`.
- macOS using a native release binary: `curl`.
- Windows: PowerShell and the OpenSSH client when copying the generated config with `scp`.

## Save The Helper

<details open>
<summary>Linux or macOS workstation (Python)</summary>

```bash
curl -fsSLo ./aks-flex-config https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/aks-flex-config
chmod +x ./aks-flex-config
```

</details>

<details>
<summary>macOS workstation (native binary)</summary>

Set an explicit tagged release and download the binary for Intel or Apple Silicon:

```bash
AKS_FLEX_NODE_VERSION="<release-tag>"
case "$(uname -m)" in
  x86_64) CONFIG_ARCH="amd64" ;;
  arm64) CONFIG_ARCH="arm64" ;;
  *) echo "Unsupported macOS architecture: $(uname -m)" >&2; exit 1 ;;
esac

curl -fL -o ./aks-flex-config \
  "https://github.com/Azure/AKSFlexNode/releases/download/${AKS_FLEX_NODE_VERSION}/aks-flex-config-darwin-${CONFIG_ARCH}"
chmod +x ./aks-flex-config
./aks-flex-config --help
```

</details>

<details>
<summary>Windows workstation (PowerShell)</summary>

Set an explicit tagged release, then download the Windows AMD64 helper:

```powershell
$AksFlexNodeVersion = "<release-tag>"
$Binary = "aks-flex-config-windows-amd64.exe"
$DownloadUrl = "https://github.com/Azure/AKSFlexNode/releases/download/$AksFlexNodeVersion/$Binary"

Invoke-WebRequest -Uri $DownloadUrl -OutFile $Binary
$AksFlexConfig = ".\$Binary"
& $AksFlexConfig --help
```

The executable runs on the Windows workstation. The Flex Node machine remains a supported Linux host. Existing labs and E2E automation continue to use `scripts/aks-flex-config` during the compatibility period.

</details>

## Shared Cluster Arguments

Most commands use the same AKS cluster selectors:

```bash
RESOURCE_GROUP="<resource-group>"
CLUSTER_NAME="<cluster-name>"
SUBSCRIPTION_ID="<subscription-id>"
AGENT_POOL_NAME="${AGENT_POOL_NAME:-aksflexnodes}"
```

<details>
<summary>Windows workstation (PowerShell)</summary>

```powershell
$ResourceGroup = "<resource-group>"
$ClusterName = "<cluster-name>"
$SubscriptionId = "<subscription-id>"
$AgentPoolName = "aksflexnodes"
```

</details>

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

<details>
<summary>Windows workstation (PowerShell)</summary>

```powershell
& $AksFlexConfig setup-node-rbac `
  --resource-group $ResourceGroup `
  --cluster-name $ClusterName `
  --subscription $SubscriptionId
```

</details>

This applies the bootstrap-related `ClusterRoleBinding` objects for the `system:bootstrappers:aks-flex-node` group.

## Generate Node Config

`generate-node-config` fetches AKS metadata and renders a config file. It requires exactly one auth mode.

Use `--output <path>` to write a config file. On Linux and macOS, the helper sets mode `0600`; on Windows, access is governed by the destination directory's Windows ACL. If omitted, the config is written to stdout.
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

<details>
<summary>Windows workstation (PowerShell)</summary>

```powershell
& $AksFlexConfig generate-node-config `
  --resource-group $ResourceGroup `
  --cluster-name $ClusterName `
  --subscription $SubscriptionId `
  --agent-pool-name $AgentPoolName `
  --bootstrap-token `
  --output .\aks-flex-node-config.json
```

</details>

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

<details>
<summary>Windows workstation (PowerShell)</summary>

```powershell
$TargetHost = "<user>@<host>"
scp .\aks-flex-node-config.json "${TargetHost}:/tmp/aks-flex-node-config.json"
```

</details>

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
