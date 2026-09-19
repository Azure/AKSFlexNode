# Operator Guide: Bootstrap an AKS Flex Node

This guide creates an Azure Kubernetes Service (AKS) cluster without a built-in Container Network Interface (CNI), installs Unbounded-Net, creates a Flex node pool, and attaches a prepared Linux host with [`scripts/bootstrap.sh`](../../scripts/bootstrap.sh).

The walkthrough uses a public AKS API endpoint and private Layer 3 connectivity between the AKS-managed node network and the flex node host network. API server access and node connectivity are separate decisions: a public API endpoint doesn't provide node, pod, service, or control-plane callback connectivity. For other evaluated topologies, see the [labs](../labs/README.md).

> [!IMPORTANT]
> AKS Flex Node is a preview feature intended for evaluation. This workflow creates Azure and Kubernetes resources and changes the target host as root. Review the complete procedure and cleanup requirements before you begin.

Run Azure CLI, `kubectl`, artifact download, and SSH commands in your **Bash environment**. Run host preparation and bootstrap commands on the separate **flex node host** only when a step explicitly directs you to.

The bootstrap script is downloaded and run interactively on the host. This guide doesn't use cloud-init. For the architecture and security rationale, see [Generated bootstrap script](../design/storage-backed-bootstrap.md).

## Flow

In this guide, you:

1. Register the preview features and create an AKS cluster with `networkPlugin=none`.
2. Install Unbounded-Net and connect the AKS and Flex network Sites over the existing private Layer 3 path.
3. Create a Flex node pool.
4. Prepare an Azure VM host and authorize its managed identity with the pool-scoped Flex Node Agent Role.
5. Download and run the versioned bootstrap script.
6. Verify the Azure Machine, Kubernetes Node, networking, and workload connectivity.

## Prerequisites

The Bash environment needs:

- Azure CLI 2.90.0 or later, authenticated to the target subscription;
- `aks-preview` Azure CLI extension `22.0.0b8` or later;
- `kubectl`, `curl`, `tar`, and an OpenSSH client;
- permission to create AKS and networking resources, register preview features, create a Flex node pool, and assign the selected host identity the Flex Node Agent Role at the target ARM agent pool;
- the Flex Node Agent Role published and visible in the target environment;
- access to the AKS admin kubeconfig;
- the `kubectl-unbounded` release matching the selected Unbounded artifacts;
- network access to the AKS API server and the flex node host management endpoint.

The flex node host needs:

- Ubuntu 24.04;
- a unique lowercase hostname suitable for a Kubernetes Node name;
- at least 4 vCPUs for the validated example;
- a root filesystem with at least 8 GiB free under `/var/lib`;
- an SSH account with root or passwordless `sudo` access;
- Bash, curl, tar, jq, nftables, systemd-container, and util-linux;
- network access to the AKS API server, Azure Resource Manager, Microsoft Entra ID, the selected agent release and artifact mirror, and required container registries;
- private Layer 3 connectivity to the AKS-managed node network, including the paths required for nodes, pods, services, and API-server-to-kubelet callbacks.

Before bootstrap, assign a managed identity to the Azure VM and grant it Flex Node Agent Role at the target ARM agent-pool scope. For a host outside Azure, prefer Azure Arc; use a service principal only when neither managed identity nor Azure Arc is available.

This guide uses:

```bash
export SUBSCRIPTION_ID="<subscription-id>"
export RESOURCE_GROUP="<resource-group>"
export AKS_NAME="<cluster-name>"
export AKS_LOCATION="<aks-region>"
export AKS_SUBNET_ID="<aks-subnet-resource-id>"

export AKS_VERSION="1.36.2"
export FLEX_VERSION="${FLEX_VERSION:-$AKS_VERSION}"
export FLEX_POOL_NAME="aksflexnodes"
export AKS_PREVIEW_VERSION="22.0.0b8"

export SERVICE_CIDR="10.94.0.0/16"
export DNS_SERVICE_IP="10.94.0.10"
export CLUSTER_NODE_CIDR="10.91.0.0/16"
export CLUSTER_POD_CIDR="10.93.0.0/16"
export FLEX_NODE_CIDR="10.92.0.0/16"
export FLEX_POD_CIDR="10.95.0.0/16"

export UNBOUNDED_VERSION="v0.8.0"
export AKS_FLEX_NODE_VERSION="v0.2.0"
```

`FLEX_VERSION` defaults to the AKS control-plane version so the new FlexNodes
pool is aligned with the cluster. Override it only when intentionally using an
AKS-supported version skew. Host bootstrap does not need a separate version
flag; `listBootstrapData` returns the pool's accepted full patch version and the
artifact template resolves from that value.

The cluster and Flex host networks must have private L3 connectivity. Use one
routed VNet, VNet peering, VPN, ExpressRoute, or an equivalent network design.
The CIDRs above must not overlap.

<a id="1-setup-azure-features-and-networking-base"></a>
<a id="2-create-a-no-cni-aks-cluster"></a>
## 1. Create a no-CNI AKS cluster

Select the subscription and confirm the active account:

```bash
az account set --subscription "$SUBSCRIPTION_ID"
az account show --query '{Name:name,SubscriptionId:id}' --output table
```

Install the preview Azure CLI extension required for Flex node pool commands:

```bash
az extension add \
  --name aks-preview \
  --allow-preview true \
  --version "$AKS_PREVIEW_VERSION" \
  --upgrade

az extension show --name aks-preview --query version --output tsv
```

Register the subscription-level `AKSFlexNodePreview` and `PutMachinePreview` features. Registration only needs to be completed once per subscription:

```bash
az feature register \
  --namespace Microsoft.ContainerService \
  --name AKSFlexNodePreview

az feature register \
  --namespace Microsoft.ContainerService \
  --name PutMachinePreview
```

Wait until both features report `Registered`:

```bash
az feature show \
  --namespace Microsoft.ContainerService \
  --name AKSFlexNodePreview \
  --query properties.state \
  --output tsv

az feature show \
  --namespace Microsoft.ContainerService \
  --name PutMachinePreview \
  --query properties.state \
  --output tsv
```

After both features are registered, refresh the resource provider registration:

```bash
az provider register \
  --namespace Microsoft.ContainerService \
  --wait

az provider show \
  --namespace Microsoft.ContainerService \
  --query registrationState \
  --output tsv
```

Don't continue until both feature commands and the provider command return `Registered`. Without these features, Azure CLI can't create the Flex node pool and the agent can't complete its Azure Machine create or update operation even when its identity has the correct role assignment.

This guide assumes that the resource group, VNet, and AKS subnet already exist. Set `AKS_SUBNET_ID` to the full resource ID of the subnet where the managed system pool will run. For a complete network-creation example, see [Create resource groups and networks](../labs/aks-public-cluster-unbounded-net-vnet-peering.md#create-resource-groups-and-networks).

Create AKS with no CNI plugin:

```bash
az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --location "$AKS_LOCATION" \
  --kubernetes-version "$AKS_VERSION" \
  --nodepool-name nodepool1 \
  --node-count 1 \
  --node-vm-size Standard_D4s_v6 \
  --network-plugin none \
  --pod-cidr "$CLUSTER_POD_CIDR" \
  --vnet-subnet-id "$AKS_SUBNET_ID" \
  --service-cidr "$SERVICE_CIDR" \
  --dns-service-ip "$DNS_SERVICE_IP" \
  --enable-managed-identity \
  --ssh-key-value "$HOME/.ssh/id_rsa.pub"
```

Load the admin kubeconfig:

```bash
az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --admin \
  --overwrite-existing
```

The system Node can initially be `NotReady` because no component has installed a
CNI configuration yet. Unbounded handles that in the next step.

Verify the cluster version and no-CNI setting:

```bash
az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --query '{state:provisioningState,version:kubernetesVersion,networkPlugin:networkProfile.networkPlugin}' \
  --output yaml
```

<a id="3-install-unbounded-and-initialize-the-sites"></a>
## 2. Install Unbounded and initialize the sites

Download the `kubectl-unbounded` binary appropriate for the workstation:

```bash
case "$(uname -m)" in
  x86_64) UNBOUNDED_ARCH=amd64 ;;
  aarch64|arm64) UNBOUNDED_ARCH=arm64 ;;
  *) echo "unsupported workstation architecture" >&2; exit 1 ;;
esac

curl -fsSLo /tmp/kubectl-unbounded.tar.gz \
  "https://github.com/Azure/unbounded/releases/download/${UNBOUNDED_VERSION}/kubectl-unbounded-linux-${UNBOUNDED_ARCH}.tar.gz"

tar -xzf /tmp/kubectl-unbounded.tar.gz -C /tmp
sudo install -m 0755 /tmp/kubectl-unbounded /usr/local/bin/kubectl-unbounded
kubectl unbounded version
```

Install the Unbounded operator, and then initialize the cluster and Flex sites:

```bash
kubectl unbounded install --timeout 5m

kubectl unbounded site init \
  --name flex-site \
  --cluster-node-cidr "$CLUSTER_NODE_CIDR" \
  --cluster-pod-cidr "$CLUSTER_POD_CIDR" \
  --node-cidr "$FLEX_NODE_CIDR" \
  --pod-cidr "$FLEX_POD_CIDR"
```

This installs the Unbounded operator and enables the networking and Machina
components. It creates:

- the `cluster` Site for the managed AKS Nodes;
- the `flex-site` Site for external Flex Nodes;
- Unbounded networking CRDs and controllers;
- Machina Machine and MachineOperation CRDs;
- the Unbounded node DaemonSet.

Create peering between the two sites. The validated VNet-peered environment used
node mesh mode:

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: cluster-flex-private-l3
spec:
  sites:
  - cluster
  - flex-site
  meshNodes: true
  tunnelProtocol: Auto
EOF
```

Wait for the CNI to converge:

```bash
kubectl -n unbounded-system rollout status \
  deployment/unbounded-net-controller --timeout=5m

kubectl -n unbounded-system rollout status \
  daemonset/unbounded-net-node --timeout=5m

kubectl get nodes -L unbounded-cloud.io/site -o wide
kubectl get sites,sitepeerings -o wide
```

The managed system Node should become `Ready` and carry the `cluster` site
label.

<a id="3-install-temporary-aks-flex-daemon-rbac"></a>
<a id="4-create-the-flexnodes-pool"></a>
## 3. Create the FlexNodes pool

A Flex node pool is a logical group for customer-provided compute. Don't configure standard virtual machine scale set properties such as node count, VM size, operating system type, or subnet settings.

Wait for any cluster operation started by extension or policy reconciliation to finish, and then create the pool with the preview Azure CLI extension:

```bash
while true; do
  if ! STATUS=$(az aks operation show-latest \
    --resource-group "$RESOURCE_GROUP" --name "$AKS_NAME" \
    --query status --output tsv); then
    echo "Failed to query the latest AKS operation" >&2
    exit 1
  fi
  case "$STATUS" in
    InProgress|Running|Updating) sleep 15 ;;
    Succeeded) break ;;
    Failed|Canceled)
      echo "Latest AKS operation ended with status: $STATUS" >&2
      exit 1
      ;;
    *)
      echo "Unexpected AKS operation status: $STATUS" >&2
      exit 1
      ;;
  esac
done

az aks nodepool add \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$AKS_NAME" \
  --name "$FLEX_POOL_NAME" \
  --vm-set-type FlexNodes \
  --mode User \
  --kubernetes-version "$FLEX_VERSION" \
  --max-pods 250 \
  --max-unavailable 1 \
  --output none
```

Record the cluster and pool resource IDs, and then inspect the completed pool:

```bash
AKS_RESOURCE_ID=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --query id \
  --output tsv)

FLEX_POOL_RESOURCE_ID="${AKS_RESOURCE_ID}/agentPools/${FLEX_POOL_NAME}"

az resource show \
  --ids "$FLEX_POOL_RESOURCE_ID" \
  --api-version 2026-05-02-preview \
  --query '{name:name,state:properties.provisioningState,type:properties.type,version:properties.orchestratorVersion}' \
  --output table
```

Continue when the provisioning state is `Succeeded`, the type is `FlexNodes`, and the returned version matches `FLEX_VERSION`.

<a id="5-prepare-the-flex-host-and-azure-identity"></a>
## 4. Prepare the Flex host and Azure identity

Each Flex node host needs an Azure identity so the agent can create and continuously read its Azure Machine resource. This guide uses an Azure VM managed identity. Use the system-assigned identity, or provide the client ID of a user-assigned identity.

For a host outside Azure, use the managed identity of an existing Azure Arc-enabled server. Use a service principal only when neither Azure VM managed identity nor Azure Arc is available; see [Joining nodes](joining-nodes.md) for those alternatives.

### Assign the host identity's pool-scoped role

Assign **Azure Kubernetes Service Flex Node Agent Role** (`8f139b0f-7eaf-460b-a9da-5b1246d9ed0d`) to the VM's managed identity at the target ARM agent-pool scope. The role includes these permissions:

- `Microsoft.ContainerService/managedClusters/agentPools/listBootstrapData/action`
- `Microsoft.ContainerService/managedClusters/agentPools/machines/read`
- `Microsoft.ContainerService/managedClusters/agentPools/machines/write`

Set the selected identity's principal object ID and assign the role:

```bash
FLEX_NODE_AGENT_ROLE_ID="8f139b0f-7eaf-460b-a9da-5b1246d9ed0d"
FLEX_HOST_PRINCIPAL_ID="<managed-identity-principal-object-id>"

az role assignment create \
  --subscription "$SUBSCRIPTION_ID" \
  --assignee-object-id "$FLEX_HOST_PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "$FLEX_NODE_AGENT_ROLE_ID" \
  --scope "$FLEX_POOL_RESOURCE_ID" \
  --output none

az role assignment list \
  --subscription "$SUBSCRIPTION_ID" \
  --assignee-object-id "$FLEX_HOST_PRINCIPAL_ID" \
  --role "$FLEX_NODE_AGENT_ROLE_ID" \
  --scope "$FLEX_POOL_RESOURCE_ID" \
  --fill-principal-name false \
  --fill-role-definition-name false \
  --query '[].{principalId:principalId,roleDefinitionId:roleDefinitionId,scope:scope}' \
  --output table
```

Use the managed identity's principal/object ID, not its client ID. For a system-assigned identity, use the VM's `identity.principalId`; for a user-assigned identity, use its `principalId`. The user-assigned identity's client ID belongs in `--msi-client-id` during bootstrap.

Verify the exact pool scope and allow role-assignment propagation before bootstrap.

Host and identity provisioning are otherwise outside this guide. Start with a prepared Ubuntu host that can reach the AKS API server and artifact endpoints. Use a 32 GiB or larger OS disk; preflight requires at least 8 GiB free under `/var/lib`. The bootstrap workflow installs missing supported host packages on demand.

Azure CLI doesn't need to be installed on the host. The bootstrap workflow authenticates the VM through Azure Instance Metadata Service.

<a id="6-download-and-run-the-bootstrap-script"></a>
## 5. Download and run the bootstrap script

SSH to the target host and become root:

```bash
ssh "<admin-user>@<host-address>"
sudo -i
```

Set the operator-provided values:

```bash
export AKS_RESOURCE_ID="<full-aks-resource-id>"
export FLEX_POOL_NAME="aksflexnodes"
export AKS_FLEX_NODE_VERSION="v0.2.0"

```

This getting-started flow downloads the agent from its GitHub release and uses the default online rootfs and component sources. Use the [offline artifacts lab](../labs/aks-public-cluster-offline-bootstrap.md) when you need mirrored or filesystem-backed artifacts.

Download the raw script instead of piping it directly to Bash:

```bash
install -d -m 0700 /run/aks-flex-node-bootstrap

curl -fsSLo /run/aks-flex-node-bootstrap/bootstrap.sh \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/bootstrap.sh"

chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
bash -n /run/aks-flex-node-bootstrap/bootstrap.sh
```

The version-matched repository script contains an unpopulated embedded-config marker. When `--fetch-bootstrap-data`, `--cluster-resource-id`, and `--agent-pool-name` are provided together, the script starts from an empty config and obtains fresh cluster-issued join settings from AKS. No base config file is required.

Run bootstrap with the VM managed identity. For a system-assigned identity:



```bash
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth msi \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-version "$AKS_FLEX_NODE_VERSION"
```

For a user-assigned managed identity, add its client ID:

```bash
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth msi \
  --msi-client-id "<user-assigned-managed-identity-client-id>" \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-version "$AKS_FLEX_NODE_VERSION"
```

The selected managed identity must be assigned to the VM and have Flex Node Agent Role at the target ARM agent-pool scope.

For a host outside Azure, prefer an already-connected Azure Arc managed identity and `--auth arc`. Use `--auth service-principal` only when managed identity and Azure Arc aren't available. See [Joining nodes](joining-nodes.md) for those alternatives.

The script performs these operations:

1. Loads the empty JSON base.
2. Applies the cluster and pool overrides.
3. Uses the Azure VM managed identity to request an ARM token.
4. Calls `listBootstrapData` for a fresh bootstrap token, API endpoint, CA, and
   component version.
5. Applies runtime configuration overrides.
6. Downloads and verifies the AKS Flex Node agent archive from GitHub Releases.
7. Writes `/etc/aks-flex-node/config.json` as `0600 root:root`.
8. Runs non-mutating preflight.
9. Registers the ARM Machine and starts the nspawn worker.
10. Installs and starts `aks-flex-node-agent.service`.

On success, remove the transient files and record completion:

```bash
rm -f /run/aks-flex-node-bootstrap/bootstrap.sh

install -d -m 0755 /var/lib/aks-flex-node
install -m 0600 /dev/null /var/lib/aks-flex-node/first-boot-complete
```

<a id="7-verify-the-joined-node"></a>
<a id="8-verify-the-joined-node"></a>
## 6. Verify the joined node

Check the Node and network site:

```bash
kubectl get nodes -L unbounded-cloud.io/site -o wide
kubectl get sites,sitepeerings -o wide
```

Check the Azure Machine resource from your Bash environment:

```bash
az aks machine list \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$AKS_NAME" \
  --nodepool-name "$FLEX_POOL_NAME" \
  --query '[].{name:name,state:properties.provisioningState,nodeName:properties.kubernetes.nodeName,version:properties.kubernetes.currentOrchestratorVersion}' \
  --output table
```

Continue when the Machine for the new Kubernetes Node reports a successful provisioning state and the expected Kubernetes version.

On the host:

```bash
stat -c '%a %U:%G %n' /etc/aks-flex-node/config.json
systemctl is-active aks-flex-node-agent
systemctl is-enabled aks-flex-node-agent
machinectl list
systemctl -M kube1 is-active kubelet containerd
```

Expected config permissions:

```text
600 root:root /etc/aks-flex-node/config.json
```

Expected bootstrap log entries:

```text
bootstrap: fetching fresh bootstrap data from AKS RP
bootstrap: downloading AKS Flex Node agent (URL redacted)
bootstrap: rendered config at /etc/aks-flex-node/config.json
bootstrap: running preflight
bootstrap: starting AKS Flex Node
```

Expected Azure Machine registration log:

```text
level=INFO msg=started task=ensure-machine
level=INFO msg="creating or updating AKS machine" machine=<node-name> pool=aksflexnodes
level=INFO msg=completed task=ensure-machine status=ok
```

Run a smoke workload pinned to the Flex Node to verify pod networking.

## Clean up

Follow [Reset and uninstall](operations.md#reset-and-uninstall) to drain the Kubernetes Node, remove local host state, and remove residual Node and Machine resources. If this guide created dedicated Azure resource groups, delete them only after you confirm that they don't contain shared resources.

## Troubleshooting

### System Node remains NotReady

Ensure the Unbounded operator, controller, and node DaemonSet are running and
that the managed AKS Node has the `cluster` site label:

```bash
kubectl -n unbounded-system get pods -o wide
kubectl get nodes -L unbounded-cloud.io/site
```

### Preflight reports insufficient disk space

Resize the host OS disk. The agent requires at least 8 GiB free under `/var/lib`.
Use 32 GiB or more for the validated examples.

### ARM Machine registration fails

Confirm:

- the selected managed identity is assigned to the VM;
- Azure Kubernetes Service Flex Node Agent Role
  (`8f139b0f-7eaf-460b-a9da-5b1246d9ed0d`) is published/visible in the target
  environment and assigned to that principal at the exact target ARM agent pool;
- role assignment propagation has completed;
- `--cluster-resource-id` and `--agent-pool-name` are correct.

### Bootstrap token expired

Generate fresh data by rerunning bootstrap with `--fetch-bootstrap-data`. Do not
reuse a generated script or config after its bootstrap token has expired.
