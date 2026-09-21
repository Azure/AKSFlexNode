# Operator Guide: Bootstrap an AKS Flex Node

This guide describes the current end-to-end operator flow for creating an AKS
cluster with no CNI, installing Unbounded networking, creating a FlexNodes pool,
and joining a prepared Linux host with [`scripts/bootstrap.sh`](../../scripts/bootstrap.sh).

The bootstrap script is downloaded and run interactively on the host. This guide
does not use cloud-init.

For the architecture and security rationale, see
[Generated Bootstrap Script Design](../design/storage-backed-bootstrap.md).

## Flow

1. Setup base Azure resources
2. Create an AKS cluster with `networkPlugin=none`.
3. Install the Unbounded operator and initialize the cluster and Flex sites.
4. Create a FlexNodes agent pool.
5. Prepare a host, download `bootstrap.sh`, and run it with SP or MSI authentication.
6. Approve the daemon CSR when no AKS Flex CSR controller is deployed.
7. Verify the ARM Machine, Kubernetes Node, networking, and agent service.

## Prerequisites

The operator workstation needs:

- An Azure subscription. [Create one for free](https://azure.microsoft.com/free/).
- Azure CLI authenticated to the target subscription.
- The following programs installed: `kubectl`, `curl`, and `python3`
- the subscription-level `Microsoft.ContainerService/PutMachinePreview` feature
  registered, followed by Microsoft.ContainerService provider re-registration
- permission to create AKS and networking resources and to grant the selected
  pre-provisioned identity access to the AKS cluster.
- the `kubectl-unbounded` plugin release matching the Unbounded artifacts.

The target host needs:

- Ubuntu 24.04;
- at least 4 vCPU for the validated example
- a root filesystem with at least 8 GiB free under `/var/lib`
- Bash, curl, tar, jq, nftables, systemd-container, and util-linux
- SSH access from your workstation to the target host
- Outbound network access to GitHub, Azure Front Door, Azure storage, and Azure Container Registries
- Network connectivity to the AKS vnet (For a demo on Azure just create the flex virtual machines in the same vnet as the AKS nodepools.)
- a unique lowercase hostname suitable for a Kubernetes Node name.

This guide uses:

```bash
export SUBSCRIPTION_ID="<subscription-id>"
export RESOURCE_GROUP="<resource-group>"
export AKS_NAME="<cluster-name>"
export AKS_LOCATION="<aks-region>"
export AKS_NODE_SKU="Standard_D4as_v6"

export AKS_VERSION="1.36.2"
export FLEX_VERSION="${FLEX_VERSION:-$AKS_VERSION}"
export FLEX_POOL_NAME="aksflexnodes"

export VNET_CIDR="10.90.0.0/15"
export SERVICE_CIDR="10.255.0.0/16"
export DNS_SERVICE_IP="10.255.0.10"
export CLUSTER_NODE_CIDR="10.90.0.0/16"
export CLUSTER_POD_CIDR="10.190.0.0/16"
export FLEX_NODE_CIDR="10.91.0.0/16"
export FLEX_POD_CIDR="10.191.0.0/16"

export UNBOUNDED_VERSION="v0.8.0"
export AKS_FLEX_NODE_VERSION="v0.1.10"
export CENTRAL_ARTIFACTS_ENDPOINT="https://azure-mirror.unbounded-cloud.io"

# bootstrap.sh downloads the agent, rootfs, and Kubernetes bootstrap bundle
# from this central artifact endpoint.
export AKS_FLEX_NODE_AGENT_URL="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/aks-flex-node/${AKS_FLEX_NODE_VERSION}/{{ARCHIVE_NAME}}"
# For an NVIDIA GPU host, use rootfs-agent-ubuntu2404-nvidia-v20260619.oci.tar.gz.
export BOOTSTRAP_OCI_IMAGE="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/${UNBOUNDED_VERSION}/rootfs/rootfs-agent-ubuntu2404-v20260619.oci.tar.gz"
export BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/${UNBOUNDED_VERSION}/bootstrap-artifacts/bootstrap-artifacts-k8s-${AKS_VERSION}$.tar.gz"
```

`FLEX_VERSION` defaults to the AKS control-plane version so the new FlexNodes
pool is aligned with the cluster. Override it only when intentionally using an
AKS-supported version skew. Host bootstrap does not need a separate version
flag; `listBootstrapData` returns the pool's accepted full patch version and the
artifact template resolves from that value.

The cluster and Flex host networks must have private L3 connectivity. Use one
routed VNet, VNet peering, VPN, ExpressRoute, or an equivalent network design.
The CIDRs above must not overlap.

## 1. Setup Azure features and networking base

> **TODO:** Azure CLI will support creating FlexNodes pools without the extension
> in a future release. This command block can be removed once the nodepool commands
> are out of preview.

Install the aks-preview azure-cli extension to add support for the new AKS commands:

```bash
az extension add --name aks-preview
```

Register the preview feature that permits AKS Flex Node to create or update ARM
Machine resources. Registration is subscription-scoped and only needs to be
completed once:

```bash
az feature register \
  --subscription ${SUBSCRIPTION_ID} \
  --namespace Microsoft.ContainerService \
  --name PutMachinePreview
```

Wait until registration reports `Registered`:

```bash
az feature show \
  --subscription ${SUBSCRIPTION_ID} \
  --namespace Microsoft.ContainerService \
  --name PutMachinePreview \
  --query properties.state \
  --output tsv
```

After the state becomes `Registered`, re-register the resource provider so the
feature takes effect:

```bash
az provider register \
  --subscription ${SUBSCRIPTION_ID} \
  --namespace Microsoft.ContainerService \
  --wait
```

Do not continue to host bootstrap while the feature is still `Registering`.
Without `PutMachinePreview`, the agent cannot complete its ARM Machine
create/update step even when its managed identity or service principal has the
correct AKS role assignment.

Create or select the resource group and networking before this step. The AKS
subnet ID must refer to the subnet where the managed system pool will run.

Create the resource group:

```bash
az group create \
  --subscription ${SUBSCRIPTION_ID} \
  --name "${RESOURCE_GROUP}" \
  --location "${AKS_LOCATION}"
```

Create the virtual network for the AKS cluster:
```bash
az network vnet create \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${AKS_NAME}-vnet" \
  --location "${AKS_LOCATION}" \
  --address-prefix "${VNET_CIDR}" \
  --subnet-name "aks" \
  --subnet-prefixes "${CLUSTER_NODE_CIDR}"
```

Create the flex pool subnet:

```bash
az network vnet subnet create \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "${RESOURCE_GROUP}" \
  --vnet-name "${AKS_NAME}-vnet" \
  --name "flex-nodes" \
  --address-prefixes "${FLEX_NODE_CIDR}"
```

Export the AKS subnet ID for the AKS create command:

```bash
export AKS_SUBNET_ID=$(az network vnet subnet show --subscription ${SUBSCRIPTION_ID} --resource-group ${RESOURCE_GROUP} --vnet-name "${AKS_NAME}-vnet" --name aks --query id --output tsv)
```

## 2. Create a no-CNI AKS cluster

Create an AKS cluster:

```bash
az aks create \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "${RESOURCE_GROUP}" \
  --name "${AKS_NAME}" \
  --location "${AKS_LOCATION}" \
  --kubernetes-version "${AKS_VERSION}" \
  --nodepool-name nodepool1 \
  --node-count 1 \
  --node-vm-size ${AKS_NODE_SKU} \
  --network-plugin none \
  --vnet-subnet-id "$AKS_SUBNET_ID" \
  --service-cidr "$SERVICE_CIDR" \
  --dns-service-ip "$DNS_SERVICE_IP" \
  --enable-managed-identity \
  --ssh-key-value "$HOME/.ssh/id_rsa.pub"
```

Load the admin kubeconfig:

```bash
az aks get-credentials \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --admin
```

The system Node can initially be `NotReady` because no component has installed a
CNI configuration yet. Unbounded handles that in the next step.

Verify the cluster version and no-CNI setting:

```bash
az aks show \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --query '{state:provisioningState,version:kubernetesVersion,networkPlugin:networkProfile.networkPlugin}' \
  --output yaml
```

## 3. Install Unbounded and initialize the sites

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

Install the unbounded operator:
```bash
kubectl unbounded install
```

Initialize the cluster and Flex sites:

```bash
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

kubectl get nodes -L net.unbounded-cloud.io/site -o wide
kubectl get sites,sitepeerings -o wide
```

The managed system Node should become `Ready` and carry the `cluster` site
label.

## 4. Create the FlexNodes pool

Create the pool:

```bash
az aks nodepool add \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "${RESOURCE_GROUP}" \
  --cluster-name "${AKS_NAME}" \
  --name "${FLEX_POOL_NAME}" \
  --vm-set-type FlexNodes \
  --kubernetes-version ${AKS_VERSION}
```

Inspect the completed pool:

```bash
az aks nodepool show \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group "${RESOURCE_GROUP}" \
  --cluster-name "${AKS_NAME}" \
  --name "${FLEX_POOL_NAME}" \
  --output table
```

Do not include normal VMSS properties such as `osType`, `count`, `vmSize`, or
subnet settings. The RP rejects unsupported FlexNodes pool properties.

## 5. Prepare the Flex host and Azure identity

Each Flex Node needs an Azure identity so the agent can create and continuously
read its ARM Machine resource. The supported identity modes are:

- **Managed identity** for an Azure VM. Use the system-assigned identity, or
  provide the client ID of a user-assigned identity.
- **Service principal** for an Azure VM or a host outside Azure. Provide its
  tenant ID, client ID, and either a client secret or certificate/private-key
  credential through a protected file.

The selected managed identity or service principal must have **Azure Kubernetes
Service Contributor Role** at the target AKS cluster resource scope. For this guide
we will use a managed identity.

Create the managed identity:

```bash
az identity create \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group ${RESOURCE_GROUP} \
  --name "${AKS_NAME}-mi" \
  --location ${AKS_LOCATION}
```

Assign the `Azure Kubernetes Service Contributor Role` role to the new identity:

```bash
export MI_PRINCIPAL_ID=$(az identity show --subscription ${SUBSCRIPTION_ID} --resource-group ${RESOURCE_GROUP} --name "${AKS_NAME}-mi" --query principalId --output tsv)
export AKS_RESOURCE_ID=$(az aks show --subscription ${SUBSCRIPTION_ID} --resource-group ${RESOURCE_GROUP} --name ${AKS_NAME} --query id --output tsv)
az role assignment create \
  --assignee-object-id ${MI_PRINCIPAL_ID} \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Kubernetes Service Contributor Role" \
  --scope ${AKS_RESOURCE_ID}
```

Create the node pool that will be used for the flex nodes you can use this to create a 3 node VMSS pool:

```bash
export MI_CLIENT_ID=$(az identity show --subscription ${SUBSCRIPTION_ID} --resource-group ${RESOURCE_GROUP} --name "${AKS_NAME}-mi" --query id --output tsv)
az vmss create \
  --subscription ${SUBSCRIPTION_ID} \
  --resource-group ${RESOURCE_GROUP} \
  --name ${FLEX_POOL_NAME} \
  --location ${AKS_LOCATION} \
  --image Ubuntu2404 \
  --instance-count 3 \
  --orchestration-mode Uniform \
  --vm-sku ${AKS_NODE_SKU} \
  --disk-controller-type nvme \
  --vnet-name "${AKS_NAME}-vnet" \
  --subnet flex-nodes \
  --admin-username azureuser \
  --authentication-type ssh \
  --ssh-key-values ~/.ssh/id_rsa.pub \
  --load-balancer "${FLEX_POOL_NAME}-lb" \
  --lb-sku Standard \
  --lb-nat-rule-name ssh \
  --backend-port 22 \
  --assign-identity ${MI_RESOURCE_ID}
```

## 6. Download and run the bootstrap script

SSH to the target host and become root:

```bash
ssh "<admin-user>@<host-address>"
sudo -i
```

Install the host prerequisites:

```bash
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y \
  curl \
  jq \
  nftables \
  systemd-container \
  tar \
  util-linux
```

Set the operator-provided values:

```bash
export AKS_RESOURCE_ID="<full-aks-resource-id>"
export FLEX_POOL_NAME="aksflexnodes"
export AKS_FLEX_NODE_VERSION="v0.1.10"
export UNBOUNDED_VERSION="v0.8.0"
export CENTRAL_ARTIFACTS_ENDPOINT="https://azure-mirror.unbounded-cloud.io"

# bootstrap.sh downloads the agent, rootfs, and Kubernetes bootstrap bundle
# from this central artifact endpoint.
export AKS_FLEX_NODE_AGENT_URL="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/aks-flex-node/${AKS_FLEX_NODE_VERSION}/{{ARCHIVE_NAME}}"
# For an NVIDIA GPU host, use rootfs-agent-ubuntu2404-nvidia-v20260619.oci.tar.gz.
export BOOTSTRAP_OCI_IMAGE="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/${UNBOUNDED_VERSION}/rootfs/rootfs-agent-ubuntu2404-v20260619.oci.tar.gz"
export BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/${UNBOUNDED_VERSION}/bootstrap-artifacts/bootstrap-artifacts-k8s-{{ .KubernetesVersion }}.tar.gz"

export FLEX_SP_TENANT_ID="<service-principal-tenant-id>"
export FLEX_SP_CLIENT_ID="<service-principal-client-id>"
export FLEX_SP_CLIENT_SECRET_FILE="/etc/aks-flex-node/credentials/sp-client-secret"
# Certificate alternative; the PEM filename does not require an extension.
# An unencrypted binary PKCS#12 file must use a .pfx suffix.
export FLEX_SP_CLIENT_CERTIFICATE_FILE="/etc/aks-flex-node/credentials/sp-client-certificate"
```

The shell expands `${AKS_FLEX_NODE_VERSION}`, and the bootstrap script expands
`{{ARCHIVE_NAME}}` for the host architecture. On an AMD64 host this resolves to:

```text
https://azure-mirror.unbounded-cloud.io/releases/aks-flex-node/v0.1.5/aks-flex-node-linux-amd64.tar.gz
```

These URLs download the agent, rootfs, and bootstrap bundle from the central
artifact endpoint. Change `CENTRAL_ARTIFACTS_ENDPOINT` to use another mirror.

AKS RP bootstrap data does not currently include the mirrored rootfs and
offline-artifact locations, so the command supplies them through dedicated CLI
overrides.

Download the raw script instead of piping it directly to Bash:

```bash
install -d -m 0700 /run/aks-flex-node-bootstrap

curl -fsSLo /run/aks-flex-node-bootstrap/bootstrap.sh \
  https://raw.githubusercontent.com/Azure/AKSFlexNode/refs/heads/main/scripts/bootstrap.sh

chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
```

The raw repository script contains an unpopulated embedded-config marker. When
`--fetch-bootstrap-data`, `--cluster-resource-id`, and `--agent-pool-name` are
provided together, the script automatically starts from an empty config and
obtains fresh cluster-issued join settings from AKS RP. No base config file is
required.

Install the service-principal credential from the operator's protected secret
delivery path. Keep this file available after bootstrap because the running
agent uses it for ARM Machine reconciliation:

```bash
install -d -o root -g root -m 0700 /etc/aks-flex-node/credentials
install -o root -g root -m 0600 \
  "<path-to-provisioned-client-secret>" \
  "$FLEX_SP_CLIENT_SECRET_FILE"
```

Run bootstrap with the service principal:

```bash
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth service-principal \
  --sp-tenant-id "$FLEX_SP_TENANT_ID" \
  --sp-client-id "$FLEX_SP_CLIENT_ID" \
  --sp-client-secret-file "$FLEX_SP_CLIENT_SECRET_FILE" \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-url "$AKS_FLEX_NODE_AGENT_URL" \
  --bootstrap-oci-image "$BOOTSTRAP_OCI_IMAGE" \
  --bootstrap-offline-artifacts-source "$BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE"
```

For certificate-based service-principal authentication, install a protected PEM
file containing the leaf-first certificate chain and RSA private key, or an
unencrypted PKCS#12 file with a `.pfx` suffix. PEM detection is content-based, so
its filename does not require a `.pem` suffix. Keep the file available for agent
service restarts. The script delegates certificate authentication to the
installed `aks-flex-node fetch-bootstrap-data` command; OpenSSL is not required
on the host for assertion construction. Azure Identity sends the leaf
thumbprint in `x5t` and the public chain in `x5c`, supporting both directly
registered certificates and Microsoft Entra Subject Name/Issuer trust policies.

```bash
install -o root -g root -m 0600 \
  "<path-to-provisioned-client-certificate-and-key>" \
  "$FLEX_SP_CLIENT_CERTIFICATE_FILE"

bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth service-principal \
  --sp-tenant-id "$FLEX_SP_TENANT_ID" \
  --sp-client-id "$FLEX_SP_CLIENT_ID" \
  --sp-client-certificate-file "$FLEX_SP_CLIENT_CERTIFICATE_FILE" \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-url "$AKS_FLEX_NODE_AGENT_URL" \
  --bootstrap-oci-image "$BOOTSTRAP_OCI_IMAGE" \
  --bootstrap-offline-artifacts-source "$BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE"
```

For an Azure VM with a system-assigned managed identity, use the same command
but replace the service-principal flags with:

```bash
  --auth msi \
```

For a user-assigned managed identity, use:

```bash
  --auth msi \
  --msi-client-id "<user-assigned-managed-identity-client-id>" \
```

The selected managed identity must be assigned to the VM and have the same AKS
Contributor role at the cluster scope.

The script performs these operations:

1. Loads the empty JSON base.
2. Applies the cluster and pool overrides.
3. Uses the selected service principal or managed identity to request an ARM
   token.
4. Calls `listBootstrapData` for a fresh bootstrap token, API endpoint, CA, and
   component version.
5. Applies rootfs, offline artifact, and runtime config overrides.
6. Downloads and verifies the AKS Flex Node agent archive.
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

## 7. Verify the joined node

Check the Node and network site:

```bash
kubectl get nodes -L net.unbounded-cloud.io/site -o wide
kubectl get sites,sitepeerings -o wide
```

Check the ARM Machine:

```bash
MACHINE_NAME="<node-name>"

az rest \
  --method get \
  --uri "https://management.azure.com${AKS_RESOURCE_ID}/agentPools/${FLEX_POOL_NAME}/machines/${MACHINE_NAME}?api-version=2025-10-02-preview" \
  --query '{name:name,state:properties.provisioningState,kubernetes:properties.kubernetes}' \
  --output yaml
```

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

Expected ARM Machine registration log:

```text
level=INFO msg=started task=ensure-machine
level=INFO msg="creating or updating AKS machine" machine=<node-name> pool=aksflexnodes
level=INFO msg=completed task=ensure-machine status=ok
```

Run a smoke workload pinned to the Flex Node to verify pod networking.

## Troubleshooting

### System Node remains NotReady

Ensure the Unbounded operator, controller, and node DaemonSet are running and
that the managed AKS Node has the `cluster` site label:

```bash
kubectl -n unbounded-system get pods -o wide
kubectl get nodes -L net.unbounded-cloud.io/site
```

### Preflight reports insufficient disk space

Resize the host OS disk. The agent requires at least 8 GiB free under `/var/lib`.
Use 32 GiB or more for the validated examples.

### ARM Machine registration fails

Confirm:

- the selected managed identity is assigned to the host, or the service
  principal credential file is present and mode `0600`;
- AKS Contributor is scoped to the target cluster for that identity;
- role assignment propagation has completed;
- `--cluster-resource-id` and `--agent-pool-name` are correct.

### MachineOperation cache synchronization times out

Confirm the temporary ClusterRole and ClusterRoleBinding from step 3 exist. A
future AKS RP release will install these resources automatically.

### Node joins but the host agent restarts

Inspect pending CSRs. The kubelet can become Ready while the separate daemon CSR
still requires approval.

```bash
journalctl -u aks-flex-node-agent --no-pager -n 200
kubectl get csr
```

### Bootstrap token expired

Generate fresh data by rerunning bootstrap with `--fetch-bootstrap-data`. Do not
reuse a generated script or config after its bootstrap token has expired.
