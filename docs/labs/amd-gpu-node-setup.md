# AMD GPU Flex Node setup

How to add an AMD Instinct GPU host to an AKS cluster as an AKS Flex Node.

> **Status:** AMD GPU Flex Node support is under active validation. The current validation target is Ubuntu 24.04 on AMD Instinct MI300X hosts.

## Overview

AKS Flex Node joins a prepared host to an AKS cluster. For AMD GPU hosts there are two extra responsibilities that AKS Flex Node does **not** take on:

1. The host must already have a working AMDGPU/ROCm kernel driver before bootstrap.
2. After the node joins, you must manually expose GPU devices and features in-cluster by installing the AMD components your workloads need, for example AMD GPU Operator or the AMD Kubernetes device plugin.

Plan for both before you start.

## Responsibility boundary

For managed AKS GPU node pools, the node provisioning path owns GPU driver preparation. That work belongs in the AKS image, AgentBaker, CSE, or another node-image pipeline before kubelet starts accepting GPU workloads.

For AKS Flex Node, the Flex Node agent does not own GPU driver installation. Flex Node starts from a host you already control, then joins that host to AKS. Keep AMDGPU/ROCm preparation outside `aks-flex-node start` so driver failures, kernel-header mismatches, Secure Boot signing, apt-source policy, and required reboots are handled before the node bootstrap path.

Use this contract:

- Host preparation installs and validates AMDGPU/ROCm.
- AKS Flex Node joins the already prepared host to AKS.
- The cluster GPU stack exposes devices to Kubernetes after the node is `Ready`.

## Validated scope

This guide has been validated for the host preparation portion on:

- VM size: `Standard_ND96isr_MI300X_v5`
- Region: `francecentral`
- OS image: Ubuntu 24.04 marketplace image
- Kernel tested: `6.17.0-1018-azure`
- ROCm package stream: `7.2.4`
- AMDGPU package stream: `30.30.4`

The validation installed the minimal host package set, loaded the AMDGPU driver, rebooted the VM, and verified that ROCm still detected all 8 MI300X devices after reboot.

## Before you begin

- An Azure subscription and AKS cluster with `kubectl` admin access.
- Azure CLI logged in to the target subscription.
- `kubectl`, Helm, `curl`, and SSH/SCP tooling on your workstation.
- An AMD Instinct GPU host with root or sudo access and outbound reach to the AKS API server.
- A host image or host preparation script that installs and validates ROCm.
- Non-overlapping network ranges for the AKS cluster, host network, pods, services, and any connected networks.

## Driver and image contract

AKS Flex Node does **not** install the AMDGPU kernel driver or ROCm packages. Pick an image where the driver is already baked in, or run a host preparation step before Flex Node bootstrap. The benefits:

- No first-boot driver build or DKMS failure during Flex Node bootstrap.
- Deterministic driver version across nodes.
- Faster `Ready` time.
- Works in restricted networks when the image already contains the needed packages.
- Failures point at the host preparation contract, not at Flex Node bootstrap.

If the image has no driver, you own driver installation, signing for Secure Boot, kernel-header matching, and kernel-update rebuilds.

### Image options

1. **Custom Ubuntu 24.04 image with a minimal ROCm host package set.** This is the preferred production contract after validation. Bake only the host packages needed for Kubernetes workloads and validation, not the full ROCm developer stack.
2. **AgentBaker-style host preparation for validation.** For early testing, run the same AMD CSE preparation flow that installs the minimal AMDGPU/ROCm host packages and validates the device nodes before running AKS Flex Node.
3. **Other GPU marketplace or partner images.** Treat as candidates to validate per region, OS, kernel, GPU family, and ROCm version.

For MI300X validation, the minimal host package families are:

- `linux-headers-$(uname -r)` and `linux-modules-extra-$(uname -r)`
- `amdgpu-dkms`
- `libdrm-amdgpu-dev`
- `rocm-core`
- `rocminfo`
- `rocm-smi-lib`

Pin exact package versions for repeatable validation runs. For production, use a Microsoft-controlled package source or a prebaked image instead of depending on a third-party apt source at node boot.

## Host preparation example

Use this example only as a validation path for Ubuntu 24.04 MI300X hosts. In production, prefer a prebaked image or a Microsoft-controlled package mirror so Flex Node bootstrap does not depend on `repo.radeon.com` at runtime.

This script intentionally installs only the host packages needed to load and validate AMDGPU/ROCm. It does not install the full ROCm developer stack.

```bash
set -euo pipefail

ROCM_VERSION="7.2.4"
AMDGPU_REPO_VERSION="30.30.4"
AMDGPU_DKMS_VERSION="1:6.16.13.30300400-2341068.24.04"
LIBDRM_AMDGPU_DEV_VERSION="1:2.4.125.07020400-2341098.24.04"
ROCM_CORE_VERSION="7.2.4.70204-93~24.04"
ROCMINFO_VERSION="1.0.0.70204-93~24.04"
ROCM_SMI_LIB_VERSION="7.8.0.70204-93~24.04"
ROCM_KEY_FINGERPRINT="CA8BB4727A47B4D09B4EE8969386B48A1A693C5C"

. /etc/os-release
if [ "${ID}" != "ubuntu" ] || [ "${VERSION_ID}" != "24.04" ]; then
  echo "This validation path is only for Ubuntu 24.04. Found ${ID} ${VERSION_ID}." >&2
  exit 1
fi

sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSLo /tmp/rocm.gpg.key https://repo.radeon.com/rocm/rocm.gpg.key
actual_fingerprint="$(gpg --show-keys --with-colons /tmp/rocm.gpg.key | awk -F: '$1 == "fpr" { print $10; exit }')"
if [ "${actual_fingerprint}" != "${ROCM_KEY_FINGERPRINT}" ]; then
  echo "Unexpected ROCm GPG key fingerprint: ${actual_fingerprint}" >&2
  exit 1
fi
sudo gpg --dearmor --yes -o /etc/apt/keyrings/rocm.gpg /tmp/rocm.gpg.key
rm -f /tmp/rocm.gpg.key

cat <<EOF | sudo tee /etc/apt/sources.list.d/rocm.list >/dev/null
deb [arch=amd64 signed-by=/etc/apt/keyrings/rocm.gpg] https://repo.radeon.com/rocm/apt/${ROCM_VERSION} noble main
deb [arch=amd64 signed-by=/etc/apt/keyrings/rocm.gpg] https://repo.radeon.com/graphics/${ROCM_VERSION}/ubuntu noble main
EOF

cat <<EOF | sudo tee /etc/apt/sources.list.d/amdgpu.list >/dev/null
deb [arch=amd64 signed-by=/etc/apt/keyrings/rocm.gpg] https://repo.radeon.com/amdgpu/${AMDGPU_REPO_VERSION}/ubuntu noble main
EOF

cat <<EOF | sudo tee /etc/apt/preferences.d/repo-radeon-pin-600 >/dev/null
Package: *
Pin: release o=repo.radeon.com
Pin-Priority: 600
EOF

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  "linux-headers-$(uname -r)" \
  "linux-modules-extra-$(uname -r)"

sudo apt-get install -y --no-install-recommends \
  "amdgpu-dkms=${AMDGPU_DKMS_VERSION}" \
  "libdrm-amdgpu-dev=${LIBDRM_AMDGPU_DEV_VERSION}" \
  "rocm-core=${ROCM_CORE_VERSION}" \
  "rocminfo=${ROCMINFO_VERSION}" \
  "rocm-smi-lib=${ROCM_SMI_LIB_VERSION}"

sudo ldconfig
echo amdgpu | sudo tee /etc/modules-load.d/amdgpu.conf >/dev/null
sudo modprobe amdgpu
```

After installation, clean the temporary AMD apt source if the host will not use it for future patching:

```bash
sudo rm -f /etc/apt/sources.list.d/rocm.list
sudo rm -f /etc/apt/sources.list.d/amdgpu.list
sudo rm -f /etc/apt/preferences.d/repo-radeon-pin-600
sudo rm -f /etc/apt/keyrings/rocm.gpg
```

Validate the host before running AKS Flex Node. Run ROCm validation commands as root, or make sure the user is in the group that can access `/dev/kfd` and `/dev/dri/renderD*`.

```bash
dkms status amdgpu
modinfo amdgpu | head
lsmod | grep '^amdgpu'
ls -l /dev/kfd /dev/dri/renderD*
sudo /opt/rocm/bin/rocm-smi --showproductname
sudo /opt/rocm/bin/rocminfo | grep -E 'Marketing Name:|gfx942'
```

For `Standard_ND96isr_MI300X_v5`, expect 8 `AMD Instinct MI300X VF` devices and 8 `gfx942` entries.

Reboot once and repeat the validation commands before joining the host. This catches module autoload, kernel/initramfs, and device-node issues before Flex Node bootstrap.

## Cluster GPU stack (manual)

After the Flex node is `Ready`, **you must install the cluster AMD GPU stack yourself**. AKS Flex Node does not deploy any of this. Use one of these paths:

- **AMD GPU Operator** - recommended when you want operator-managed device plugin, node labels, metrics, tests, and optional DRA support. Keep host driver ownership explicit; if the driver is already prepared on the host, configure the operator so it does not replace that contract.
- **AMD Kubernetes device plugin** - lighter manual path when you only need legacy Kubernetes device plugin resources.

Example AMD GPU Operator install:

```bash
helm repo add rocm https://rocm.github.io/gpu-operator
helm repo update

helm install amd-gpu-operator rocm/gpu-operator-charts \
  --namespace kube-amd-gpu \
  --create-namespace
```

Example lightweight device plugin install:

```bash
kubectl create -f https://raw.githubusercontent.com/ROCm/k8s-device-plugin/master/k8s-ds-amdgpu-dp.yaml
kubectl create -f https://raw.githubusercontent.com/ROCm/k8s-device-plugin/master/k8s-ds-amdgpu-labeller.yaml
```

Use the AMD GPU Operator DRA driver only when your workloads request DRA `DeviceClass` resources. If your workloads request legacy `amd.com/gpu` resources, install the device plugin path.

## Provisioning path

Use direct host bootstrap: create the AMD GPU VM or bare metal host yourself, prepare and validate ROCm, then run AKS Flex Node bootstrap on that host.

## Direct host bootstrap

Use direct host bootstrap when you manage the GPU host lifecycle directly. This path is useful for a single validation VM, a manually provisioned bare metal host, or an environment where another system owns VM creation.

### 1. Provision an AMD GPU host

Create the VM or prepare the bare metal host with:

- Ubuntu 24.04 for the current MI300X validation path.
- Outbound HTTPS reachability to the AKS API server.
- A ROCm-capable image or host preparation script.
- Any host-specific networking required to reach the AKS VNet, overlay, or gateway.

Before running AKS Flex Node, confirm the host driver works:

```bash
lsmod | grep amdgpu
ls -l /dev/kfd /dev/dri/renderD*
sudo /opt/rocm/bin/rocminfo | grep -E 'Name:|Marketing Name:|gfx'
sudo /opt/rocm/bin/rocm-smi --showproductname
```

If these fail, fix the image or driver installation first. AKS Flex Node bootstrap should not be the first component to discover a missing or mismatched driver.

### 2. Prepare AKS bootstrap credentials

On your workstation, use `aks-flex-config` to create the bootstrap RBAC and render a host config. This is the same setup used by the general node-joining flow; the AMD-specific requirement is that the target host image already has working ROCm driver support.

```bash
RESOURCE_GROUP="<aks-resource-group>"
CLUSTER_NAME="<aks-cluster-name>"
SUBSCRIPTION_ID="<subscription-id>"
AGENT_POOL_NAME="${AGENT_POOL_NAME:-aksflexnodes}"

curl -fsSLo ./aks-flex-config https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/aks-flex-config
chmod +x ./aks-flex-config

./aks-flex-config setup-node-rbac \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID"

./aks-flex-config generate-node-config \
  --resource-group "$RESOURCE_GROUP" \
  --cluster-name "$CLUSTER_NAME" \
  --subscription "$SUBSCRIPTION_ID" \
  --agent-pool-name "$AGENT_POOL_NAME" \
  --bootstrap-token \
  --output ./aks-flex-node-config.json
```

Copy `./aks-flex-node-config.json` to the AMD GPU host.

### 3. Install AKS Flex Node on the host

```bash
sudo su
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash
aks-flex-node version
```

### 4. Write the host config

```bash
TARGET_HOST="<user>@<host>"
scp ./aks-flex-node-config.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

On the AMD GPU host:

```bash
sudo su
install -d -m 0755 /etc/aks-flex-node
install -m 0600 /tmp/aks-flex-node-config.json /etc/aks-flex-node/config.json
cat /etc/aks-flex-node/config.json
```

### 5. Bootstrap and watch the node

```bash
# Keep bootstrap-created nspawn rootfs paths traversable by non-root service users.
umask 022
aks-flex-node start --config /etc/aks-flex-node/config.json
journalctl -u aks-flex-node-agent -f
```

From your workstation:

```bash
kubectl get nodes -o wide
kubectl describe node <amd-gpu-flex-node-name>
```

After the node is `Ready`, install the cluster AMD GPU stack from the **Cluster GPU stack (manual)** section if it is not already installed. The host driver is local to the node; AMD GPU Operator, device plugin, node labeller, metrics, and optional DRA are cluster components.

## Validation

```bash
# Node Ready, then identify your AMD GPU node name.
kubectl get nodes -o wide

# GPU capacity from the device plugin path.
kubectl describe node <amd-gpu-flex-node-name> | grep -A5 -E 'Capacity|Allocatable|amd.com/gpu'

# Host driver and runtime.
lsmod | grep amdgpu
ls -l /dev/kfd /dev/dri/renderD*
sudo /opt/rocm/bin/rocm-smi --showproductname
sudo /opt/rocm/bin/rocminfo | grep -E 'Marketing Name:|gfx'
systemctl is-active containerd aks-flex-node-agent
```

Expect: node `Ready`, `amd.com/gpu` capacity present when the device plugin path is installed, ROCm tools list the GPUs, and agent plus containerd are active.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Node not `Ready` | `journalctl -u aks-flex-node-agent`, API-server reachability, bootstrap creds. |
| Node `Ready`, no GPU capacity | AMD GPU Operator or device plugin installed? `/dev/kfd` and render nodes present on host? |
| Operator selects no nodes | Check node labels such as `feature.node.kubernetes.io/amd-gpu` or `feature.node.kubernetes.io/amd-vgpu`, then adjust the operator `DeviceConfig` selector. |
| Pods pending for GPU | Workload uses DRA but DRA driver is not installed, or uses legacy `amd.com/gpu` but only DRA is installed. Match request style to install. |
| ROCm validation fails after kernel update | Rebuild or repave from an image whose driver matches the running kernel. |
| Driver version drift | Pin the image version and ROCm package versions. |

## Caveats

- AKS Flex Node does not install the AMDGPU kernel driver or ROCm packages.
- AKS Flex Node does not install AMD GPU Operator, AMD device plugin, node labeller, metrics exporter, or DRA driver.
- Image + driver + kernel + containerd versions are part of the AMD GPU node contract. Record them per validation run.
- The MI300X path is the first validation target. Validate other AMD GPU families before using this document as a production runbook for them.
