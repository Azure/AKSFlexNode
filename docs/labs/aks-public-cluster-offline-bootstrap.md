# AKS Flex Node With Offline Bootstrap Artifacts

Join a Flex node using filesystem or local OCI bootstrap artifacts instead of public artifact endpoints. Use the [public VNet-peering lab](aks-public-cluster-unbounded-net-vnet-peering.md) for the cluster, Flex pool, host identity, and private Layer 3 underlay. The same artifact flow can use a private API endpoint when the host and your Bash environment can resolve and reach it.

> [!IMPORTANT]
> This lab covers an additional configuration for evaluation. Review its status, prerequisites, and version scope before use.
>
> **Status:** Validated evaluation scenario
>
> **Last validated:** 2026-11-08
>
> **Validated versions:** AKS Kubernetes `1.36.2`, AKS Flex Node `v0.3.0`, Unbounded `v0.21.0`, rootfs `v20261031`, Ubuntu `24.04`, ORAS `1.3.2`, and registry `2.8.3`
>
> **Validated scope:** Filesystem and local OCI bootstrap, checksum/sandbox-image verification, pre-staged agent use, ARM identity access during artifact lockdown, canonical runtime pulls through loopback port `15000`, healthy networking/Gantry pods, Node readiness, bidirectional pod/ClusterIP/DNS traffic, exec/logs callbacks, unjoin/rejoin with new Node UIDs, smoke cleanup, and Azure resource-group removal passed. Public GitHub, GHCR, MCR, and Kubernetes artifact endpoints remained blocked on the Flex host.
>
> **Validation toolchain:** Azure CLI `2.92.0`, `aks-preview` `22.0.0b8`, and kubectl `1.36.2`
>
> **Host OS:** Ubuntu 24.04
>
> **Architecture:** amd64

Offline artifacts don't make the host air-gapped. Managed-identity bootstrap and the running daemon still need Azure Resource Manager (ARM), IMDS, and the AKS API. Install host packages, stage the agent and bootstrap script, and mirror runtime images before restricting artifact egress. Node Problem Detector is skipped when `bootstrap.offlineArtifacts.source` is set because the upstream bundle doesn't contain NPD.

The Unbounded operator retains ownership of networking, Gantry, and managed kube-proxy workloads. Keep their canonical image references; redirect this worker's pulls through containerd configuration rather than copying or patching DaemonSets.

## Prerequisites

Prepare the base lab through:

1. [Create resource groups and networks](aks-public-cluster-unbounded-net-vnet-peering.md#create-resource-groups-and-networks).
2. [Create a public no-CNI AKS cluster](aks-public-cluster-unbounded-net-vnet-peering.md#create-a-public-no-cni-aks-cluster).
3. [Create the Flex node pool](aks-public-cluster-unbounded-net-vnet-peering.md#create-the-flex-node-pool).
4. [Create the Flex VM](aks-public-cluster-unbounded-net-vnet-peering.md#create-the-flex-vm), assign its pool-scoped role, and verify SSH access.

Don't bootstrap the host yet. If you already joined it, [cordon, drain, and reset it](../usage/operations.md#reset-and-uninstall), then remove its Kubernetes Node before this bootstrap. Keep its ARM Machine and identity for rejoin.

Your Bash environment needs Azure CLI, `kubectl`, `jq`, SSH/SCP, `curl`, and `tar`. The connected preparation environment needs ORAS; local-registry mode also needs Podman or Docker on the target VM. Use a fresh or reset Ubuntu 24.04 amd64 host with root access.

In your Bash environment, retrieve the pool's accepted patch version without printing bootstrap data:

```bash
AKS_RG="<aks-resource-group>"
CLUSTER_NAME="<aks-cluster-name>"
AGENT_POOL_NAME="aksflexnodes"
KUBERNETES_VERSION=$(az aks nodepool show -g "$AKS_RG" \
  --cluster-name "$CLUSTER_NAME" --name "$AGENT_POOL_NAME" \
  --query orchestratorVersion -o tsv)
printf 'Pool Kubernetes version: %s\n' "$KUBERNETES_VERSION"
```

Set these nonsecret artifact values in your API-connected Bash environment and in any separate connected preparation environment. Copy the pool version printed above. The target VM's root-shell values are initialized separately in **Prepare The Host Identity**:

```bash
KUBERNETES_VERSION="1.36.2"
KUBERNETES_VERSION_V="v${KUBERNETES_VERSION#v}"
UNBOUNDED_VERSION="v0.21.0"
AKS_FLEX_NODE_VERSION="v0.3.0"
ROOTFS_TAG="v20261031"
ROOTFS_IMAGE_UPSTREAM="ghcr.io/azure/agent-ubuntu2404:${ROOTFS_TAG}"
ARTIFACT_TAG="${UNBOUNDED_VERSION}-k8s-${KUBERNETES_VERSION_V}"
ARTIFACT_BUNDLE_UPSTREAM="ghcr.io/azure/unbounded/bootstrap-artifacts:${ARTIFACT_TAG}"
```

The bundle must match the pool's full Kubernetes patch version. The agent expands `.KubernetesVersion` with a leading `v`; the bundle manifest supplies the bootstrap binary versions and checksums.

<a id="install-unbounded-net-with-site-scoped-node-daemonsets"></a>
## Install The Unbounded Operator

In your API-connected Bash environment, install the versioned plugin:

```bash
case "$(uname -m)" in
  x86_64) UNBOUNDED_ARCH=amd64 ;;
  aarch64|arm64) UNBOUNDED_ARCH=arm64 ;;
  *) echo "unsupported architecture" >&2; exit 1 ;;
esac
curl -fsSLo /tmp/kubectl-unbounded.tar.gz \
  "https://github.com/Azure/unbounded/releases/download/${UNBOUNDED_VERSION}/kubectl-unbounded-linux-${UNBOUNDED_ARCH}.tar.gz"
curl -fsSLo /tmp/unbounded-checksums.txt \
  "https://github.com/Azure/unbounded/releases/download/${UNBOUNDED_VERSION}/checksums.txt"
(cd /tmp && awk -v archive="kubectl-unbounded-linux-${UNBOUNDED_ARCH}.tar.gz" \
  '$2 == archive {print $1 "  kubectl-unbounded.tar.gz"}' unbounded-checksums.txt | sha256sum --check --strict) || exit 1
tar -xzf /tmp/kubectl-unbounded.tar.gz -C /tmp
sudo install -m 0755 /tmp/kubectl-unbounded /usr/local/bin/kubectl-unbounded
kubectl unbounded version
kubectl unbounded install --timeout 60s

kubectl unbounded site init --name flex-site \
  --cluster-node-cidr 10.91.0.0/16 --cluster-pod-cidr 10.93.0.0/16 \
  --node-cidr 10.92.0.0/16 --pod-cidr 10.95.0.0/16

# site init creates gateway defaults; this topology uses node mesh instead.
kubectl delete sitegatewaypoolassignments cluster flex-site
kubectl delete gatewaypool gw-main

kubectl apply -f - <<'EOF'
apiVersion: net.unbounded-cloud.io/v1alpha1
kind: SitePeering
metadata:
  name: cluster-flex-private-l3
spec:
  sites: [cluster, flex-site]
  meshNodes: true
  tunnelProtocol: Auto
EOF

kubectl -n unbounded-system wait --for=create deployment/unbounded-net-controller --timeout=60s
kubectl -n unbounded-system get pods -o wide
kubectl -n unbounded-system rollout status deployment/unbounded-net-controller --timeout=60s
kubectl -n unbounded-system rollout status daemonset/unbounded-net-node --timeout=60s
kubectl get nodes -L unbounded-cloud.io/site -o wide
```

The managed AKS node should become `Ready`. If a rollout stalls, inspect pods, events, and container logs before waiting again; use `--previous` for restarted containers.

## Install Host Prerequisites On The Target VM

On the target VM, while package repositories remain reachable:

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  systemd-container curl tar jq nftables util-linux podman

ORAS_VERSION="1.3.2"
curl -fsSLo /tmp/oras_1.3.2_linux_amd64.tar.gz \
  "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_linux_amd64.tar.gz"
curl -fsSLo /tmp/oras_1.3.2_checksums.txt \
  "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_checksums.txt"
(cd /tmp && awk '$2 == "oras_1.3.2_linux_amd64.tar.gz"' oras_1.3.2_checksums.txt | sha256sum --check --strict) || exit 1
sudo tar -C /usr/local/bin -xzf /tmp/oras_1.3.2_linux_amd64.tar.gz oras
oras version
```

<a id="generate-a-baseline-aks-flex-node-config"></a>
## Prepare The Host Identity

Keep the VM's managed identity and pool-scoped Azure Kubernetes Service Flex Node Agent Role from the base lab. Allow role propagation before lockdown. Fresh `listBootstrapData` retrieval replaces client-side bootstrap RBAC and rendered baseline configuration.

On a fresh or reset target VM, open a root shell and reinitialize the nonsecret values after `sudo -i`. Use the pool version retrieved in your Bash environment. Keep this shell for all subsequent target-VM steps:

```bash
sudo -i
AKS_RESOURCE_ID="<full-aks-resource-id>"
AGENT_POOL_NAME="aksflexnodes"
KUBERNETES_VERSION="1.36.2"
KUBERNETES_VERSION_V="v${KUBERNETES_VERSION#v}"
UNBOUNDED_VERSION="v0.21.0"
AKS_FLEX_NODE_VERSION="v0.3.0"
ROOTFS_TAG="v20261031"
ROOTFS_IMAGE_UPSTREAM="ghcr.io/azure/agent-ubuntu2404:${ROOTFS_TAG}"
ARTIFACT_TAG="${UNBOUNDED_VERSION}-k8s-${KUBERNETES_VERSION_V}"
ARTIFACT_BUNDLE_UPSTREAM="ghcr.io/azure/unbounded/bootstrap-artifacts:${ARTIFACT_TAG}"
STAGE=/run/aks-flex-node-bootstrap
AGENT_STAGE=/opt/aks-flex-node/bootstrap-agent
install -d -m 0700 "$STAGE" "$AGENT_STAGE"

curl -fsSLo "$STAGE/bootstrap.sh" \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/bootstrap.sh"
chmod 0700 "$STAGE/bootstrap.sh"
bash -n "$STAGE/bootstrap.sh"
curl -fsSLo "$STAGE/aks-flex-node-linux-amd64.tar.gz" \
  "https://github.com/Azure/AKSFlexNode/releases/download/${AKS_FLEX_NODE_VERSION}/aks-flex-node-linux-amd64.tar.gz"
curl -fsSLo "$STAGE/checksums.txt" \
  "https://github.com/Azure/AKSFlexNode/releases/download/${AKS_FLEX_NODE_VERSION}/checksums.txt"
(cd "$STAGE" && awk '$2 == "aks-flex-node-linux-amd64.tar.gz"' checksums.txt | sha256sum --check --strict) || exit 1
tar -xzf "$STAGE/aks-flex-node-linux-amd64.tar.gz" -C "$AGENT_STAGE" || exit 1
AGENT_BIN="$AGENT_STAGE/aks-flex-node-linux-amd64"
chmod 0700 "$AGENT_BIN"
"$AGENT_BIN" version
```

The executable is staged under `/opt`, which must allow execution, rather than `/run`, which can be mounted `noexec`. Scripts and archives remain in `/run` and the script is invoked through Bash.

If a matching agent is already installed, retain it after reset, stage only `bootstrap.sh`, and set `AGENT_BIN=/opt/unbounded/agent/bin/aks-flex-node`. Don't overwrite an active managed binary layout. Both modes use `--installed-agent "$AGENT_BIN"`, avoiding archive downloads after lockdown; successful bootstrap installs the durable managed layout under `/opt/unbounded/agent`.

## Option A: Use A Filesystem Bootstrap Artifact Bundle

Filesystem mode reads bootstrap binaries from `file://` and the rootfs from a local OCI layout. Runtime networking and workload images still need the local mirror configured in Option B; you can omit Option B's rootfs and bootstrap-bundle copies.

### Stage The Rootfs Image As A Local OCI Layout

On the connected preparation host or target VM before lockdown:

```bash
ROOTFS_LAYOUT_DIR=/opt/aks-flex-node/offline/images/agent-ubuntu2404
ROOTFS_TAG=v20261031
sudo mkdir -p "$(dirname "$ROOTFS_LAYOUT_DIR")"
sudo chown "$(id -u):$(id -g)" "$(dirname "$ROOTFS_LAYOUT_DIR")"
oras copy --to-oci-layout "$ROOTFS_IMAGE_UPSTREAM" "${ROOTFS_LAYOUT_DIR}:${ROOTFS_TAG}"
oras manifest fetch --oci-layout "${ROOTFS_LAYOUT_DIR}:${ROOTFS_TAG}" >/dev/null
```

### Pull The Bootstrap Artifact Bundle To Files

```bash
ARTIFACTS_DIR="/opt/aks-flex-node/offline/bootstrap-artifacts/${KUBERNETES_VERSION_V}"
sudo mkdir -p "$ARTIFACTS_DIR"
sudo chown "$(id -u):$(id -g)" "$ARTIFACTS_DIR"
oras pull --output "$ARTIFACTS_DIR" "$ARTIFACT_BUNDLE_UPSTREAM"
test -f "$ARTIFACTS_DIR/manifest.json"
jq -e '.containerImages | length > 0' "$ARTIFACTS_DIR/manifest.json"
```

Retain every file referenced by the manifest, including checksums and sandbox image archives. If you stage elsewhere, copy the complete directories to these paths on the target VM.

## Option B: Use A Local OCI Registry For Bootstrap And Flex-Site Runtime Artifacts

### Start The Local Registry

On the target VM:

```bash
sudo podman run -d --name aks-flex-offline-registry --restart=always \
  --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:15000 \
  docker.io/library/registry:2
curl -fsS http://127.0.0.1:15000/v2/ >/dev/null
```

Use port **15000**, not **5000**. Unbounded `v0.21.0` deploys Gantry's read-only mirror through host port `127.0.0.1:5000`; its CNI port mapping can intercept requests even when another process listens there. Uploads sent to Gantry return HTTP 405, and noncanonical manifest requests can return HTTP 503. Don't disable Gantry or replace operator-managed workloads to work around the conflict.

<a id="mirror-bootstrap-and-flex-site-runtime-artifacts"></a>
### Mirror Bootstrap And Operator-Managed Runtime Artifacts

In your Bash environment, capture the exact runtime references:

```bash
UNBOUNDED_NODE_IMAGE_UPSTREAM=$(kubectl -n unbounded-system get ds unbounded-net-node \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="node")].image}')
KUBE_PROXY_IMAGE_UPSTREAM=$(kubectl -n unbounded-system get ds unbounded-net-kube-proxy-flex-site \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="kube-proxy")].image}')
GANTRY_IMAGE_UPSTREAM=$(kubectl -n unbounded-system get ds gantry \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="gantry")].image}')
printf '%s\n' "$UNBOUNDED_NODE_IMAGE_UPSTREAM" "$KUBE_PROXY_IMAGE_UPSTREAM" "$GANTRY_IMAGE_UPSTREAM"
```

Export those nonsecret references on the target VM, then mirror them while upstream registries remain reachable:

```bash
UNBOUNDED_NODE_IMAGE_UPSTREAM="<captured-node-image>"
KUBE_PROXY_IMAGE_UPSTREAM="<captured-kube-proxy-image>"
GANTRY_IMAGE_UPSTREAM="<captured-gantry-image>"
ROOTFS_IMAGE_LOCAL=127.0.0.1:15000/aks-flex/rootfs/agent-ubuntu2404:v20261031
ARTIFACT_BUNDLE_LOCAL="127.0.0.1:15000/aks-flex/bootstrap-artifacts:${ARTIFACT_TAG}"

# Omit these two copies when using filesystem bootstrap artifacts.
oras copy --to-plain-http "$ROOTFS_IMAGE_UPSTREAM" "$ROOTFS_IMAGE_LOCAL"
oras copy --to-plain-http "$ARTIFACT_BUNDLE_UPSTREAM" "$ARTIFACT_BUNDLE_LOCAL"

# Preserve each canonical repository path; containerd replaces only its host.
for image in "$UNBOUNDED_NODE_IMAGE_UPSTREAM" "$KUBE_PROXY_IMAGE_UPSTREAM" \
  "$GANTRY_IMAGE_UPSTREAM" docker.io/library/busybox:1.36; do
  local_image="127.0.0.1:15000/${image#*/}"
  oras copy --to-plain-http "$image" "$local_image"
  oras manifest fetch --plain-http "$local_image" >/dev/null
done
```

If the VM never has public artifact access, copy complete OCI layouts from a connected staging host with `oras copy --to-oci-layout`, transfer them to the VM, and publish with `oras copy --from-oci-layout --to-plain-http`. Don't copy partial OCI blobs or change operator image references.

## Restrict Egress After Staging

Allow the AKS API, ARM, IMDS, Azure platform DNS, and the base lab's private node underlay. Deny other artifact destinations before bootstrap. Loopback registry traffic doesn't traverse an Azure NSG.

### Preflight-Only Host-Level Block With nftables

Host-level rules are useful only for a preflight sanity check: bootstrap starts an nftables reset unit that flushes them. Don't use them to claim persistent isolation. On the target VM, create this temporary table only when it doesn't already exist:

```bash
BLOCKED_ARTIFACT_HOSTS=(
  dl.k8s.io github.com objects.githubusercontent.com
  github-releases.githubusercontent.com pkg-containers.githubusercontent.com
  ghcr.io storage.googleapis.com packages.microsoft.com
  mcr.microsoft.com registry.k8s.io
)
sudo nft add table inet aksflex_offline
sudo nft 'add set inet aksflex_offline blocked_v4 { type ipv4_addr; flags interval; }'
sudo nft 'add chain inet aksflex_offline output { type filter hook output priority 0; policy accept; }'
sudo nft 'add rule inet aksflex_offline output ip daddr @blocked_v4 tcp dport { 80, 443 } reject'
for host in "${BLOCKED_ARTIFACT_HOSTS[@]}"; do
  getent ahostsv4 "$host"
done | awk '{print $1}' | sort -u | while read -r ip; do
  sudo nft add element inet aksflex_offline blocked_v4 "{ ${ip} }"
done
sudo nft list table inet aksflex_offline
```

CDN addresses can change, and different hosts can share an address. Remove the table with `sudo nft delete table inet aksflex_offline` before recreating it. Use the NSG controls below for complete bootstrap and workload isolation.

### Persistent Lockdown With NSG Rules

Run Azure commands in your Bash environment. Use the NSG actually attached to the target VM, not an unrelated name:

```bash
VM_RG="<vm-resource-group>"
VM_NAME="<flex-vm-name>"
NIC_ID=$(az vm show -g "$VM_RG" -n "$VM_NAME" \
  --query 'networkProfile.networkInterfaces[0].id' -o tsv)
NSG_ID=$(az network nic show --ids "$NIC_ID" --query networkSecurityGroup.id -o tsv)
if [[ -z "$NSG_ID" ]]; then
  NSG_NAME=aks-flex-offline-nsg
  az network nsg create -g "$VM_RG" -n "$NSG_NAME" --output none
  az network nic update --ids "$NIC_ID" --network-security-group "$NSG_NAME" --output none
  NSG_RG="$VM_RG"
else
  NSG_NAME="${NSG_ID##*/}"
  NSG_RG=$(printf '%s' "$NSG_ID" | cut -d/ -f5)
fi
az network nic list-effective-nsg --ids "$NIC_ID" -o table

AKS_API_FQDN=$(az aks show -g "$AKS_RG" -n "$CLUSTER_NAME" --query fqdn -o tsv)
mapfile -t AKS_API_IPS < <(getent ahostsv4 "$AKS_API_FQDN" | awk '{print $1}' | sort -u)
[[ ${#AKS_API_IPS[@]} -gt 0 ]] || exit 1

az network nsg rule create -g "$NSG_RG" --nsg-name "$NSG_NAME" -n allow-aks-api \
  --priority 300 --direction Outbound --access Allow --protocol Tcp \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes "${AKS_API_IPS[@]}" --destination-port-ranges 443
az network nsg rule create -g "$NSG_RG" --nsg-name "$NSG_NAME" -n allow-aks-vnet \
  --priority 310 --direction Outbound --access Allow --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes 10.91.0.0/16 --destination-port-ranges '*'
az network nsg rule create -g "$NSG_RG" --nsg-name "$NSG_NAME" -n allow-azure-platform \
  --priority 320 --direction Outbound --access Allow --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes 168.63.129.16 --destination-port-ranges '*'
az network nsg rule create -g "$NSG_RG" --nsg-name "$NSG_NAME" -n allow-imds \
  --priority 330 --direction Outbound --access Allow --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes 169.254.169.254 --destination-port-ranges '*'
az network nsg rule create -g "$NSG_RG" --nsg-name "$NSG_NAME" -n allow-arm \
  --priority 340 --direction Outbound --access Allow --protocol Tcp \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes AzureResourceManager --destination-port-ranges 443
az network nsg rule create -g "$NSG_RG" --nsg-name "$NSG_NAME" -n deny-all-egress \
  --priority 4000 --direction Outbound --access Deny --protocol '*' \
  --source-address-prefixes '*' --source-port-ranges '*' \
  --destination-address-prefixes '*' --destination-port-ranges '*'
az network nsg rule list -g "$NSG_RG" --nsg-name "$NSG_NAME" -o table
```

Retain approved SSH access and any additional platform allows required by your environment. For a private cluster, allow its private API/underlay ranges instead. Public API addresses can change; use an FQDN-aware firewall for durable public-endpoint policy.

After propagation, test from the target VM:

```bash
AKS_API_FQDN="<public-aks-fqdn>"
curl -k -sS --connect-timeout 10 --max-time 15 -o /dev/null \
  -w 'AKS API: %{http_code}\n' "https://${AKS_API_FQDN}:443"
curl -fsS http://127.0.0.1:15000/v2/ >/dev/null
for url in https://dl.k8s.io https://ghcr.io https://mcr.microsoft.com https://github.com; do
  if curl -sS --connect-timeout 5 --max-time 8 -o /dev/null "$url"; then
    echo "unexpectedly reachable: $url" >&2
    exit 1
  fi
  echo "blocked: $url"
done
```

The API should return HTTP 401 without credentials. Artifact requests must fail to connect; an HTTP 401 or 403 from a registry still proves reachability, so don't use `curl -f` for this blocked-egress check.

<a id="install-the-config-and-run-preflight"></a>
<a id="bootstrap-the-node"></a>
## Bootstrap The Node With Offline Overrides

In the same target-VM root shell, choose one source. If you opened a new shell, rerun the nonsecret declarations in **Prepare The Host Identity** and set `AGENT_BIN` to the verified executable under `/opt/aks-flex-node/bootstrap-agent` or the matching installed agent under `/opt/unbounded/agent/bin`:

<a id="patch-the-config-for-filesystem-mode"></a>

```bash
# Filesystem rootfs and bootstrap binaries:
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --installed-agent "$AGENT_BIN" \
  --auth msi --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" --agent-pool-name "$AGENT_POOL_NAME" \
  --agent-version v0.3.0 \
  --bootstrap-oci-image 'oci-layout:///opt/aks-flex-node/offline/images/agent-ubuntu2404:v20261031' \
  --bootstrap-offline-artifacts-source 'file:///opt/aks-flex-node/offline/bootstrap-artifacts/{{ .KubernetesVersion }}'
```

<a id="patch-the-config-for-local-registry-mode"></a>

Or use the local registry for both:

```bash
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --installed-agent "$AGENT_BIN" \
  --auth msi --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" --agent-pool-name "$AGENT_POOL_NAME" \
  --agent-version v0.3.0 \
  --bootstrap-oci-image '127.0.0.1:15000/aks-flex/rootfs/agent-ubuntu2404:v20261031' \
  --bootstrap-offline-artifacts-source 'oci://127.0.0.1:15000/aks-flex/bootstrap-artifacts:v0.21.0-k8s-{{ .KubernetesVersion }}'
```

Bootstrap retrieves pool data through ARM, performs preflight, ensures the Machine, provisions the worker, and starts the daemon. The Node can remain `NotReady` until its runtime mirror is configured below.

<a id="configure-nspawn-containerd-for-the-temporary-local-http-registry"></a>
## Configure Nspawn Containerd To Use The Local Mirror

> [!WARNING]
> Plain HTTP is for this isolated loopback test only. Use a trusted HTTPS registry and durable worker provisioning outside the lab.

The worker shares the host network namespace but has a separate filesystem. On the target VM, find the active machine and set per-registry mirrors:

```bash
MACHINE=$(sudo jq -r .activeMachine /etc/aks-flex-node/daemon-state.json)
case "$MACHINE" in kube1|kube2) ;; *) exit 1 ;; esac
LEADER=$(machinectl show "$MACHINE" -p Leader --value)
for registry in ghcr.io mcr.microsoft.com docker.io; do
  CERTS_DIR="/proc/${LEADER}/root/etc/containerd/certs.d/${registry}"
  sudo install -d -m 0755 "$CERTS_DIR"
  printf 'server = "https://%s"\n\n[host."http://127.0.0.1:15000"]\n  capabilities = ["pull", "resolve"]\n' "$registry" | \
    sudo tee "$CERTS_DIR/hosts.toml" >/dev/null
done
sudo systemctl -M "$MACHINE" restart containerd
sudo systemctl -M "$MACHINE" is-active containerd

sudo systemd-run --machine="$MACHINE" --pipe --wait --collect \
  /usr/local/bin/crictl pull "$UNBOUNDED_NODE_IMAGE_UPSTREAM"
sudo systemd-run --machine="$MACHINE" --pipe --wait --collect \
  /usr/local/bin/crictl pull "$KUBE_PROXY_IMAGE_UPSTREAM"
sudo systemd-run --machine="$MACHINE" --pipe --wait --collect \
  /usr/local/bin/crictl pull "$GANTRY_IMAGE_UPSTREAM"
```

Per-registry files take precedence over Gantry's default containerd mirror. This configuration belongs only to the active rootfs; repave doesn't copy it to the new side. Recreate it for lab rejoin or use durable provisioning with a trusted mirror.

<a id="deploy-the-offline-flex-site-networking-daemonsets"></a>
In your Bash environment, verify convergence without patching DaemonSets:

```bash
FLEX_NODE_NAME="<flex-node-name>"
kubectl wait --for=create "node/${FLEX_NODE_NAME}" --timeout=60s
kubectl wait --for=condition=Ready "node/${FLEX_NODE_NAME}" --timeout=60s
kubectl -n unbounded-system get pods -o wide
kubectl -n unbounded-system rollout status ds/unbounded-net-node --timeout=60s
kubectl -n unbounded-system rollout status ds/unbounded-net-kube-proxy-flex-site --timeout=60s
kubectl -n unbounded-system rollout status ds/gantry --timeout=60s
```

## Validate That Offline Artifacts Were Used

Check preflight/bootstrap output for the selected local rootfs and artifact source, successful checksum verification and sandbox image import, and the offline NPD warning. Don't print the credential-bearing runtime configuration.

Inspect local-registry request paths on the host:

```bash
sudo podman logs aks-flex-offline-registry 2>&1 | \
  grep -E '/v2/(aks-flex|azure/unbounded-net-node|azure/gantry|oss/v2/kubernetes/kube-proxy|library/busybox)/'
```

Expect successful manifest/blob requests for canonical runtime repositories. Run the [base lab's bidirectional pod, exec, and logs checks](aks-public-cluster-unbounded-net-vnet-peering.md#verify); the mirrored BusyBox image also permits workload validation while public registries remain blocked. Re-run the blocked-egress checks after the workloads pass.

For rejoin validation, cordon/drain the Node, run `/opt/unbounded/agent/bin/aks-flex-node reset` as root, delete the Kubernetes Node, and repeat the selected bootstrap and mirror steps. Keep the NSG restrictions active throughout. Require a new Node UID, `Ready`, healthy networking pods, and successful cross-node traffic again.

After the final successful bootstrap, remove transient scripts and archives from `/run/aks-flex-node-bootstrap` and the staging directory `/opt/aks-flex-node/bootstrap-agent`. Keep the installed managed agent under `/opt/unbounded/agent`, the offline artifact directories, and the local registry until validation finishes.

## Troubleshooting

### Preflight Fails `host-packages`

Install required packages before isolation; offline preflight doesn't use public apt repositories to repair the host.

### Preflight Fails `oci-image-reachable`

For filesystem mode, check the layout and exact tag:

```bash
oras manifest fetch --oci-layout /opt/aks-flex-node/offline/images/agent-ubuntu2404:v20261031 >/dev/null
```

For registry mode:

```bash
curl -fsS http://127.0.0.1:15000/v2/ >/dev/null
oras manifest fetch --plain-http 127.0.0.1:15000/aks-flex/rootfs/agent-ubuntu2404:v20261031 >/dev/null
```

### Flex-Site Pods Fail With HTTPS Or `ImagePullBackOff`

Inspect pod status, events, and containerd logs before extending rollout waits. Check the active rootfs's per-registry `hosts.toml` files and repeat CRI pulls through port 15000. An error targeting port 5000 is targeting Gantry, not this lab's standalone registry. HTTP 405 on publication usually indicates the same port conflict.

### Preflight Fails Artifact Checks

Check the full pool version, leading `v`, manifest and referenced checksum/archive paths. For local registry mode:

```bash
oras manifest fetch --plain-http "127.0.0.1:15000/aks-flex/bootstrap-artifacts:${ARTIFACT_TAG}" >/dev/null
```

A missing agent after lockdown means staging was incomplete. Restore approved artifact access or transfer the reviewed archive and script from the connected preparation environment; don't disable identity or registration checks.

## Clean up

Remove smoke workloads and temporary host firewall tables, then delete the two resource groups inherited from the base lab:

```bash
kubectl delete pod flex-exec-smoke aks-peering-smoke flex-peering-smoke --ignore-not-found --wait=false
az group delete --name "$AKS_RG" --yes --no-wait
az group delete --name "$VM_RG" --yes --no-wait
az group exists --name "$AKS_RG"
az group exists --name "$VM_RG"
```

Both existence checks should eventually return `false`; also verify the AKS-managed node resource group is removed. If you retain the VM instead, drain/reset it, remove the lab's local registry and mirror files, and remove only the NSG rules you created.
