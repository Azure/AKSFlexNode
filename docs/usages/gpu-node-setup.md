# GPU Flex Node setup

This guide explains the general flow for adding GPU-capable AKS Flex Nodes to an AKS cluster.

> **Status:** GPU Flex Node support is under active validation. Use a validated image and confirm the driver, container runtime, and GPU device labels before scheduling production workloads.

## Overview

AKS Flex Node turns a prepared host into an AKS worker node. For GPU hosts, the extra requirement is that the host must already have a working NVIDIA kernel driver before AKS Flex Node bootstraps Kubernetes components.

The high-level flow is:

1. Choose a GPU-capable host image or prebaked image.
2. Install or verify the NVIDIA host driver on that image.
3. Install AKS Flex Node and join the host to the AKS cluster.
4. Install cluster GPU components such as NVIDIA GPU Operator, GPU Feature Discovery, and NVIDIA DRA driver.
5. Validate that the node is `Ready`, reports the expected GPU labels, and can run a GPU workload.

Expected result: the Flex Node appears in `kubectl get nodes` as `Ready`, has NVIDIA GPU labels from GPU Feature Discovery, and can run pods that request GPU resources through the configured GPU scheduling path.

## Before you begin

You need:

- An AKS cluster with admin access through `kubectl`.
- One or more GPU-capable hosts or virtual machines.
- A GPU host image that matches the hardware SKU.
- Root access on the target host.
- Outbound network access from the host to the AKS API server and required package and container registries.
- A cluster GPU add-on plan, such as NVIDIA GPU Operator plus NVIDIA DRA driver.

> **Important:** AKS Flex Node does not install the NVIDIA kernel driver. The GPU host driver must already be available before AKS Flex Node bootstraps the host.

## What this flow does

The setup flow separates host preparation from cluster GPU scheduling:

| Layer | Responsibility |
| --- | --- |
| Host image | Provides the operating system, kernel, and NVIDIA kernel driver. |
| AKS Flex Node | Installs and configures the AKS worker-node components, including kubelet and containerd. |
| GPU Operator components | Configure cluster GPU discovery and runtime integration. In this flow, they do not install the host driver when driver installation is disabled. |
| NVIDIA DRA driver | Exposes GPUs through Kubernetes Dynamic Resource Allocation when DRA is used. |
| GPU Feature Discovery | Adds labels such as GPU product, driver version, and GPU count to nodes. |

## Driver and image contract

The NVIDIA driver is the main contract to get right.

AKS Flex Node assumes the host can already load the GPU kernel driver and expose NVIDIA devices. In current Flex GPU validation, this is achieved by choosing a GPU-capable DSVM/HPC image that already includes the driver stack for the target GPU family.

Valid image options include:

1. **GPU-capable DSVM/HPC image.** Use a marketplace image such as an Ubuntu HPC or data science VM image that is built for GPU workloads, then validate the driver on first boot.
2. **AKS managed GPU-capable image or image ID.** Use a validated AKS managed GPU image where the driver is provided by AKS managed GPU bootstrap. Confirm the image ID, OS, kernel, containerd version, and driver version before using it for Flex Nodes.
3. **Custom or prebaked image.** Install and bake the NVIDIA driver yourself before running AKS Flex Node, then validate the image with the exact GPU SKU.
4. **Other GPU-capable marketplace image.** Select another image only after validating that it provides the required driver, kernel modules, container runtime support, and GPU devices.

Do not rely on AKS Flex Node to repair a missing or incompatible GPU driver after bootstrap.

## Example image options

Use placeholders in automation and pin images for repeatable validation. Avoid depending on `latest` for production rollouts unless the rollout process records and validates the resolved image version.

### DSVM/HPC marketplace image

```yaml
imageReference:
  publisher: microsoft-dsvm
  offer: ubuntu-hpc
  sku: "2204"
  version: latest
securityType: Standard
```

This pattern is useful when the image already contains the expected NVIDIA driver stack for the GPU host SKU. Validate the resolved image version before broad rollout.

### AKS managed image ID

```yaml
imageID: /subscriptions/<subscription-id>/resourceGroups/<image-resource-group>/providers/Microsoft.Compute/galleries/<gallery>/images/<image-name>/versions/<image-version>
```

Use this when a specific AKS managed GPU-capable image has been validated for the target GPU family. Record the resolved node image label, OS image, kernel, containerd version, and NVIDIA driver version.

### Custom or prebaked image

```yaml
imageID: /subscriptions/<subscription-id>/resourceGroups/<resource-group>/providers/Microsoft.Compute/galleries/<gallery>/images/<custom-gpu-image>/versions/<version>
```

Use this when you own driver installation and image hardening. The image should be tested before AKS Flex Node bootstrap.

## Cluster GPU components

Install the cluster GPU components after the host image contract is clear.

Common components are:

- **NVIDIA GPU Operator:** manages GPU-related Kubernetes components. If `ClusterPolicy.spec.driver.enabled=false`, it does not install the host NVIDIA kernel driver.
- **NVIDIA Container Toolkit:** configures container runtime integration so GPU devices can be passed to containers.
- **GPU Feature Discovery (GFD):** labels GPU nodes with product, driver, runtime, and GPU count details.
- **NVIDIA DRA driver:** exposes GPU devices through Kubernetes Dynamic Resource Allocation. DRA `DeviceClass` objects commonly include names such as `gpu.nvidia.com` and `mig.nvidia.com`.

> **Note:** DRA and legacy extended resources are different scheduling paths. In DRA-based clusters, a node can have GPU labels and DRA devices even when legacy `nvidia.com/gpu` node capacity is `0`. Validate the resource path your workloads use instead of assuming legacy capacity will be populated.

## Example Flex node configuration

The exact API shape depends on the provisioning layer used with AKS Flex Node. The example below shows the important GPU fields to capture in a Karpenter-style node class and node pool. Replace names, SKUs, image values, and labels with values from your environment.

```yaml
apiVersion: karpenter.azure.com/v1alpha2
kind: AzureFlexNodeClass
metadata:
  name: gpu-flex-h100
spec:
  imageReference:
    publisher: microsoft-dsvm
    offer: ubuntu-hpc
    sku: "2204"
    version: latest
  securityType: Standard
  osDiskSizeGB: 256
  tags:
    workload: gpu
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu-flex
spec:
  template:
    metadata:
      labels:
        aks-flex-node.azure.com/gpu: "true"
    spec:
      nodeClassRef:
        group: karpenter.azure.com
        kind: AzureFlexNodeClass
        name: gpu-flex-h100
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        - key: karpenter.azure.com/sku
          operator: In
          values: ["<gpu-vm-size>"]
```

If you use a pinned image ID instead of marketplace image fields, replace `imageReference` with the validated `imageID`.

## Validation

Run these checks after the host joins the cluster.

### Confirm the Flex Node is ready

```bash
kubectl get nodes -l aks-flex-node.azure.com/gpu=true -o wide
```

Example output:

```text
NAME                  STATUS   ROLES    AGE   VERSION   INTERNAL-IP   OS-IMAGE              KERNEL-VERSION       CONTAINER-RUNTIME
gpu-flex-node-000001  Ready    <none>   18m   v1.34.x   10.0.0.10     Ubuntu 22.04.5 LTS    5.15.0-xxxx-azure    containerd://2.0.x
```

### Confirm GPU labels

```bash
kubectl get node <gpu-flex-node-name> --show-labels \
  | tr ',' '\n' \
  | grep -E 'nvidia.com/gpu|feature.node.kubernetes.io'
```

Useful labels include:

```text
nvidia.com/gpu.count=8
nvidia.com/gpu.driver.major=580
nvidia.com/gpu.driver.minor=126
nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3
feature.node.kubernetes.io/system-os_release.ID=ubuntu
```

Label names and values vary by GPU model, driver version, and GFD version. Use them as validation signals, not as a hardcoded contract.

### Confirm GPU Operator behavior

```bash
kubectl get clusterpolicy -o yaml | grep -A3 'driver:'
```

If the policy shows `enabled: false` under `spec.driver`, the GPU Operator is not installing the host NVIDIA driver. That is expected for flows where the driver comes from the image.

### Confirm DRA resources when using DRA

```bash
kubectl get deviceclasses
kubectl get resourceclaims -A
kubectl get pods -A | grep -E 'nvidia|dra|gpu'
```

Example `DeviceClass` names:

```text
gpu.nvidia.com
mig.nvidia.com
```

If your workloads use DRA, validate that the workload creates and binds the expected `ResourceClaim`. If your workloads use legacy GPU requests, validate legacy `nvidia.com/gpu` capacity separately.

### Validate on the host

On the target host, confirm the driver and runtime before blaming Kubernetes scheduling:

```bash
nvidia-smi
lsmod | grep nvidia
systemctl is-active containerd
journalctl -u aks-flex-node-agent --no-pager -n 100
```

Expected result: `nvidia-smi` can see the GPUs, NVIDIA kernel modules are loaded, containerd is active, and the AKS Flex Node agent has no bootstrap errors.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| Node is not `Ready` | Check `journalctl -u aks-flex-node-agent`, kubelet logs, outbound connectivity to the AKS API server, and bootstrap credentials. |
| Node is `Ready` but has no GPU labels | Confirm GFD is installed and running, then verify `nvidia-smi` and loaded NVIDIA kernel modules on the host. |
| GPU Operator reports driver errors | Confirm whether `spec.driver.enabled` is intended to be `false`. If driver installation is disabled, fix the host image instead of expecting the operator to install the driver. |
| Legacy `nvidia.com/gpu` capacity is `0` | Check whether the cluster uses DRA. DRA devices can be available even when legacy extended resource capacity is not populated. |
| Workload remains pending | Compare the workload's GPU request style with the cluster setup: DRA `ResourceClaim` and `DeviceClass` vs legacy `resources.limits["nvidia.com/gpu"]`. |
| Driver version differs across nodes | Confirm the image ID or resolved marketplace image version used for each node class. Pin images when repeatability matters. |

## Caveats

- AKS Flex Node does not install the NVIDIA kernel driver.
- GPU Operator driver installation may be disabled by design. Do not describe the operator as the driver installer unless `spec.driver.enabled=true` and that path has been validated.
- Image, driver, kernel, and containerd versions are part of the GPU node contract. Record them with every validation run.
- Do not hardcode validation cluster names in runbooks or automation. Use placeholders such as `<cluster-name>`, `<resource-group>`, and `<gpu-flex-node-name>`.
- Current validated Flex GPU nodes may not use newer host routing fields or other experimental CRD paths. Document those paths only when the live CRs actually use them.
