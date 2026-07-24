# Operator Guide: Bootstrap an AKS Flex Node

This guide describes the current end-to-end operator flow for creating an AKS
cluster with no CNI, installing Unbounded networking, creating a FlexNodes pool,
and joining a prepared Linux host with [`scripts/bootstrap.sh`](../../scripts/bootstrap.sh).

The bootstrap script is downloaded and run interactively on the host. This guide
does not use cloud-init.

For the script's complete option reference and security model, see
[Generated Bootstrap Script](bootstrap-script.md).

## Flow

1. Create an AKS cluster with `networkPlugin=none`.
2. Install the Unbounded operator and initialize the cluster and Flex sites.
3. Install the temporary AKS Flex daemon MachineOperation RBAC.
4. Create a FlexNodes agent pool.
5. Prepare a host, download `bootstrap.sh`, and run it with MSI authentication.
6. Approve the daemon CSR when no AKS Flex CSR controller is deployed.
7. Verify the ARM Machine, Kubernetes Node, networking, and agent service.

## Prerequisites

The operator workstation needs:

- Azure CLI authenticated to the target subscription;
- `kubectl`;
- permission to create AKS, networking, managed identity, role assignment, and
  virtual machine resources;
- access to the AKS admin kubeconfig;
- the `kubectl-unbounded` release matching the Unbounded artifacts.

The target host needs:

- Ubuntu 22.04/24.04 or Azure Linux 3;
- at least 4 vCPU for the validated example;
- a root filesystem with at least 8 GiB free under `/var/lib`;
- Bash, curl, tar, jq, nftables, systemd-container, and util-linux;
- network access to the AKS API server, GitHub agent release, Azure Front Door
  artifact mirror, and required container registries;
- a unique lowercase hostname suitable for a Kubernetes Node name.

This guide uses:

```bash
export SUBSCRIPTION_ID="<subscription-id>"
export RESOURCE_GROUP="<resource-group>"
export AKS_NAME="<cluster-name>"
export AKS_LOCATION="<aks-region>"
export AKS_SUBNET_ID="<aks-subnet-resource-id>"
export FLEX_SUBNET_ID="<flex-host-subnet-resource-id>"

export AKS_VERSION="1.36.2"
export FLEX_VERSION="1.35.6"
export FLEX_POOL_NAME="aksflexnodes"

export SERVICE_CIDR="10.94.0.0/16"
export DNS_SERVICE_IP="10.94.0.10"
export CLUSTER_NODE_CIDR="10.91.0.0/16"
export CLUSTER_POD_CIDR="10.93.0.0/16"
export FLEX_NODE_CIDR="10.92.0.0/16"
export FLEX_POD_CIDR="10.95.0.0/16"

export UNBOUNDED_VERSION="v0.1.24-rc.18"
export AKS_FLEX_NODE_VERSION="v0.1.5.alpha-9"
```

The cluster and Flex host networks must have private L3 connectivity. Use one
routed VNet, VNet peering, VPN, ExpressRoute, or an equivalent network design.
The CIDRs above must not overlap.

## 1. Create a no-CNI AKS cluster

Select the subscription:

```bash
az account set --subscription "$SUBSCRIPTION_ID"
```

Create or select the resource group and networking before this step. The AKS
subnet ID must refer to the subnet where the managed system pool will run.

Create AKS with no CNI plugin:

```bash
az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --location "$AKS_LOCATION" \
  --kubernetes-version "$AKS_VERSION" \
  --nodepool-name nodepool1 \
  --node-count 1 \
  --node-vm-size Standard_D4s_v5 \
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

Initialize the cluster and Flex sites:

```bash
kubectl unbounded site init \
  --name flex-site \
  --cluster-node-cidr "$CLUSTER_NODE_CIDR" \
  --cluster-pod-cidr "$CLUSTER_POD_CIDR" \
  --node-cidr "$FLEX_NODE_CIDR" \
  --pod-cidr "$FLEX_POD_CIDR" \
  --install-timeout 10m
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

## 3. Install temporary AKS Flex daemon RBAC

When the Machina MachineOperation CRD is installed, AKS Flex Node discovers it
and enables its MachineOperation reconciler. A future AKS RP release will
install and manage the required ClusterRole and ClusterRoleBinding automatically
as part of the FlexNodes pool setup. Until that RP release is deployed in the
target region, operators must apply the temporary RBAC below manually.

The Flex daemon certificate belongs to:

```text
aks-flex-node-daemons
```

Install the current integration RBAC:

```bash
kubectl apply -f - <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aks-flex-node-daemon
  labels:
    kubernetes.azure.com/managedby: aks
rules:
- apiGroups:
  - unbounded-cloud.io
  resources:
  - machineoperations
  - machines
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - unbounded-cloud.io
  resources:
  - machineoperations/status
  verbs:
  - update
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-daemon
  labels:
    kubernetes.azure.com/managedby: aks
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aks-flex-node-daemon
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: aks-flex-node-daemons
EOF
```

Without this binding, the host agent can authenticate but exits after its
controller-runtime cache fails to synchronize:

```text
failed to wait for aks-flex-node-daemon caches to sync
kind source: *v1alpha3.MachineOperation
```

This manual RBAC step is a temporary preview requirement. Remove it from the
operator workflow after the AKS RP release that installs the Flex daemon RBAC
automatically is deployed in the target region.

## 4. Create the FlexNodes pool

> **TODO:** Azure CLI will support creating FlexNodes pools in a future release.
> Replace this preview `az rest` flow with the supported `az aks nodepool`
> command when that capability becomes available.

Use the preview agent-pool ARM API. A FlexNodes pool supports only a minimal set
of properties.

Create the request body:

```bash
cat > /tmp/flex-pool.json <<EOF
{
  "properties": {
    "type": "FlexNodes",
    "mode": "User",
    "orchestratorVersion": "${FLEX_VERSION}",
    "maxPods": 250
  }
}
EOF
```

Create the pool:

```bash
AKS_RESOURCE_ID=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$AKS_NAME" \
  --query id --output tsv)

az rest \
  --method put \
  --uri "https://management.azure.com${AKS_RESOURCE_ID}/agentPools/${FLEX_POOL_NAME}?api-version=2026-05-02-preview" \
  --headers 'Content-Type=application/json' \
  --body "$(cat /tmp/flex-pool.json)"
```

Wait until provisioning succeeds:

```bash
FLEX_POOL_RESOURCE_ID="${AKS_RESOURCE_ID}/agentPools/${FLEX_POOL_NAME}"

az resource wait \
  --ids "$FLEX_POOL_RESOURCE_ID" \
  --api-version 2026-05-02-preview \
  --custom "properties.provisioningState=='Succeeded'" \
  --interval 10 \
  --timeout 900
```

Inspect the completed pool:

```bash
az resource show \
  --ids "$FLEX_POOL_RESOURCE_ID" \
  --api-version 2026-05-02-preview \
  --query '{name:name,state:properties.provisioningState,type:properties.type,version:properties.orchestratorVersion}' \
  --output yaml
```

Do not include normal VMSS properties such as `osType`, `count`, `vmSize`, or
subnet settings. The RP rejects unsupported FlexNodes pool properties.

## 5. Prepare the Flex host and managed identity

The host can be an existing VM or bare-metal machine. This example uses a
user-assigned managed identity on an Azure VM.

Create the identity:

```bash
export FLEX_LOCATION="<host-region>"
export FLEX_IDENTITY_NAME="${AKS_NAME}-flex-identity"

az identity create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEX_IDENTITY_NAME" \
  --location "$FLEX_LOCATION"

FLEX_IDENTITY_ID=$(az identity show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEX_IDENTITY_NAME" \
  --query id --output tsv)

FLEX_IDENTITY_CLIENT_ID=$(az identity show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEX_IDENTITY_NAME" \
  --query clientId --output tsv)

FLEX_IDENTITY_PRINCIPAL_ID=$(az identity show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEX_IDENTITY_NAME" \
  --query principalId --output tsv)
```

Grant the identity AKS Contributor at the cluster scope:

```bash
az role assignment create \
  --assignee-object-id "$FLEX_IDENTITY_PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Kubernetes Service Contributor Role" \
  --scope "$AKS_RESOURCE_ID"
```

Assign this identity to the target VM when creating it or with
`az vm identity assign`. Allow time for role assignment propagation before
running bootstrap.

For example, create an Ubuntu 24.04 VM without cloud-init bootstrap automation:

```bash
export FLEX_VM_NAME="<unique-flex-host-name>"
export FLEX_ADMIN_USER="azureuser"

az vm create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$FLEX_VM_NAME" \
  --location "$FLEX_LOCATION" \
  --size Standard_D4s_v5 \
  --image Canonical:ubuntu-24_04-lts:server:latest \
  --os-disk-size-gb 32 \
  --admin-username "$FLEX_ADMIN_USER" \
  --ssh-key-values "$HOME/.ssh/id_rsa.pub" \
  --subnet "$FLEX_SUBNET_ID" \
  --public-ip-sku Standard \
  --nsg-rule SSH \
  --assign-identity "$FLEX_IDENTITY_ID" \
  --os-disk-delete-option Delete \
  --nic-delete-option Delete
```

Use a 32 GiB or larger OS disk. The validated Azure Linux marketplace image had
a 5 GiB default disk, which failed the agent's 8 GiB free-space preflight until
it was expanded.

Install host prerequisites before running the script.

Ubuntu:

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  curl jq nftables systemd-container tar util-linux
```

Azure Linux 3:

```bash
sudo dnf install -y \
  curl jq nftables systemd-container tar util-linux
```

Do not install Azure CLI on the host.

## 6. Download and run the bootstrap script

SSH to the target host and become root:

```bash
ssh "<admin-user>@<host-address>"
sudo -i
```

Set the operator-provided values:

```bash
export AKS_RESOURCE_ID="<full-aks-resource-id>"
export FLEX_POOL_NAME="aksflexnodes"
export FLEX_IDENTITY_CLIENT_ID="<user-assigned-managed-identity-client-id>"
export AKS_LOCATION="<aks-region>"
export AKS_FLEX_NODE_VERSION="v0.1.5.alpha-9"
```

Select a rootfs matching the host OS.

Ubuntu 24.04:

```bash
export ROOTFS_URL="https://unbounded-azure-mirror-ejd3aeefdrhncchk.b01.azurefd.net/releases/v0.1.24-rc.18/rootfs/rootfs-agent-ubuntu2404-v20260619.oci.tar.gz"
```

Azure Linux 3:

```bash
export ROOTFS_URL="https://unbounded-azure-mirror-ejd3aeefdrhncchk.b01.azurefd.net/releases/v0.1.24-rc.18/rootfs/rootfs-agent-azlinux3-v20260619.oci.tar.gz"
```

Use the Kubernetes-version template for bootstrap artifacts:

```bash
export OFFLINE_ARTIFACTS_URL='https://unbounded-azure-mirror-ejd3aeefdrhncchk.b01.azurefd.net/releases/v0.1.24-rc.18/bootstrap-artifacts/bootstrap-artifacts-k8s-{{ .KubernetesVersion }}.tar.gz'
```

Download the raw script instead of piping it directly to Bash:

```bash
install -d -m 0700 /run/aks-flex-node-bootstrap
printf '{}\n' > /run/aks-flex-node-bootstrap/base-config.json
chmod 0600 /run/aks-flex-node-bootstrap/base-config.json

curl -fsSLo /run/aks-flex-node-bootstrap/bootstrap.sh \
  https://raw.githubusercontent.com/Azure/AKSFlexNode/refs/heads/hbc/install-script/scripts/bootstrap.sh

chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
bash -n /run/aks-flex-node-bootstrap/bootstrap.sh
```

The empty base config is intentional. The command supplies the cluster and pool,
and `--fetch-bootstrap-data` obtains fresh cluster-issued join settings from
AKS RP.

Run bootstrap:

```bash
AKS_FLEX_NODE_BASE_CONFIG_FILE=/run/aks-flex-node-bootstrap/base-config.json \
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth msi \
  --msi-client-id "$FLEX_IDENTITY_CLIENT_ID" \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --resource-manager-endpoint https://management.azure.com \
  --agent-version "$AKS_FLEX_NODE_VERSION" \
  --agent-sha256 042a2a12384637eb13721be04ac5ffa4c884abab50b4baa2fb5cbeba6785ee05 \
  --bootstrap-oci-image "$ROOTFS_URL" \
  --bootstrap-offline-artifacts-source "$OFFLINE_ARTIFACTS_URL" \
  --config-overrides "{
    \"azure\": {
      \"targetCluster\": {
        \"location\": \"${AKS_LOCATION}\"
      }
    },
    \"agent\": {
      \"logLevel\": \"info\",
      \"logDir\": \"/var/log/aks-flex-node\",
      \"machineClient\": {
        \"mode\": \"arm\"
      },
      \"requireMachineRegistration\": true,
      \"machineOperationMode\": \"auto\"
    },
    \"node\": {
      \"labels\": {
        \"aks-flex-node.azure.com/bootstrap-scenario\": \"operator-first-boot\"
      }
    }
  }"
```

The script performs these operations:

1. Loads the empty JSON base.
2. Applies the cluster and pool overrides.
3. Uses MSI to request an ARM token from IMDS.
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
rm -f \
  /run/aks-flex-node-bootstrap/bootstrap.sh \
  /run/aks-flex-node-bootstrap/base-config.json

install -d -m 0755 /var/lib/aks-flex-node
install -m 0600 /dev/null /var/lib/aks-flex-node/first-boot-complete
```

## 7. Approve the daemon CSR when required

The kubelet node-client CSR is normally approved by the cluster bootstrap RBAC.
The long-running Flex daemon CSR requires the AKS Flex CSR approver.

If that controller is not deployed, find the pending CSR and inspect it before
manual approval:

```bash
kubectl get csr

kubectl get csr <csr-name> -o jsonpath='{.spec.request}' \
  | base64 -d \
  | openssl req -noout -subject
```

For the target Node, the daemon CSR should contain:

```text
CN = system:node:<node-name>
O  = system:nodes
O  = aks-flex-node-daemons
```

Approve only the verified daemon CSR:

```bash
kubectl certificate approve <csr-name>
```

Manual approval is a lab fallback. Production environments should deploy the
AKS Flex CSR approver.

## 8. Verify the joined node

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

- the managed identity is assigned to the host;
- AKS Contributor is scoped to the target cluster;
- role assignment propagation has completed;
- `--cluster-resource-id` and `--agent-pool-name` are correct;
- `agent.requireMachineRegistration` is true so failure remains fatal.

### MachineOperation cache synchronization times out

Confirm the temporary RBAC from step 3 exists and that the daemon group can
list/watch MachineOperations:

```bash
kubectl auth can-i list machineoperations.unbounded-cloud.io \
  --as="system:node:<node-name>" \
  --as-group=system:nodes \
  --as-group=aks-flex-node-daemons
```

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
