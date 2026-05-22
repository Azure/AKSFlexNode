# GPU Flex Node setup

How to add a GPU host to an AKS cluster as an AKS Flex Node.

> **Status:** GPU Flex Node support is under active validation.

## Overview

AKS Flex Node joins a prepared host to an AKS cluster. For GPU hosts there are two extra responsibilities that AKS Flex Node does **not** take on:

1. The host must already have a working **NVIDIA kernel driver** before bootstrap.
2. After the node joins, you must manually expose GPU devices and features in-cluster. Install NVIDIA GPU Operator, NVIDIA Device Plugin, NVIDIA GPU Feature Discovery (GFD), and the NVIDIA Dynamic Resource Allocation (DRA) Driver.

Plan for both before you start.

## Before you begin

- An AKS cluster with `kubectl` admin access.
- A GPU host with root access and outbound reach to the AKS API server.
- A GPU host image that already includes the NVIDIA driver.
- Helm installed on your workstation to install the cluster GPU stack.

## Driver and image contract

AKS Flex Node does **not** install the NVIDIA kernel driver. Pick an image where the driver is already baked in. The benefits:

- No first-boot driver build or DKMS failure.
- Deterministic driver version across nodes.
- Faster `Ready` time; no kernel-headers/reboot dance.
- Works in restricted networks.
- Failures point at the image, not at Flex Node bootstrap.

If the image has no driver, you own driver installation, signing for Secure Boot, and kernel-update rebuilds.

### Image options

1. **Ubuntu HPC marketplace image (current validation).** `microsoft-dsvm/ubuntu-hpc/2204/latest`. Other SKUs/versions in the same offer are candidates to validate per region and GPU family:
   - `microsoft-dsvm/ubuntu-hpc/2204` — baseline for current Flex H100/H200 validation.
   - `microsoft-dsvm/ubuntu-hpc/2404` — newer kernel; validate before use.
   - `microsoft-dsvm/ubuntu-hpc/2404-gb` — Grace/Blackwell variant; **not** the current Flex H100/H200 path.
2. **Custom prebaked image.** Bake the NVIDIA driver, Fabric Manager (multi-GPU SXM), and any required signed kernel modules. Most portable fallback because you own the contract.
3. **Other GPU marketplace or partner images.** Treat as candidates to validate.

> **Note:** AKS managed GPU node pools are not a host-image option. They install the driver at boot through the AKS managed GPU bootstrap path, so the image itself is not baked with the driver and cannot be reused as-is by AKS Flex Node.

List and pin candidate Ubuntu HPC versions:

```bash
az vm image list-skus --publisher microsoft-dsvm --offer ubuntu-hpc --location <region> --output table
az vm image list --publisher microsoft-dsvm --offer ubuntu-hpc --sku "2204" --location <region> --all --output table
```

## Cluster GPU stack (manual)

After the Flex node is `Ready`, **you must install the cluster GPU stack yourself**. AKS Flex Node does not deploy any of this. Install at least:

- **NVIDIA GPU Operator** — manages cluster GPU components. Set `driver.enabled=false` because the driver comes from the host image.
- **NVIDIA Device Plugin** — exposes GPU resources to Kubernetes.
- **GPU Feature Discovery (GFD)** — labels nodes with GPU product, driver, and count.
- **NVIDIA DRA Driver** — optional, only if your workloads use Dynamic Resource Allocation.

Example Helm install with the driver disabled:

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
helm install --create-namespace -n gpu-operator gpu-operator nvidia/gpu-operator \
  --set driver.enabled=false \
  --set gfd.enabled=true
```

Confirm the operator picked up the host driver and is not trying to install one:

```bash
kubectl -n gpu-operator get pods
kubectl get clusterpolicy -o jsonpath='{.items[0].spec.driver.enabled}'  # expect: false
```

If you skip this step, the node will be `Ready` but pods will not get GPUs.

> **Optional:** NVIDIA DRA driver exposes GPUs through Kubernetes Dynamic Resource Allocation (`DeviceClass` names such as `gpu.nvidia.com`, `mig.nvidia.com`). In DRA clusters, a node can have GPU labels and DRA devices even when legacy `nvidia.com/gpu` capacity is `0`. Install only if your workloads use DRA.

## Provisioning path

Use direct host bootstrap: create the GPU VM or bare metal host yourself, install or select the GPU-capable image, then run AKS Flex Node bootstrap on that host.

## Direct host bootstrap

Use direct host bootstrap when you manage the GPU host lifecycle directly. This path is useful for a single validation VM, a manually provisioned bare metal host, or an environment where another system owns VM creation.

### 1. Provision a GPU-capable host

Create the VM or prepare the bare metal host with:

- Ubuntu 22.04 or 24.04.
- Outbound HTTPS reachability to the AKS API server.
- A GPU-capable image or prebaked custom image with the NVIDIA driver already installed.
- Any host-specific networking required to reach the AKS VNet, overlay, or gateway.

Before running AKS Flex Node, confirm the host driver works:

```bash
nvidia-smi
lsmod | grep nvidia
```

If these fail, fix the image or driver installation first. AKS Flex Node bootstrap should not be the first component to discover a missing or mismatched driver.

### 2. Prepare AKS bootstrap credentials

On your workstation, create a bootstrap token and RBAC bindings with the shared bootstrap-token example. This is the same setup used by the general node-joining flow; the GPU-specific requirement is that the target host image already has a working NVIDIA driver.

```bash
RESOURCE_GROUP="<aks-resource-group>"
CLUSTER_NAME="<aks-cluster-name>"

az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --admin \
  --overwrite-existing

TOKEN_ID="$(openssl rand -hex 3 | tr '[:upper:]' '[:lower:]')"
TOKEN_SECRET="$(openssl rand -hex 8 | tr '[:upper:]' '[:lower:]')"
BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"
# Set expiry long enough for your planned node/agent lifetime.
EXPIRATION="$(
  python3 - <<'PY'
from datetime import datetime, timedelta, timezone
print((datetime.now(timezone.utc) + timedelta(hours=24)).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
)"

curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/docs/examples/bootstrap-token-rbac.yaml \
  -o /tmp/bootstrap-token-rbac.yaml

# Replace placeholders in /tmp/bootstrap-token-rbac.yaml:
# - __BOOTSTRAP_TOKEN__
# - __TOKEN_ID__ (the TOKEN_ID value created above)
# - __EXPIRATION__
kubectl apply -f /tmp/bootstrap-token-rbac.yaml
```

Collect the values the host config needs:

```bash
TENANT_ID=$(az account show --query tenantId -o tsv)
SUBSCRIPTION_ID=$(az account show --query id -o tsv)
AKS_RESOURCE_ID=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query id -o tsv)
LOCATION=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query location -o tsv)
KUBERNETES_VERSION=$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query kubernetesVersion -o tsv)
SERVER_URL=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA_CERT_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
```

Copy those values, plus `BOOTSTRAP_TOKEN`, to a root shell on the GPU host.

### 3. Install AKS Flex Node on the host

```bash
sudo su
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash
aks-flex-node version
```

### 4. Write the host config

Render the shared bootstrap-token config example instead of hand-writing the JSON:

```bash
mkdir -p /etc/aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/docs/examples/bootstrap-token-config.json \
  -o /etc/aks-flex-node/config.json

# Replace placeholders in /etc/aks-flex-node/config.json:
# - __TENANT_ID__, __SUBSCRIPTION_ID__, __AKS_RESOURCE_ID__, __LOCATION__
# - __BOOTSTRAP_TOKEN__, __SERVER_URL__, __CA_CERT_DATA__, __KUBERNETES_VERSION__

cat /etc/aks-flex-node/config.json
```

### 5. Bootstrap and watch the node

```bash
aks-flex-node start --config /etc/aks-flex-node/config.json
journalctl -u aks-flex-node-agent -f
```

From your workstation:

```bash
kubectl get nodes -o wide
kubectl describe node <gpu-flex-node-name>
```

After the node is `Ready`, install the cluster GPU stack from the previous section if it is not already installed. The host driver is local to the node; GPU Operator, Device Plugin, GFD, and optional DRA are cluster components.

## Validation

```bash
# Node Ready (then identify your GPU node name)
kubectl get nodes -o wide

# GPU labels (populated by GFD)
kubectl get node <gpu-flex-node-name> --show-labels | tr ',' '\n' | grep nvidia.com/gpu

# Host driver and runtime
nvidia-smi
lsmod | grep nvidia
systemctl is-active containerd aks-flex-node-agent
```

Expect: node `Ready`, `nvidia.com/gpu.product` and `nvidia.com/gpu.count` labels present, `nvidia-smi` lists the GPUs, agent and containerd active.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Node not `Ready` | `journalctl -u aks-flex-node-agent`, API-server reachability, bootstrap creds. |
| Node `Ready`, no GPU labels | GPU Operator and GFD installed? `nvidia-smi` works on host? |
| GPU Operator complains about driver | Should be `driver.enabled=false`. Fix the image, not the operator. |
| Pods pending for GPU | Workload uses DRA but DRA driver isn't installed, or uses legacy `nvidia.com/gpu` but cluster is DRA-only. Match request style to install. |
| Driver version drift | Pin the image version. |

## Caveats

- AKS Flex Node does not install the NVIDIA kernel driver.
- AKS Flex Node does not install GPU Operator, Device Plugin, GFD, or DRA. These are manual.
- Image + driver + kernel + containerd versions are part of the GPU node contract. Record them per validation run.
