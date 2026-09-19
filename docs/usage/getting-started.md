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
3. Apply temporary daemon permissions when the cluster doesn't provide them.
4. Create a Flex node pool.
5. Prepare a host and authorize its Azure Arc managed identity, Azure VM managed identity, or service principal at the AKS cluster scope.
6. Download and run the versioned bootstrap script.
7. Approve the daemon certificate signing request (CSR) when no approver is deployed.
8. Verify the Azure Machine, Kubernetes Node, networking, workload connectivity, and agent service.

## Prerequisites

The Bash environment needs:

- Azure CLI 2.90.0 or later, authenticated to the target subscription;
- `aks-preview` Azure CLI extension `22.0.0b8` or later;
- `kubectl`, `curl`, `tar`, and an OpenSSH client;
- permission to create AKS and networking resources, register preview features, create a Flex node pool, and grant the selected host identity access to the AKS cluster;
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

Before bootstrap, select one Azure host identity and grant it the required role at the target AKS cluster scope. Use Azure VM managed identity for an Azure VM, Azure Arc managed identity for an Arc-enabled server, or a service principal when managed identity isn't available. Prefer a certificate over a long-lived service principal secret.

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
export CENTRAL_ARTIFACTS_ENDPOINT="https://unbounded-azure-mirror-ejd3aeefdrhncchk.b01.azurefd.net"

# bootstrap.sh downloads the agent, rootfs, and Kubernetes bootstrap bundle
# from this central artifact endpoint.
export AKS_FLEX_NODE_AGENT_URL="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/aks-flex-node/${AKS_FLEX_NODE_VERSION}/{{ARCHIVE_NAME}}"
# For an NVIDIA GPU host, use rootfs-agent-ubuntu2404-nvidia-v20260619.oci.tar.gz.
export BOOTSTRAP_OCI_IMAGE="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/${UNBOUNDED_VERSION}/rootfs/rootfs-agent-ubuntu2404-v20260619.oci.tar.gz"
export BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE="${CENTRAL_ARTIFACTS_ENDPOINT}/releases/${UNBOUNDED_VERSION}/bootstrap-artifacts/bootstrap-artifacts-k8s-{{ .KubernetesVersion }}.tar.gz"
```

`FLEX_VERSION` defaults to the AKS control-plane version so the new FlexNodes
pool is aligned with the cluster. Override it only when intentionally using an
AKS-supported version skew. Host bootstrap does not need a separate version
flag; `listBootstrapData` returns the pool's accepted full patch version and the
artifact template resolves from that value.

The cluster and Flex host networks must have private L3 connectivity. Use one
routed VNet, VNet peering, VPN, ExpressRoute, or an equivalent network design.
The CIDRs above must not overlap.

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

## 3. Install temporary AKS Flex daemon RBAC

> [!IMPORTANT]
> **Temporary preview requirement:** When the Unbounded MachineOperation CRD is
> installed, AKS Flex Node discovers it and enables its MachineOperation
> reconciler. A future AKS RP release will install and manage the required
> ClusterRole and ClusterRoleBinding automatically as part of FlexNodes pool
> setup. Until that release is deployed in the target region, operators must
> apply the temporary RBAC below manually.

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
  - get
  - patch
  - update
- apiGroups:
  - ""
  resources:
  - nodes
  verbs:
  - get
  - list
  - watch
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

A Flex node pool is a logical group for customer-provided compute. Don't configure standard virtual machine scale set properties such as node count, VM size, operating system type, or subnet settings.

Wait for any cluster operation started by extension or policy reconciliation to finish, and then create the pool with the preview Azure CLI extension:

```bash
while STATUS=$(az aks operation show-latest \
    --resource-group "$RESOURCE_GROUP" --name "$AKS_NAME" \
    --query status --output tsv 2>/dev/null) && \
    [[ "$STATUS" == "InProgress" || "$STATUS" == "Running" ]]; do
  sleep 15
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

## 5. Prepare the Flex host and Azure identity

Each flex node host needs an Azure identity so the agent can create and continuously read its Azure Machine resource. Select one identity mode:

- **Azure Arc managed identity** for a server that is already connected to Azure Arc. Flex Node uses the existing Arc identity but doesn't manage the Arc agent or resource lifecycle.
- **Azure VM managed identity** for an Azure VM. Use the system-assigned identity, or provide the client ID of a user-assigned identity.
- **Service principal** for a host that can't use either managed identity path. Prefer a certificate and private key over a long-lived client secret, and deliver the credential through a protected file.

The selected identity must have **Azure Kubernetes Service Contributor Role** at the target AKS cluster resource scope. The operator is responsible for provisioning or enabling the identity, associating it with the host when applicable, granting the role, and allowing role-assignment propagation to complete before bootstrap starts.

Set the principal object ID for the selected identity, create the cluster-scoped assignment, and verify it from your Bash environment:

```bash
HOST_PRINCIPAL_OBJECT_ID="<managed-identity-or-service-principal-object-id>"

az role assignment create \
  --assignee-object-id "$HOST_PRINCIPAL_OBJECT_ID" \
  --assignee-principal-type ServicePrincipal \
  --role "Azure Kubernetes Service Contributor Role" \
  --scope "$AKS_RESOURCE_ID" \
  --output none

az role assignment list \
  --assignee "$HOST_PRINCIPAL_OBJECT_ID" \
  --scope "$AKS_RESOURCE_ID" \
  --query '[].{role:roleDefinitionName,scope:scope}' \
  --output table
```

Continue when the expected role and cluster scope appear. Allow time for the assignment to propagate before bootstrap.

Host and identity provisioning are otherwise outside this guide. Start with
a prepared Ubuntu host that can reach the AKS API server and artifact endpoints.
Use a 32 GiB or larger OS disk; the agent preflight requires at least 8 GiB free
under `/var/lib`.

Install the host prerequisites:

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  curl \
  jq \
  nftables \
  systemd-container \
  tar \
  util-linux
```

Azure CLI doesn't need to be installed on the host. The bootstrap workflow authenticates through Azure Arc, Azure Instance Metadata Service, or service principal OAuth directly.

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
export AKS_FLEX_NODE_VERSION="v0.2.0"
export UNBOUNDED_VERSION="v0.8.0"
export CENTRAL_ARTIFACTS_ENDPOINT="https://unbounded-azure-mirror-ejd3aeefdrhncchk.b01.azurefd.net"

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
https://unbounded-azure-mirror-ejd3aeefdrhncchk.b01.azurefd.net/releases/aks-flex-node/v0.2.0/aks-flex-node-linux-amd64.tar.gz
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
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/bootstrap.sh"

chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
bash -n /run/aks-flex-node-bootstrap/bootstrap.sh
```

The version-matched repository script contains an unpopulated embedded-config marker. When `--fetch-bootstrap-data`, `--cluster-resource-id`, and `--agent-pool-name` are provided together, the script starts from an empty config and obtains fresh cluster-issued join settings from AKS. No base config file is required.

Complete only the command for the identity selected in the preceding section.

For an Azure Arc-enabled server, first confirm that `azcmagent show` reports `Connected`, `himdsd.service` is active, and the Arc machine identity has access to the target AKS cluster. Then run:

```bash
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth arc \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-url "$AKS_FLEX_NODE_AGENT_URL" \
  --bootstrap-oci-image "$BOOTSTRAP_OCI_IMAGE" \
  --bootstrap-offline-artifacts-source "$BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE"
```

For a service principal, install the credential from the operator's protected secret delivery path. Keep this file available after bootstrap because the running agent uses it for Azure Machine reconciliation:

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
3. Uses the selected Azure Arc managed identity, Azure VM managed identity, or service principal to request an ARM token.
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

## 7. Approve the daemon CSR when required

> [!IMPORTANT]
> **TODO:** A future AKS RP release will automate the Flex daemon CSR approval
> flow. Until that release is deployed in the target region, operators must
> inspect and approve the daemon CSR manually when the AKS Flex CSR approver is
> not present.

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
