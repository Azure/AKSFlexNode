# NVIDIA GPU Flex Node setup

How to add an NVIDIA GPU host to an AKS cluster as an AKS Flex Node.

> [!IMPORTANT]
> This lab covers an additional configuration for evaluation. Review its status, prerequisites, and version scope before use.
>
> **Status:** Validated evaluation scenario
>
> **Last validated:** 2026-09-23 with `Standard_NC24ads_A100_v4` in `westus2`
>
> **Version scope:** This lab pins AKS Flex Node `v0.2.0`. The host image, driver, kernel, and cluster GPU stack must be validated as one combination.
>
> **Host OS:** Ubuntu 24.04
>
> **Architecture:** amd64

## Overview

AKS Flex Node joins a prepared host to an AKS cluster. For GPU hosts there are two extra responsibilities that AKS Flex Node does **not** take on:

1. The host must already have a working **NVIDIA kernel driver** before bootstrap.
2. After the node joins, you must manually expose GPU devices and features in-cluster by installing the NVIDIA components your workloads need (for example, GPU Operator with Device Plugin and GFD, plus the optional DRA Driver when workloads use DRA).

Plan for both before you start.

## Before you begin

- An Azure subscription and AKS cluster with `kubectl` admin access.
- Azure CLI 2.90.0 or later, signed in to the target subscription.
- `kubectl`, Helm, `curl`, and SSH/SCP tooling on your workstation.
- A GPU host with root or sudo access and outbound reach to the AKS API server.
- A GPU host image that already includes the NVIDIA driver.
- A 256 GiB host OS disk for the validated A100 configuration; the default 64 GiB disk reached kubelet disk-pressure thresholds after bootstrap and GPU Operator image pulls.
- Non-overlapping network ranges for the AKS cluster, host network, pods, services, and any connected networks.

## Driver and image contract

AKS Flex Node does **not** install the NVIDIA kernel driver. Pick an image where the driver is already baked in. The benefits:

- No first-boot driver build or DKMS failure.
- Deterministic driver version across nodes.
- Faster `Ready` time; no kernel-headers/reboot dance.
- Works in restricted networks.
- Failures point at the image, not at Flex Node bootstrap.

If the image has no driver, you own driver installation, signing for Secure Boot, and kernel-update rebuilds.

### Image options

1. **Ubuntu HPC marketplace image.** Use an Ubuntu 24.04 SKU and validate the selected image version in the target region and GPU family:
   - `microsoft-dsvm/ubuntu-hpc/2404` — validated on `Standard_NC24ads_A100_v4` with image `24.04-v100:24.04.2026092201` and NVIDIA driver `580.178.04`.
   - `microsoft-dsvm/ubuntu-hpc/2404-gb` — Grace/Blackwell variant; validate only with compatible hardware.
2. **Custom prebaked image.** Bake the NVIDIA driver, Fabric Manager (multi-GPU SXM), and any required signed kernel modules. Most portable fallback because you own the contract.
3. **Other GPU marketplace or partner images.** Treat as candidates to validate.

> **Note:** AKS managed GPU node pools are not a host-image option. They install the driver at boot through the AKS managed GPU bootstrap path, so the image itself is not baked with the driver and cannot be reused as-is by AKS Flex Node.

List and pin candidate Ubuntu HPC versions:

```bash
az vm image list-skus --publisher microsoft-dsvm --offer ubuntu-hpc --location <region> --output table
az vm image list --publisher microsoft-dsvm --offer ubuntu-hpc --sku "2404" --location <region> --all --output table
```

## Cluster GPU stack (manual)

After the Flex node is `Ready`, **you must install the cluster GPU stack yourself**. AKS Flex Node does not deploy any of this. Install at least:

- **NVIDIA GPU Operator** — manages cluster GPU components. Set `driver.enabled=false` because the driver comes from the host image.
- **NVIDIA Device Plugin** — exposes GPU resources to Kubernetes.
- **GPU Feature Discovery (GFD)** — labels nodes with GPU product, driver, and count.
- **NVIDIA DRA Driver** — optional, only if your workloads use Dynamic Resource Allocation.

AKS Flex Node already installs the NVIDIA container toolkit inside the nspawn worker. Disable both driver and toolkit management in GPU Operator:

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia && helm repo update
helm install --create-namespace -n gpu-operator gpu-operator nvidia/gpu-operator \
  --set driver.enabled=false \
  --set toolkit.enabled=false \
  --set devicePlugin.enabled=true \
  --set gfd.enabled=true \
  --set 'validator.toolkit.env[0].name=NVIDIA_VISIBLE_DEVICES' \
  --set 'validator.toolkit.env[0].value=nvidia.com/gpu=all'
```

The CDI-qualified validator value makes GPU Operator validate the runtime integration already supplied by AKS Flex Node instead of attempting legacy toolkit injection. Use your preferred NVIDIA path to install the optional DRA driver only when workloads request DRA `DeviceClass` resources.

Confirm the operator picked up the host driver and is not trying to install one:

```bash
kubectl -n gpu-operator get pods
kubectl get clusterpolicy -o jsonpath='{.items[0].spec.driver.enabled}'  # expect: false
kubectl get clusterpolicy -o jsonpath='{.items[0].spec.toolkit.enabled}' # expect: false
```

If you skip this step, the node will be `Ready` but pods will not get GPUs.

> **Optional:** NVIDIA DRA driver exposes GPUs through Kubernetes Dynamic Resource Allocation (`DeviceClass` names such as `gpu.nvidia.com`, `mig.nvidia.com`). In DRA clusters, a node can have GPU labels and DRA devices even when legacy `nvidia.com/gpu` capacity is `0`. Install only if your workloads use DRA.

## Provisioning path

Use direct host bootstrap: create the GPU VM or bare metal host yourself, install or select the GPU-capable image, then run AKS Flex Node bootstrap on that host.

## Direct host bootstrap

Use direct host bootstrap when you manage the GPU host lifecycle directly. This path is useful for a single validation VM, a manually provisioned bare metal host, or an environment where another system owns VM creation.

### 1. Provision a GPU-capable host

Create the VM or prepare the bare metal host with:

- Ubuntu 24.04.
- Outbound HTTPS reachability to the AKS API server.
- A GPU-capable image or prebaked custom image with the NVIDIA driver already installed.
- Any host-specific networking required to reach the AKS VNet, overlay, or gateway.

Before running AKS Flex Node, confirm the host driver works:

```bash
nvidia-smi
lsmod | grep nvidia
```

If these fail, fix the image or driver installation first. AKS Flex Node bootstrap should not be the first component to discover a missing or mismatched driver.

On an A100 host, also inspect MIG mode:

```bash
nvidia-smi --query-gpu=mig.mode.current --format=csv,noheader
```

A100 validation with MIG enabled but no GPU instances failed while generating the CDI specification. Disable MIG and reboot before bootstrap when the node should expose the full physical GPU, or create and validate the intended MIG layout before continuing:

```bash
sudo nvidia-smi -mig 0
sudo reboot
```

After reconnecting, confirm that `nvidia-smi` reports MIG mode as `Disabled`.

<a id="2-prepare-aks-bootstrap-credentials"></a>
### 2. Prepare the Azure identity

For an Azure GPU VM, enable a managed identity, assign it Flex Node Agent Role at the target ARM agent-pool scope, and ensure the Flex node pool exists by following the [identity-backed operator workflow](../usage/getting-started.md). For a host outside Azure, use its Azure Arc or service principal path.

Record the cluster resource ID and Flex pool name; you will export them on the GPU host after becoming root.

<a id="3-install-aks-flex-node-on-the-host"></a>
### 3. Bootstrap the host

On the GPU host, download and inspect the versioned bootstrap script instead of piping it to Bash:

```bash
sudo -i

export AKS_RESOURCE_ID="<full-aks-resource-id>"
export FLEX_POOL_NAME="aksflexnodes"
export AKS_FLEX_NODE_VERSION="v0.2.0"

install -d -m 0700 /run/aks-flex-node-bootstrap

curl -fsSLo /run/aks-flex-node-bootstrap/bootstrap.sh \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/bootstrap.sh"

chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
bash -n /run/aks-flex-node-bootstrap/bootstrap.sh

bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth msi \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-version "$AKS_FLEX_NODE_VERSION"
```

Use `--msi-client-id` for a user-assigned identity. Use the corresponding Arc or service-principal command from the operator guide for a host that doesn't use Azure VM managed identity.

<a id="4-write-the-host-config"></a>
### 4. Confirm the NVIDIA runtime

AKS Flex Node generates the CDI specification and configures the NVIDIA container runtime inside the nspawn worker during bootstrap. Don't run `nvidia-ctk` manually on the host or inside the worker.

Before installing GPU Operator, confirm that the bootstrap output reports successful NVIDIA setup.

<a id="5-bootstrap-and-watch-the-node"></a>
### 5. Watch the node

From your workstation:

```bash
kubectl get nodes -o wide
kubectl describe node <gpu-flex-node-name>
```

Continue when the node is `Ready`. Then install the cluster GPU stack from the **Cluster GPU stack (manual)** section. The host driver is local to the node; GPU Operator, Device Plugin, GFD, and optional DRA are cluster components.

## Validation

```bash
# Node Ready (then identify your GPU node name)
kubectl get nodes -o wide

# GPU labels (populated by GFD)
kubectl get node <gpu-flex-node-name> --show-labels | tr ',' '\n' | grep nvidia.com/gpu

# Host driver
nvidia-smi
lsmod | grep nvidia
```

Run a legacy GPU workload after `nvidia.com/gpu` becomes allocatable:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: nvidia-smi-validation
spec:
  restartPolicy: Never
  nodeName: <gpu-flex-node-name>
  runtimeClassName: nvidia
  containers:
  - name: nvidia-smi
    image: nvcr.io/nvidia/cuda:12.8.0-base-ubuntu24.04
    command: ["nvidia-smi"]
    resources:
      limits:
        nvidia.com/gpu: 1
```

Expect: node `Ready`, `nvidia.com/gpu.product` and `nvidia.com/gpu.count` labels present, `nvidia.com/gpu: 1` allocatable, the validation Pod succeeds, and `nvidia-smi` lists the GPU.

<a id="current-validation-blockers"></a>
## Validation result

The successful 2026-09-23 run used:

- AKS `1.36.2`, Unbounded `v0.8.0`, and AKS Flex Node `v0.2.0`;
- `Standard_NC24ads_A100_v4` in `westus2`;
- Ubuntu HPC `24.04-v100:24.04.2026092201` with NVIDIA driver `580.178.04`;
- a 256 GiB OS disk;
- MIG disabled before bootstrap;
- managed-identity online bootstrap;
- GPU Operator `v26.7.1` with driver and toolkit management disabled;
- the NVIDIA runtime managed by AKS Flex Node;
- GPU Operator toolkit management disabled and its toolkit validator configured to request the CDI-qualified `nvidia.com/gpu=all` device.

The Flex Node became `Ready`, GPU Feature Discovery labeled the node as `NVIDIA-A100-80GB-PCIe`, `nvidia.com/gpu: 1` became allocatable, and an `nvidia-smi` Pod completed successfully.


## Troubleshooting

| Symptom | Check |
| --- | --- |
| Node not `Ready` | `kubectl describe node`, kubelet logs in the nspawn worker, API-server reachability, and bootstrap credentials. |
| Node `Ready`, no GPU labels | GPU Operator and GFD installed? Did bootstrap complete NVIDIA setup? Does `nvidia-smi` work on the host? |
| GPU Operator toolkit validation can't find `nvidia-smi` | Keep `toolkit.enabled=false` and set its validator's `NVIDIA_VISIBLE_DEVICES` to the CDI-qualified value `nvidia.com/gpu=all`. |
| GPU Operator complains about the driver | `driver.enabled` should be `false`. Fix the host image rather than asking GPU Operator to replace its driver. |
| Pods pending for GPU | Workload uses DRA but DRA driver isn't installed, or uses legacy `nvidia.com/gpu` but cluster is DRA-only. Match request style to install. |
| Driver version drift | Pin the image version. |

## Caveats

- AKS Flex Node does not install the NVIDIA kernel driver.
- AKS Flex Node does not install GPU Operator, Device Plugin, GFD, or DRA. These are manual.
- Image + driver + kernel + containerd versions are part of the GPU node contract. Record them per validation run.

## Clean up

Remove validation workloads created for this lab. If the GPU stack was installed only for evaluation, remove it by following the NVIDIA operator or plugin documentation for the exact version you installed.

To detach the Flex node, follow [Reset and uninstall](../usage/operations.md#reset-and-uninstall). Don't remove a shared cluster GPU stack while other GPU nodes depend on it.
