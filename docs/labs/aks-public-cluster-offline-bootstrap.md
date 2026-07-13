# AKS Flex Node With Offline Bootstrap Artifacts

This lab shows how to join a Flex Node when the bootstrap binaries are served from an offline artifact source instead of public upstream URLs. The walkthrough uses a public AKS API server and the [public AKS + unbounded-net + VNet peering lab](aks-public-cluster-unbounded-net-vnet-peering.md) as the network and cluster base, but the same offline artifact flow also works with a private AKS cluster when the target VM can resolve and reach the private API endpoint. The lab changes the Flex VM bootstrap flow to use:

- Host prerequisites installed before the node is isolated.
- A mirrored rootfs OCI image, either as a local OCI layout or in a registry reachable from the target VM.
- A mirrored Unbounded bootstrap artifact bundle, either:
  - as files on the target VM, or
  - as an OCI artifact in a local registry running on the target VM.
- `bootstrap.offlineArtifacts.source` in the AKS Flex Node config.
- Site-scoped Unbounded-Net DaemonSets so the AKS site keeps using GHCR while the offline Flex site pulls mirrored runtime images from the VM-local registry.
- An NSG allowlist that remains active throughout bootstrap and denies all other outbound destinations.

The goal is to prove that bootstrap does not fetch Kubernetes, CRI, CNI, crictl, or required Flex-site runtime images from public upstream endpoints such as `dl.k8s.io`, GHCR, or MCR. The local registry in this lab intentionally uses plain HTTP on loopback for short-lived testing. Because the Flex worker's nspawn machine shares the host network namespace, containerd can reach that registry after a registry-specific `hosts.toml` is installed in the active nspawn rootfs. This override is a temporary lab mechanism, not a recommended production registry configuration.

## Prerequisites

Start with the base public VNet-peered lab through these sections:

1. [Create Resource Groups And Networks](aks-public-cluster-unbounded-net-vnet-peering.md#create-resource-groups-and-networks)
2. [Create A Public No-CNI AKS Cluster](aks-public-cluster-unbounded-net-vnet-peering.md#create-a-public-no-cni-aks-cluster)
3. [Create The Flex VM](aks-public-cluster-unbounded-net-vnet-peering.md#create-the-flex-vm) and verify SSH access.

Do not apply the base lab's unrestricted `unbounded-net-node` DaemonSet. This lab installs the Unbounded-Net controller and shared RBAC but replaces that DaemonSet with separate `aks-site` and `flex-site` DaemonSets. This prevents the offline Flex node from trying to pull the public GHCR node image after it joins.

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
ARTIFACT_TAG="v20260708-k8s-${KUBERNETES_VERSION_V}"
ARTIFACT_BUNDLE_UPSTREAM="ghcr.io/azure/unbounded/bootstrap-artifacts:${ARTIFACT_TAG}"
UNBOUNDED_VERSION="v0.1.10"
UNBOUNDED_NODE_IMAGE_UPSTREAM="ghcr.io/azure/unbounded-net-node:${UNBOUNDED_VERSION}"
```

Use a bundle tag that matches the Kubernetes version you configure for the Flex Node. AKS Flex Node accepts `components.kubernetes` with or without a leading `v`; the offline artifact template expands `.KubernetesVersion` with the leading `v`. In offline mode, the artifact manifest supplies the artifact versions used by bootstrap.

## Install Unbounded-Net With Site-Scoped Node DaemonSets

On the workstation, render Unbounded-Net but do not apply its unrestricted node DaemonSet:

```bash
rm -rf /tmp/unbounded
git clone --depth 1 --branch "$UNBOUNDED_VERSION" \
  https://github.com/Azure/unbounded.git /tmp/unbounded

cd /tmp/unbounded
make VERSION="$UNBOUNDED_VERSION" net-manifests
```

Create a Kustomize base and an AKS-site overlay. The site label is added to both the DaemonSet selector and its node selector so the public image can only run on AKS-site nodes:

```bash
SPLIT_DIR="/tmp/unbounded/deploy/net/site-split"
mkdir -p "$SPLIT_DIR/base" "$SPLIT_DIR/aks-site" "$SPLIT_DIR/flex-site"
cp deploy/net/rendered/node/03-daemonset.yaml "$SPLIT_DIR/base/daemonset.yaml"

cat > "$SPLIT_DIR/base/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- daemonset.yaml
EOF

cat > "$SPLIT_DIR/aks-site/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../base
nameSuffix: -aks-site
patches:
- target:
    group: apps
    version: v1
    kind: DaemonSet
    name: unbounded-net-node
  patch: |-
    - op: add
      path: /metadata/labels/net.unbounded-cloud.io~1site
      value: aks-site
    - op: add
      path: /spec/selector/matchLabels/net.unbounded-cloud.io~1site
      value: aks-site
    - op: add
      path: /spec/template/metadata/labels/net.unbounded-cloud.io~1site
      value: aks-site
    - op: add
      path: /spec/template/spec/nodeSelector
      value:
        net.unbounded-cloud.io/site: aks-site
EOF

kubectl kustomize "$SPLIT_DIR/aks-site" > "$SPLIT_DIR/daemonset-aks-site.yaml"
```

Apply the controller, CRDs, shared node RBAC, and only the AKS-site node DaemonSet:

```bash
cd /tmp/unbounded
kubectl apply --server-side --force-conflicts -f deploy/net/rendered/00-namespace.yaml
kubectl apply --server-side --force-conflicts -f deploy/net/rendered/01-configmap.yaml
kubectl apply --server-side --force-conflicts -f deploy/net/rendered/crd/
kubectl apply --server-side --force-conflicts -f deploy/net/rendered/controller/
kubectl apply --server-side --force-conflicts -f deploy/net/rendered/node/01-serviceaccount.yaml
kubectl apply --server-side --force-conflicts -f deploy/net/rendered/node/02-rbac.yaml
kubectl -n unbounded-net delete ds unbounded-net-node --ignore-not-found
kubectl apply --server-side --force-conflicts -f "$SPLIT_DIR/daemonset-aks-site.yaml"

kubectl -n unbounded-net rollout status deploy/unbounded-net-controller --timeout=5m
```

Create the sites and private-L3 peering from the base lab:

```bash
kubectl apply -f - <<'EOF'
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: aks-site
spec:
  nodeCidrs:
  - 10.91.0.0/16
  podCidrAssignments:
  - assignmentEnabled: true
    cidrBlocks:
    - 10.93.0.0/16
  manageCniPlugin: true
---
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: Site
metadata:
  name: flex-site
spec:
  nodeCidrs:
  - 10.92.0.0/16
  podCidrAssignments:
  - assignmentEnabled: true
    cidrBlocks:
    - 10.95.0.0/16
  manageCniPlugin: true
---
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: aks-flex-private-l3
spec:
  sites:
  - aks-site
  - flex-site
  meshNodes: true
  tunnelProtocol: Auto
EOF

until kubectl get nodes -l net.unbounded-cloud.io/site=aks-site -o name | grep -q .; do
  sleep 2
done
kubectl -n unbounded-net rollout status ds/unbounded-net-node-aks-site --timeout=5m
kubectl get nodes -L net.unbounded-cloud.io/site -o wide
```

At this point the AKS node uses the public GHCR image. Do not create the Flex-site node DaemonSet yet; it is applied after the Flex node's containerd is configured for the local HTTP registry.

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

This mode makes bootstrap offline, but a permanently locked-down Flex node still needs a source for the Unbounded-Net and kube-proxy container images. Use the local registry runtime-image steps from Option B, or preload those images into containerd through another controlled mechanism.

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

## Option B: Use A Local OCI Registry For Bootstrap And Flex-Site Runtime Artifacts

In OCI registry mode, the rootfs image, bootstrap artifact bundle, Unbounded-Net node image, and Flex-site kube-proxy image are mirrored into an unauthenticated registry on the target VM. The first two are read by the host bootstrap process. The runtime images are pulled by containerd inside the nspawn worker after its loopback registry configuration is installed.

### Start The Local Registry

On the target VM:

```bash
sudo podman run -d --name aks-flex-offline-registry --restart=always \
  --network host \
  -e REGISTRY_HTTP_ADDR=127.0.0.1:5000 \
  docker.io/library/registry:2

curl -fsS http://127.0.0.1:5000/v2/ >/dev/null
```

### Mirror Bootstrap And Flex-Site Runtime Artifacts

On the workstation, obtain the exact kube-proxy image selected by the Unbounded-Net controller:

```bash
KUBE_PROXY_IMAGE_UPSTREAM="$(kubectl -n unbounded-net get ds unbounded-net-kube-proxy-flex-site \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="kube-proxy")].image}')"
echo "$KUBE_PROXY_IMAGE_UPSTREAM"
```

While the target VM still has egress to the public artifact sources, mirror all required content:

```bash
KUBE_PROXY_IMAGE_UPSTREAM="<value printed by the workstation command above>"
ROOTFS_IMAGE_LOCAL="127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619"
ARTIFACT_BUNDLE_LOCAL="127.0.0.1:5000/aks-flex/bootstrap-artifacts:${ARTIFACT_TAG}"
UNBOUNDED_NODE_IMAGE_LOCAL="127.0.0.1:5000/offline/unbounded-net-node:${UNBOUNDED_VERSION}"
KUBE_PROXY_IMAGE_LOCAL="127.0.0.1:5000/offline/kube-proxy:${KUBERNETES_VERSION_V}"

oras copy --to-plain-http "$ROOTFS_IMAGE_UPSTREAM" "$ROOTFS_IMAGE_LOCAL"
oras copy --to-plain-http "$ARTIFACT_BUNDLE_UPSTREAM" "$ARTIFACT_BUNDLE_LOCAL"
oras copy --to-plain-http "$UNBOUNDED_NODE_IMAGE_UPSTREAM" "$UNBOUNDED_NODE_IMAGE_LOCAL"
oras copy --to-plain-http "$KUBE_PROXY_IMAGE_UPSTREAM" "$KUBE_PROXY_IMAGE_LOCAL"

for ref in \
  "$ROOTFS_IMAGE_LOCAL" \
  "$ARTIFACT_BUNDLE_LOCAL" \
  "$UNBOUNDED_NODE_IMAGE_LOCAL" \
  "$KUBE_PROXY_IMAGE_LOCAL"; do
  oras manifest fetch --plain-http "$ref" >/dev/null
  echo "mirrored $ref"
done
```

The offline bundle manifest should include the sandbox image archive required by containerd. Verify it before lockdown:

```bash
rm -rf /tmp/bootstrap-artifacts-check
oras pull --plain-http \
  --output /tmp/bootstrap-artifacts-check \
  "$ARTIFACT_BUNDLE_LOCAL"

jq '.containerImages' /tmp/bootstrap-artifacts-check/manifest.json
```

If the target VM is never allowed to reach public registries, perform the mirror in a connected staging environment, move the registry contents or exported OCI artifacts into the target network, and restore them into the local registry before continuing.

### Patch The Config For Local Registry Mode

On your workstation:

```bash
ROOTFS_IMAGE_LOCAL="127.0.0.1:5000/aks-flex/rootfs/agent-ubuntu2404:v20260619"
ARTIFACTS_SOURCE="oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:v20260708-k8s-{{ .KubernetesVersion }}"

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
      "source": "oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:v20260708-k8s-{{ .KubernetesVersion }}"
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

## Restrict Egress After Staging

After the host packages, bootstrap content, and Flex-site runtime images are staged locally, apply the outbound lockdown before running preflight or bootstrap. Use NSG rules, Azure Firewall, UDRs, or equivalent subnet-level controls that remain active while `aks-flex-node start` runs.

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

### Persistent Lockdown With NSG Rules

NSGs cannot filter arbitrary FQDNs. For a strict lab validation, allow only the AKS API, the AKS VNet underlay, and required Azure platform addresses, then deny every other destination. A final `Deny *` is stricter than denying only the `Internet` service tag, which can have unintuitive behavior for public addresses hosted in Azure.

Confirm that the NSG is associated with the Flex VM NIC or subnet before relying on these rules. The following creates and attaches a NIC-level NSG when the VM does not already have one:

```bash
VM_RG="<vm-resource-group>"
VM_NAME="<flex-vm-name>"
NSG_NAME="<flex-vm-nsg-name>"
NIC_ID="$(az vm show -g "$VM_RG" -n "$VM_NAME" \
  --query 'networkProfile.networkInterfaces[0].id' -o tsv)"

if [[ -z "$(az network nic show --ids "$NIC_ID" --query networkSecurityGroup.id -o tsv)" ]]; then
  az network nsg create -g "$VM_RG" -n "$NSG_NAME"
  az network nic update --ids "$NIC_ID" --network-security-group "$NSG_NAME"
fi

az network nic list-effective-nsg --ids "$NIC_ID" -o table
```

Resolve every current IPv4 address for the public AKS API and add them to the allow rule:

```bash
AKS_VNET_CIDR="10.91.0.0/16"
AKS_API_FQDN="$(az aks show -g "$AKS_RG" -n "$CLUSTER_NAME" --query fqdn -o tsv)"
mapfile -t AKS_API_IPS < <(getent ahostsv4 "$AKS_API_FQDN" | awk '{print $1}' | sort -u)

az network nsg rule create \
  -g "$VM_RG" --nsg-name "$NSG_NAME" -n allow-aks-api \
  --priority 300 --direction Outbound --access Allow --protocol Tcp \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes "${AKS_API_IPS[@]}" \
  --destination-port-ranges 443

az network nsg rule create \
  -g "$VM_RG" --nsg-name "$NSG_NAME" -n allow-aks-vnet \
  --priority 310 --direction Outbound --access Allow --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes "$AKS_VNET_CIDR" \
  --destination-port-ranges '*'

# Azure DNS and VM agent wire-server endpoint.
az network nsg rule create \
  -g "$VM_RG" --nsg-name "$NSG_NAME" -n allow-azure-platform \
  --priority 320 --direction Outbound --access Allow --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes 168.63.129.16 \
  --destination-port-ranges '*'

# Azure Instance Metadata Service.
az network nsg rule create \
  -g "$VM_RG" --nsg-name "$NSG_NAME" -n allow-imds \
  --priority 330 --direction Outbound --access Allow --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes 169.254.169.254 \
  --destination-port-ranges '*'

az network nsg rule create \
  -g "$VM_RG" --nsg-name "$NSG_NAME" -n deny-all-egress \
  --priority 4000 --direction Outbound --access Deny --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes '*' \
  --destination-port-ranges '*'
```

The local registry uses loopback and does not traverse the NSG. Preserve any additional explicit allows required by your Azure environment. Public AKS API addresses can change; this IP allowlist is suitable for a bounded lab. Use Azure Firewall FQDN rules for a durable public-API deployment. For a private AKS cluster, allow the private API and required VNet/private-endpoint ranges instead.

After NSG propagation, verify from the target VM that the API and local registry remain reachable while public registries are blocked:

```bash
curl -k -sS --connect-timeout 10 -o /dev/null -w 'AKS API: %{http_code}\n' \
  "https://${AKS_API_FQDN}:443"
curl -fsS http://127.0.0.1:5000/v2/ >/dev/null

for url in https://dl.k8s.io https://ghcr.io https://mcr.microsoft.com https://github.com; do
  if curl -fsS --connect-timeout 5 --max-time 8 "$url" >/dev/null; then
    echo "unexpectedly reachable: $url"
    exit 1
  fi
  echo "blocked: $url"
done
```

An unauthenticated AKS API response is normally HTTP 401 and proves network reachability.

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

The node can register before the site-specific networking pods run. In local registry mode, configure the active nspawn machine and deploy those pods next.

## Configure Nspawn Containerd For The Temporary Local HTTP Registry

> **Testing only:** The `hosts.toml` below permits plain-HTTP image pulls from a loopback registry. Use it only for this isolated, short-lived lab. Production environments should use an HTTPS registry whose certificate is already trusted by the nspawn rootfs, avoiding this per-machine override entirely.

The nspawn worker shares the host network namespace, including loopback, but it has its own filesystem and containerd configuration. Confirm the namespace relationship and registry reachability on the target VM:

```bash
MACHINE="$(machinectl list --no-legend --no-pager | awk '$1 ~ /^kube[12]$/ {print $1; exit}')"
LEADER="$(machinectl show "$MACHINE" -p Leader --value)"

echo "host netns:    $(readlink /proc/1/ns/net)"
echo "machine netns: $(readlink "/proc/${LEADER}/ns/net")"

sudo nsenter -t "$LEADER" -n -m -u -i \
  curl -fsS http://127.0.0.1:5000/v2/ >/dev/null
```

The two namespace identifiers should match. Containerd is already configured with `config_path = "/etc/containerd/certs.d"`; add only the loopback registry's plain-HTTP resolver configuration to the active rootfs:

```bash
CERTS_DIR="/var/lib/machines/${MACHINE}/etc/containerd/certs.d/127.0.0.1:5000"
sudo install -d -m 0755 "$CERTS_DIR"
sudo tee "$CERTS_DIR/hosts.toml" >/dev/null <<'EOF'
server = "http://127.0.0.1:5000"

[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]
EOF

sudo systemctl -M "$MACHINE" restart containerd
sudo systemctl -M "$MACHINE" is-active containerd
```

Validate pulls through CRI before creating the Flex-site DaemonSets:

```bash
UNBOUNDED_NODE_IMAGE_LOCAL="127.0.0.1:5000/offline/unbounded-net-node:${UNBOUNDED_VERSION}"
KUBE_PROXY_IMAGE_LOCAL="127.0.0.1:5000/offline/kube-proxy:${KUBERNETES_VERSION_V}"

sudo machinectl shell "$MACHINE" /usr/local/bin/crictl pull "$UNBOUNDED_NODE_IMAGE_LOCAL"
sudo machinectl shell "$MACHINE" /usr/local/bin/crictl pull "$KUBE_PROXY_IMAGE_LOCAL"
```

This temporary file belongs only to the active `kube1` or `kube2` rootfs and is not durable machine configuration. A blue-green repave does not carry it to the new side. Recreate it only when repeating this HTTP-based test; do not automate the insecure override for production. Use a trusted HTTPS registry instead.

## Deploy The Offline Flex-Site Networking DaemonSets

On the workstation, render the Flex-site Unbounded-Net DaemonSet from the same base used for the public site:

```bash
SPLIT_DIR="/tmp/unbounded/deploy/net/site-split"
UNBOUNDED_VERSION="v0.1.10"
UNBOUNDED_NODE_IMAGE_LOCAL="127.0.0.1:5000/offline/unbounded-net-node:${UNBOUNDED_VERSION}"
KUBE_PROXY_IMAGE_LOCAL="127.0.0.1:5000/offline/kube-proxy:${KUBERNETES_VERSION_V}"

cat > "$SPLIT_DIR/flex-site/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- ../base
nameSuffix: -flex-site
images:
- name: ghcr.io/azure/unbounded-net-node
  newName: 127.0.0.1:5000/offline/unbounded-net-node
  newTag: ${UNBOUNDED_VERSION}
patches:
- target:
    group: apps
    version: v1
    kind: DaemonSet
    name: unbounded-net-node
  patch: |-
    - op: add
      path: /metadata/labels/net.unbounded-cloud.io~1site
      value: flex-site
    - op: add
      path: /spec/selector/matchLabels/net.unbounded-cloud.io~1site
      value: flex-site
    - op: add
      path: /spec/template/metadata/labels/net.unbounded-cloud.io~1site
      value: flex-site
    - op: add
      path: /spec/template/spec/nodeSelector
      value:
        net.unbounded-cloud.io/site: flex-site
EOF

kubectl kustomize "$SPLIT_DIR/flex-site" > "$SPLIT_DIR/daemonset-flex-site.yaml"
```

Unbounded-Net normally creates a managed kube-proxy DaemonSet using the cluster provider's public MCR image. Create a provider kube-proxy DaemonSet for `flex-site` that uses the local mirror instead. The controller detects this coverage and removes its managed kube-proxy label from the Flex node:

```bash
kubectl -n unbounded-net get ds unbounded-net-kube-proxy-flex-site -o json | jq \
  --arg image "$KUBE_PROXY_IMAGE_LOCAL" '
    del(.metadata.annotations,
        .metadata.creationTimestamp,
        .metadata.generation,
        .metadata.managedFields,
        .metadata.resourceVersion,
        .metadata.uid,
        .status)
    | .metadata.name = "offline-kube-proxy-flex-site"
    | .metadata.labels["app.kubernetes.io/name"] = "offline-kube-proxy"
    | .spec.selector.matchLabels["app.kubernetes.io/name"] = "offline-kube-proxy"
    | .spec.template.metadata.labels["app.kubernetes.io/name"] = "offline-kube-proxy"
    | .spec.template.spec.nodeSelector = {
        "net.unbounded-cloud.io/site": "flex-site"
      }
    | (.spec.template.spec.initContainers[]
        | select(.name == "kube-proxy-bootstrap")
        | .image) = $image
    | (.spec.template.spec.containers[]
        | select(.name == "kube-proxy")
        | .image) = $image
  ' > "$SPLIT_DIR/kube-proxy-flex-site.yaml"

kubectl apply --server-side --force-conflicts -f "$SPLIT_DIR/daemonset-flex-site.yaml"
kubectl apply --server-side --force-conflicts -f "$SPLIT_DIR/kube-proxy-flex-site.yaml"

kubectl -n unbounded-net rollout status ds/unbounded-net-node-flex-site --timeout=5m
kubectl -n unbounded-net rollout status ds/offline-kube-proxy-flex-site --timeout=5m
```

Verify that each site uses its intended image and that the controller-managed public kube-proxy DaemonSet has no desired Flex-site pods:

```bash
kubectl -n unbounded-net get ds -o wide
kubectl -n unbounded-net get pods -o wide
kubectl get nodes -L net.unbounded-cloud.io/site -o wide
kubectl describe node <flex-node-name>
```

Expected image placement:

```text
unbounded-net-node-aks-site      ghcr.io/azure/unbounded-net-node:v0.1.10
unbounded-net-node-flex-site     127.0.0.1:5000/offline/unbounded-net-node:v0.1.10
offline-kube-proxy-flex-site     127.0.0.1:5000/offline/kube-proxy:v1.35.0
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
downloading kubernetes binary url=oci://127.0.0.1:5000/aks-flex/bootstrap-artifacts:v20260708-k8s-v1.35.0#kubernetes/v1.35.0/bin/linux/amd64/kubelet
```

Expected evidence for filesystem mode includes log lines and URLs like:

```text
using local OCI layout image image=oci-layout:///opt/aks-flex-node/offline/images/agent-ubuntu2404:v20260619 layout=/opt/aks-flex-node/offline/images/agent-ubuntu2404
downloading kubernetes binary url=file:///opt/aks-flex-node/offline/bootstrap-artifacts/v1.35.0/kubernetes/v1.35.0/bin/linux/amd64/kubelet
```

There should be no bootstrap downloads from public upstream artifact URLs:

```bash
if sudo grep -E 'https://dl.k8s.io|https://github.com/kubernetes-sigs/cri-tools' \
  /var/log/aks-flex-node/aks-flex-node.log; then
  echo "unexpected public artifact download"
  exit 1
fi
```

For local registry mode, verify that containerd—not just the host-side ORAS client—requested the mirrored runtime images:

```bash
sudo podman logs aks-flex-offline-registry 2>&1 | \
  grep -E 'containerd/.*/v2/offline/(unbounded-net-node|kube-proxy)|/v2/offline/(unbounded-net-node|kube-proxy).*(containerd|HTTP)'

sudo machinectl shell "$MACHINE" /usr/local/bin/crictl images | \
  grep -E '127.0.0.1:5000|pause'
```

The registry log should contain successful manifest or blob requests with a `containerd` user agent. The CRI image list should contain the two local runtime images and the sandbox image imported from the offline bundle.

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

### Flex-Site Pods Fail With HTTPS Or `ImagePullBackOff`

Confirm the active machine has the plain-HTTP registry configuration and that containerd loaded it:

```bash
MACHINE="$(machinectl list --no-legend --no-pager | awk '$1 ~ /^kube[12]$/ {print $1; exit}')"
sudo cat "/var/lib/machines/${MACHINE}/etc/containerd/certs.d/127.0.0.1:5000/hosts.toml"
sudo systemctl -M "$MACHINE" restart containerd
sudo machinectl shell "$MACHINE" /usr/local/bin/crictl pull \
  "127.0.0.1:5000/offline/unbounded-net-node:${UNBOUNDED_VERSION}"
```

If the machine changed from `kube1` to `kube2`, recreate `hosts.toml` only to continue this temporary HTTP-registry test. Also verify that the Flex-site DaemonSets reference `127.0.0.1:5000`, not GHCR or MCR. For a durable deployment, replace the local HTTP registry with trusted HTTPS and omit this override.

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
