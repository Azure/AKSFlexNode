# AKS Flex Node With Offline Bootstrap Artifacts

This lab shows how to join a Flex Node when the bootstrap binaries are served from an offline artifact source instead of public upstream URLs. The walkthrough uses a public AKS API server and the [public AKS + unbounded-net + VNet peering lab](aks-public-cluster-unbounded-net-vnet-peering.md) as the network and cluster base, but the same offline artifact flow also works with a private AKS cluster when the target VM can resolve and reach the private API endpoint. The lab changes the Flex VM bootstrap flow to use:

- Host prerequisites installed before the node is isolated.
- A mirrored rootfs OCI image, either as a local OCI layout or in a registry reachable from the target VM.
- A mirrored Unbounded bootstrap artifact bundle, either:
  - as files on the target VM, or
  - as an OCI artifact in a local registry running on the target VM.
- `bootstrap.offlineArtifacts.source` in the AKS Flex Node config.

The goal is to prove that bootstrap does not fetch Kubernetes, CRI, CNI, or crictl artifacts from public upstream endpoints such as `dl.k8s.io` or `github.com/kubernetes-sigs/cri-tools`.

## Prerequisites

Start with the base public VNet-peered lab through these sections:

1. [Create Resource Groups And Networks](aks-public-cluster-unbounded-net-vnet-peering.md#create-resource-groups-and-networks)
2. [Create A Public No-CNI AKS Cluster](aks-public-cluster-unbounded-net-vnet-peering.md#create-a-public-no-cni-aks-cluster)
3. [Install Unbounded-Net](aks-public-cluster-unbounded-net-vnet-peering.md#install-unbounded-net)
4. [Create Sites And Mesh Peering](aks-public-cluster-unbounded-net-vnet-peering.md#create-sites-and-mesh-peering)
5. Create the Flex VM and verify SSH access.

You also need the following tools in the connected preparation environment. The preparation environment can be the target VM before egress is restricted, or a separate staging host that can copy files into the target VM.

- `az`, `kubectl`, `jq`, `ssh`, `scp`
- `aks-flex-node` installed on the target VM
- `oras` for copying OCI images/artifacts and pulling filesystem artifacts.
- `podman` or Docker on the target VM if using the local registry mode.

This lab uses these example artifact versions:

```bash
KUBERNETES_VERSION="1.35.0"
KUBERNETES_VERSION_V="v${KUBERNETES_VERSION#v}"
ROOTFS_IMAGE_UPSTREAM="ghcr.io/azure/agent-ubuntu2404:v20260619"
ARTIFACT_TAG="alpha-0cd4fe2-k8s-${KUBERNETES_VERSION_V}"
ARTIFACT_BUNDLE_UPSTREAM="ghcr.io/azure/unbounded/bootstrap-artifacts:${ARTIFACT_TAG}"
```

Use a bundle tag that matches the Kubernetes version you configure for the Flex Node. AKS Flex Node accepts `components.kubernetes` with or without a leading `v`; the offline artifact template expands `.KubernetesVersion` with the leading `v`. In offline mode, the artifact manifest supplies the artifact versions used by bootstrap.

## Install Host Prerequisites On The Target VM

Offline artifact mode treats missing host packages as fatal during preflight because bootstrap cannot rely on public package repositories after the host is isolated. Install the host prerequisites while the target VM still has package repository access, or install them from your own internal package mirror.

On the target VM:

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  systemd-container \
  curl \
  nftables \
  util-linux
```

Install lab tooling separately. `jq` is used to patch the generated config, and local registry mode needs a container runtime such as `podman`:

```bash
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y jq podman
```

Install `oras` if it is not already available. This lab uses `oras` for both the rootfs image copy and the bootstrap artifact bundle copy/pull, so `skopeo` is not required:

```bash
ORAS_VERSION="1.3.2"
curl -fsSL "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_linux_amd64.tar.gz" \
  -o /tmp/oras.tar.gz
sudo tar -C /usr/local/bin -xzf /tmp/oras.tar.gz oras
oras version
```

## Generate A Baseline AKS Flex Node Config

On your workstation, create the bootstrap-token RBAC and generate a node config. This follows the same pattern as the base lab and quickstart.

```bash
AKS_RG="<aks-resource-group>"
CLUSTER_NAME="<aks-cluster-name>"
SUBSCRIPTION_ID="<subscription-id>"
AGENT_POOL_NAME="${AGENT_POOL_NAME:-aksflexnodes}"

scripts/aks-flex-config setup-node-rbac \
  --resource-group "$AKS_RG" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID"

scripts/aks-flex-config generate-node-config \
  --resource-group "$AKS_RG" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --agent-pool-name "$AGENT_POOL_NAME" \
  --bootstrap-token \
  --output ./aks-flex-node-config.json
```

If the Flex VM has multiple private IPs, or if you want to pin the node IP, set `node.kubelet.nodeIP` in the generated config.

## Option A: Use A Filesystem Bootstrap Artifact Bundle

In filesystem mode, the target VM reads bootstrap artifacts from a local directory or `file://` URL. The rootfs is also staged on the filesystem as a local OCI image layout and referenced with `oci-layout://`.

### Stage The Rootfs Image As A Local OCI Layout

Mirror the rootfs image into a local OCI layout while the VM still has egress, or perform this on a connected staging host and copy the OCI layout directory into the VM.

On the target VM:

```bash
ROOTFS_LAYOUT_DIR="/opt/aks-flex-node/offline/images/agent-ubuntu2404"
ROOTFS_IMAGE_LOCAL="oci-layout://${ROOTFS_LAYOUT_DIR}:v20260619"

sudo mkdir -p "$(dirname "$ROOTFS_LAYOUT_DIR")"
sudo chown "$(id -u):$(id -g)" "$(dirname "$ROOTFS_LAYOUT_DIR")"

oras copy \
  --to-oci-layout \
  "$ROOTFS_IMAGE_UPSTREAM" \
  "${ROOTFS_LAYOUT_DIR}:v20260619"
```

### Pull The Bootstrap Artifact Bundle To Files

Pull the Unbounded bootstrap artifact bundle into the directory that the agent will read. The directory name is versioned so the config can use the `.KubernetesVersion` template value.

```bash
ARTIFACTS_ROOT="/opt/aks-flex-node/offline/bootstrap-artifacts"
ARTIFACTS_DIR="${ARTIFACTS_ROOT}/${KUBERNETES_VERSION_V}"

sudo mkdir -p "$ARTIFACTS_DIR"
sudo chown "$(id -u):$(id -g)" "$ARTIFACTS_DIR"

oras pull \
  --output "$ARTIFACTS_DIR" \
  "$ARTIFACT_BUNDLE_UPSTREAM"

find "$ARTIFACTS_DIR" -maxdepth 4 -type f | sort | head -50
```

The directory should contain `manifest.json` and the paths referenced by that manifest, such as Kubernetes binaries, checksums, containerd, runc, CNI, and crictl artifacts.

### Patch The Config For Filesystem Mode

On your workstation:

```bash
ROOTFS_IMAGE_LOCAL="oci-layout:///opt/aks-flex-node/offline/images/agent-ubuntu2404:v20260619"
ARTIFACTS_SOURCE="file:///opt/aks-flex-node/offline/bootstrap-artifacts/{{ .KubernetesVersion }}"

jq \
  --arg kubernetesVersion "$KUBERNETES_VERSION" \
  --arg rootfsImage "$ROOTFS_IMAGE_LOCAL" \
  --arg offlineSource "$ARTIFACTS_SOURCE" \
  '.components = (.components // {})
   | .components.kubernetes = $kubernetesVersion
   | .bootstrap = (.bootstrap // {})
   | .bootstrap.ociImage = $rootfsImage
   | .bootstrap.offlineArtifacts.source = $offlineSource' \
  ./aks-flex-node-config.json > ./aks-flex-node-config.offline-files.json
```

The relevant fields in the rendered config should look like this:

```json
{
  "components": {
    "kubernetes": "1.35.0"
  },
  "bootstrap": {
    "ociImage": "oci-layout:///opt/aks-flex-node/offline/images/agent-ubuntu2404:v20260619",
    "offlineArtifacts": {
      "source": "file:///opt/aks-flex-node/offline/bootstrap-artifacts/{{ .KubernetesVersion }}"
    }
  }
}
```

Keep the rest of the generated config, including `azure`, `node`, `networking`, and authentication fields.

Copy the config to the target VM:

```bash
TARGET_HOST="<user>@<flex-vm-public-ip>"
scp ./aks-flex-node-config.offline-files.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

## Option B: Use A Local OCI Registry For Rootfs And Bootstrap Artifacts

In OCI registry mode, both the rootfs image and the bootstrap artifact bundle are mirrored into an unauthenticated registry reachable from the target VM. This is usually the best model for larger offline or restricted-egress environments.

### Start The Local Registry

On the target VM:

```bash
sudo podman run -d --name aks-flex-offline-registry --restart=always \
  -p 127.0.0.1:5000:5000 \
  docker.io/library/registry:2

curl -fsS http://127.0.0.1:5000/v2/ >/dev/null
```

### Mirror The Rootfs Image And Bootstrap Artifact Bundle

While the target VM still has egress to the public artifact sources:

```bash
ROOTFS_IMAGE_LOCAL="127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619"
ARTIFACT_BUNDLE_LOCAL="127.0.0.1:5000/aks-flex/bootstrap-artifacts:${ARTIFACT_TAG}"

oras copy \
  --to-plain-http \
  "$ROOTFS_IMAGE_UPSTREAM" \
  "$ROOTFS_IMAGE_LOCAL"

oras copy \
  --to-plain-http \
  "$ARTIFACT_BUNDLE_UPSTREAM" \
  "$ARTIFACT_BUNDLE_LOCAL"
```

If the target VM is never allowed to reach public registries, perform the mirror in a connected staging environment, move the registry contents or exported OCI artifacts into the target network, and restore them into the local registry before continuing.

### Patch The Config For Local Registry Mode

On your workstation:

```bash
ROOTFS_IMAGE_LOCAL="127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619"
ARTIFACTS_SOURCE="oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:alpha-0cd4fe2-k8s-{{ .KubernetesVersion }}"

jq \
  --arg kubernetesVersion "$KUBERNETES_VERSION" \
  --arg rootfsImage "$ROOTFS_IMAGE_LOCAL" \
  --arg offlineSource "$ARTIFACTS_SOURCE" \
  '.components = (.components // {})
   | .components.kubernetes = $kubernetesVersion
   | .bootstrap = (.bootstrap // {})
   | .bootstrap.ociImage = $rootfsImage
   | .bootstrap.offlineArtifacts.source = $offlineSource' \
  ./aks-flex-node-config.json > ./aks-flex-node-config.offline-registry.json
```

The relevant fields in the rendered config should look like this:

```json
{
  "components": {
    "kubernetes": "1.35.0"
  },
  "bootstrap": {
    "ociImage": "127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619",
    "offlineArtifacts": {
      "source": "oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:alpha-0cd4fe2-k8s-{{ .KubernetesVersion }}"
    }
  }
}
```

Keep the rest of the generated config, including `azure`, `node`, `networking`, and authentication fields.

Copy the config to the target VM:

```bash
TARGET_HOST="<user>@<flex-vm-public-ip>"
scp ./aks-flex-node-config.offline-registry.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

## Optional: Restrict Egress After Staging

After the host packages, rootfs image, and bootstrap artifact bundle are staged locally, restrict egress using your preferred mechanism, such as NSG rules, Azure Firewall, UDRs, or private-only routing.

The target VM still needs to reach:

- The AKS API server over HTTPS 443.
- Any control-plane or node networking paths required by the base VNet peering lab.
- The local registry on `127.0.0.1:5000` if using local registry mode.
- The local rootfs OCI layout path if using filesystem mode.

For a strict validation, block public artifact endpoints such as:

```text
dl.k8s.io
github.com
objects.githubusercontent.com
github-releases.githubusercontent.com
pkg-containers.githubusercontent.com
ghcr.io
storage.googleapis.com
packages.microsoft.com
mcr.microsoft.com
registry.k8s.io
```

### Preflight-Only Host-Level Block With nftables

For a quick preflight-only sanity check, add temporary host firewall rules on the target VM after all packages, images, and artifacts are staged. This blocks the currently resolved IPs for common public artifact hosts while leaving the AKS API server and the loopback registry reachable.

Do not rely on these host-level `nftables` rules as the strict bootstrap isolation mechanism. During `start`, AKS Flex Node installs and starts an nftables reset unit that runs `flush ruleset`, so rules created before bootstrap are intentionally removed. Use the NSG or Azure Firewall option below for isolation that remains in effect during bootstrap.

```bash
BLOCKED_ARTIFACT_HOSTS=(
  dl.k8s.io
  github.com
  objects.githubusercontent.com
  github-releases.githubusercontent.com
  pkg-containers.githubusercontent.com
  ghcr.io
  storage.googleapis.com
  packages.microsoft.com
  mcr.microsoft.com
  registry.k8s.io
)

sudo nft add table inet aksflex_offline 2>/dev/null || true
sudo nft 'add set inet aksflex_offline blocked_v4 { type ipv4_addr; flags interval; }' 2>/dev/null || true
sudo nft 'add chain inet aksflex_offline output { type filter hook output priority 0; policy accept; }' 2>/dev/null || true
sudo nft 'add rule inet aksflex_offline output ip daddr @blocked_v4 tcp dport { 80, 443 } reject' 2>/dev/null || true

for host in "${BLOCKED_ARTIFACT_HOSTS[@]}"; do
  getent ahostsv4 "$host" | awk '{print $1}' | sort -u | while read -r ip; do
    sudo nft add element inet aksflex_offline blocked_v4 "{ ${ip} }" 2>/dev/null || true
  done
done

sudo nft list table inet aksflex_offline
```

These rules are intentionally temporary and IP-based. Public artifact hostnames often use CDNs, so DNS answers can change. Re-run the resolver loop if the validation runs much later. For strict bootstrap validation, use subnet-level controls such as NSG rules or Azure Firewall application rules.

Remove the temporary rules with:

```bash
sudo nft delete table inet aksflex_offline
```

### Subnet-Level Block With NSG Rules

For stronger validation in Azure, use NSG rules after staging. NSGs cannot deny arbitrary FQDNs, so the strict pattern is to allow only required destinations, then deny outbound Internet. For a public AKS API server, resolve the API FQDN and allow that IP on TCP 443 before adding the deny rule.

```bash
VM_RG="<vm-resource-group>"
NSG_NAME="<flex-vm-nsg-name>"
AKS_API_FQDN="$(az aks show -g "$AKS_RG" -n "$CLUSTER_NAME" --query fqdn -o tsv)"
AKS_API_IP="$(getent ahostsv4 "$AKS_API_FQDN" | awk 'NR==1 {print $1}')"

az network nsg rule create \
  -g "$VM_RG" \
  --nsg-name "$NSG_NAME" \
  -n allow-aks-api \
  --priority 300 \
  --direction Outbound \
  --access Allow \
  --protocol Tcp \
  --source-address-prefixes '*' \
  --destination-address-prefixes "$AKS_API_IP" \
  --destination-port-ranges 443

az network nsg rule create \
  -g "$VM_RG" \
  --nsg-name "$NSG_NAME" \
  -n deny-internet-outbound \
  --priority 4000 \
  --direction Outbound \
  --access Deny \
  --protocol '*' \
  --source-address-prefixes '*' \
  --destination-address-prefixes Internet \
  --destination-port-ranges '*'
```

If you are using a private AKS cluster, allow the private API endpoint and required VNet/private endpoint ranges instead of the public API IP. If your validation needs durable FQDN allow/deny controls instead of resolved IPs, use Azure Firewall application rules plus a route table that sends VM subnet egress through the firewall.

## Install The Config And Run Preflight

On the target VM:

```bash
sudo install -d -m 0755 /etc/aks-flex-node
sudo install -m 0600 /tmp/aks-flex-node-config.json /etc/aks-flex-node/config.json

sudo aks-flex-node preflight --config /etc/aks-flex-node/config.json
```

Preflight should report successful host, API server, rootfs image, and artifact checks. It also reports a warning that node-problem-detector is disabled while offline artifacts are configured; this is temporary until NPD is included in upstream Unbounded bootstrap artifacts. In offline artifact mode, preflight is also the point where missing host packages are detected before bootstrap mutates the machine.

For automation, use JSON output:

```bash
sudo aks-flex-node preflight --config /etc/aks-flex-node/config.json --output json
```

## Bootstrap The Node

Run bootstrap from the target VM:

```bash
sudo sh -c 'umask 022; aks-flex-node start --config /etc/aks-flex-node/config.json'
```

Verify the node from your workstation:

```bash
kubectl get nodes -o wide
kubectl describe node <flex-node-name>
```

## Validate That Offline Artifacts Were Used

On the target VM, inspect the agent log:

```bash
sudo grep -E 'pulling OCI image|downloading (kubernetes|cri-tools)|bootstrap-artifacts|dl.k8s.io|kubernetes-sigs' \
  /var/log/aks-flex-node/aks-flex-node.log
```

Expected evidence for local registry mode includes URLs like:

```text
pulling OCI image image=127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619
downloading kubernetes binary url=oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:alpha-0cd4fe2-k8s-v1.35.0#kubernetes/v1.35.0/bin/linux/amd64/kubelet
```

Expected evidence for filesystem mode includes log lines and URLs like:

```text
using local OCI layout image image=oci-layout:///opt/aks-flex-node/offline/images/agent-ubuntu2404:v20260619 layout=/opt/aks-flex-node/offline/images/agent-ubuntu2404
downloading kubernetes binary url=file:///opt/aks-flex-node/offline/bootstrap-artifacts/v1.35.0/kubernetes/v1.35.0/bin/linux/amd64/kubelet
```

There should be no bootstrap downloads from public upstream artifact URLs:

```bash
sudo grep -E 'https://dl.k8s.io|https://github.com/kubernetes-sigs/cri-tools' \
  /var/log/aks-flex-node/aks-flex-node.log && echo "unexpected public artifact download"
```

## Troubleshooting

### Preflight Fails `host-packages`

Install the required host packages before isolating the VM from package repositories:

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y systemd-container curl nftables util-linux
```

### Preflight Fails `oci-image-reachable`

For filesystem mode, check that the local OCI layout exists and has the expected tag:

```bash
test -f /opt/aks-flex-node/offline/images/agent-ubuntu2404/oci-layout
oras manifest fetch --oci-layout /opt/aks-flex-node/offline/images/agent-ubuntu2404:v20260619 >/dev/null
```

For local registry mode, check the rootfs image reference and local registry health:

```bash
curl -fsS http://127.0.0.1:5000/v2/ >/dev/null
oras manifest fetch --plain-http 127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619 >/dev/null
```

### Preflight Fails Artifact Checks

For local registry mode, verify the OCI artifact is present:

```bash
oras manifest fetch --plain-http "127.0.0.1:5000/aks-flex/bootstrap-artifacts:${ARTIFACT_TAG}" >/dev/null
```

For filesystem mode, verify `manifest.json` and expected artifact paths exist:

```bash
sudo test -f "/opt/aks-flex-node/offline/bootstrap-artifacts/${KUBERNETES_VERSION_V}/manifest.json"
sudo find "/opt/aks-flex-node/offline/bootstrap-artifacts/${KUBERNETES_VERSION_V}" -maxdepth 4 -type f | sort | head -50
```
