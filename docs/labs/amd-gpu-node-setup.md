# AMD GPU Flex Node setup

How to add an AMD Instinct GPU host to an AKS cluster as an AKS Flex Node.

> **Status:** AMD GPU Flex Node support is under active validation. The current validated host-preparation path is Ubuntu 24.04 on AMD Instinct MI300X hosts. Other AMD GPU families, OS images, and kernel versions are candidates to validate against the same prepared-host contract.

## Overview

AKS Flex Node joins a prepared host to an AKS cluster. For AMD GPU hosts there are two extra responsibilities that AKS Flex Node does **not** take on:

1. The host must already have a working AMDGPU/ROCm kernel driver before bootstrap.
2. After the node joins, you must manually expose GPU devices and features in-cluster by installing the AMD components your workloads need, for example AMD GPU Operator or the AMD Kubernetes device plugin.

Plan for both before you start.

## Responsibility boundary

For managed AKS GPU node pools, the node provisioning path owns GPU driver preparation. For external Flex Node hosts, use your own host image or host-preparation pipeline before kubelet starts accepting GPU workloads.

For AKS Flex Node, the Flex Node agent does not own GPU driver installation. Flex Node starts from a host you already control, then joins that host to AKS. Keep AMDGPU/ROCm preparation outside `aks-flex-node start` so driver failures, kernel-header mismatches, Secure Boot signing, apt-source policy, and required reboots are handled before the node bootstrap path.

Use this contract:

- Host preparation installs and validates AMDGPU/ROCm.
- AKS Flex Node joins the already prepared host to AKS.
- The cluster GPU stack exposes devices to Kubernetes after the node is `Ready`.

## Validated scope and OS policy

AKS Flex Node has the same bootstrap contract across GPU vendors: join a prepared host after the GPU driver works. The OS-specific work is the host image and driver preparation before `aks-flex-node start`.

This guide has been validated for the host preparation portion on:

- VM size: `Standard_ND96isr_MI300X_v5`
- Region: `francecentral`
- OS image: Ubuntu 24.04 marketplace image
- Kernel tested: `6.17.0-1018-azure`
- ROCm package stream: `7.2.4`
- AMDGPU package stream: `30.30.4`

The validation installed the minimal host package set, removed the Ubuntu cloud image `amdgpu` blacklist entry, loaded the AMDGPU driver, rebooted the VM, and verified that ROCm still detected all 8 MI300X devices after reboot.

Treat other combinations as separate validation targets. Do not assume this exact apt package set works on Ubuntu 22.04, non-Ubuntu distributions, different kernels, or other AMD GPU families without a clean install plus reboot validation.

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

1. **Custom ROCm-capable image with a minimal host package set.** This is the preferred production contract after validation. Bake only the host packages needed for Kubernetes workloads and validation, not the full ROCm developer stack. The current validated base is Ubuntu 24.04.
2. **Other OS releases, GPU marketplace images, or partner images.** Treat as candidates to validate per region, OS, kernel, GPU family, and ROCm version.

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

For other OS images, keep the same prepared-host contract but replace this block with an OS-specific installation path and record the validated kernel, package stream, GPU family, and reboot result.

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
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  "linux-headers-$(uname -r)" \
  "linux-modules-extra-$(uname -r)"

sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  "amdgpu-dkms=${AMDGPU_DKMS_VERSION}" \
  "libdrm-amdgpu-dev=${LIBDRM_AMDGPU_DEV_VERSION}" \
  "rocm-core=${ROCM_CORE_VERSION}" \
  "rocminfo=${ROCMINFO_VERSION}" \
  "rocm-smi-lib=${ROCM_SMI_LIB_VERSION}"

sudo ldconfig

# Ubuntu 24.04 cloud images can deny-list amdgpu by default. Remove only the
# amdgpu deny-list entry; leave other entries such as radeon in place.
amdgpu_blacklist_files="$(mktemp)"
sudo grep -RslE '^[[:space:]]*blacklist[[:space:]]+amdgpu([[:space:]]|$)' \
  /etc/modprobe.d /lib/modprobe.d /usr/lib/modprobe.d \
  > "${amdgpu_blacklist_files}" 2>/dev/null || true
while IFS= read -r blacklist_file; do
  [ -n "${blacklist_file}" ] || continue
  sudo sed -i.bak -E '/^[[:space:]]*blacklist[[:space:]]+amdgpu([[:space:]]|$)/d' "${blacklist_file}"
done < "${amdgpu_blacklist_files}"
rm -f "${amdgpu_blacklist_files}"

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
if modprobe -c | grep -E '^[[:space:]]*blacklist[[:space:]]+amdgpu'; then
  echo "amdgpu is still deny-listed" >&2
  exit 1
fi
lsmod | grep '^amdgpu'
ls -l /dev/kfd /dev/dri/renderD*
sudo /opt/rocm/bin/rocm-smi --showproductname
sudo /opt/rocm/bin/rocminfo | grep -E 'Marketing Name:|gfx942'
```

For `Standard_ND96isr_MI300X_v5`, expect 8 `AMD Instinct MI300X VF` devices and 8 `gfx942` entries.

Reboot once and repeat the validation commands before joining the host. This catches module autoload, kernel/initramfs, and device-node issues before Flex Node bootstrap.

Do not add `amdgpu` to `/etc/modules-load.d` for this validation path. After the blacklist entry is removed, PCI device discovery loads the driver after reboot without forcing it through the early `systemd-modules-load` path.

## Cluster GPU stack (manual)

After the Flex node is `Ready`, **you must install the cluster AMD GPU stack yourself**. AKS Flex Node does not deploy any of this. Use one of these paths:

- **AMD GPU Operator** - recommended when you want operator-managed device plugin, node labels, metrics, tests, and optional DRA support. The configuration below disables operator driver and KMM management so the preinstalled host driver remains outside the operator's ownership.
- **AMD Kubernetes device plugin** - lighter manual path when you only need standard Kubernetes extended resources such as `amd.com/gpu`.

### AMD GPU Operator

This path pins AMD GPU Operator chart `v1.5.0`, cert-manager `v1.15.1`, and device plugin `1.31.0.10`. These versions and the Kubernetes 1.29+ requirement come from the [AMD GPU Operator v1.5.0 installation guide](https://github.com/ROCm/gpu-operator/blob/v1.5.0/docs/installation/kubernetes-helm.md). Install cert-manager first because the operator uses it for webhook certificates:

```bash
helm repo add jetstack https://charts.jetstack.io --force-update
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --version v1.15.1 \
  --set crds.enabled=true \
  --wait

helm repo add rocm https://rocm.github.io/gpu-operator
helm repo update

cat > amd-gpu-operator-values.yaml <<'EOF'
# Standard_ND96isr_MI300X_v5 exposes MI300X VFs. NFD labels those devices
# amd-vgpu, while the chart's default DeviceConfig selects amd-gpu.
crds:
  defaultCR:
    # Avoid Helm map merging with the chart's amd-gpu selector. Apply the
    # MI300X VF DeviceConfig explicitly after the operator is ready.
    install: false
kmm:
  enabled: false
  watch: false
remediation:
  enabled: false
  installCRDs: false
EOF

helm upgrade --install amd-gpu-operator rocm/gpu-operator-charts \
  --namespace kube-amd-gpu \
  --create-namespace \
  --version v1.5.0 \
  --values amd-gpu-operator-values.yaml \
  --wait

cat <<'EOF' | kubectl apply -f -
apiVersion: amd.com/v1alpha1
kind: DeviceConfig
metadata:
  name: mi300x-vf
  namespace: kube-amd-gpu
spec:
  selector:
    feature.node.kubernetes.io/amd-vgpu: "true"
  driver:
    # The host preparation contract owns AMDGPU. Never replace it in-cluster.
    enable: false
    blacklist: false
  devicePlugin:
    enableDevicePlugin: true
    devicePluginImage: rocm/k8s-device-plugin:1.31.0.10
    nodeLabellerImage: rocm/k8s-device-plugin:labeller-1.31.0.10
  metricsExporter:
    enable: false
  draDriver:
    enable: false
EOF

kubectl get deviceconfig -n kube-amd-gpu mi300x-vf -o yaml
kubectl get pods -n kube-amd-gpu -o wide
```

If your AMD host has the physical-GPU label `feature.node.kubernetes.io/amd-gpu=true` instead, change the selector in the `DeviceConfig` manifest. Confirm the actual label before installation with `kubectl get node <amd-gpu-flex-node-name> --show-labels`.

### Lightweight device plugin

The alternative below pins the upstream manifests to the `v1.31.0.10` release tag rather than tracking `master`. It does not install or manage the host driver:

```bash
AMD_DEVICE_PLUGIN_VERSION="v1.31.0.10"
kubectl apply -f "https://raw.githubusercontent.com/ROCm/k8s-device-plugin/${AMD_DEVICE_PLUGIN_VERSION}/k8s-ds-amdgpu-dp.yaml"
kubectl apply -f "https://raw.githubusercontent.com/ROCm/k8s-device-plugin/${AMD_DEVICE_PLUGIN_VERSION}/k8s-ds-amdgpu-labeller.yaml"
```

Use the AMD GPU Operator DRA driver only when your workloads request DRA `DeviceClass` resources. If your workloads request standard `amd.com/gpu` extended resources, install the device plugin path.

## Provisioning path

Use direct host bootstrap: create the AMD GPU VM or bare metal host yourself, prepare and validate ROCm, then run AKS Flex Node bootstrap on that host.

## Direct host bootstrap

Use direct host bootstrap when you manage the GPU host lifecycle directly. This path is useful for a single validation VM, a manually provisioned bare metal host, or an environment where another system owns VM creation.

### 1. Provision an AMD GPU host

Create the VM or prepare the bare metal host with:

- An OS image whose AMDGPU/ROCm driver path has been validated for the target GPU family. Use Ubuntu 24.04 for the current MI300X validation path.
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

AKS_FLEX_NODE_VERSION="v0.14"
curl -fsSLo ./aks-flex-config \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/aks-flex-config"
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
AKS_FLEX_NODE_VERSION="v0.14"
curl -fsSL \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/install.sh" | bash
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
# Do not print this file: azure.bootstrapToken.token is a credential.
stat -c '%a %U:%G %n' /etc/aks-flex-node/config.json
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

The following validation uses a ROCm `7.2.4` userspace image to match the documented host package stream. Flex Node exposes the AMD device files and sysfs data; it does not add ROCm libraries to workload images. Each GPU workload image must contain a ROCm userspace compatible with the host driver.

```bash
# Node Ready, then identify your AMD GPU node name.
kubectl get nodes -o wide

# GPU capacity from the device plugin path.
kubectl describe node <amd-gpu-flex-node-name> | grep -A5 -E 'Capacity|Allocatable|amd.com/gpu'

# Run a real in-Pod diagnostic while requesting one GPU.
cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: amd-rocm-validation
spec:
  restartPolicy: Never
  nodeSelector:
    feature.node.kubernetes.io/amd-vgpu: "true"
  containers:
  - name: rocm
    image: rocm/dev-ubuntu-24.04:7.2.4
    command: ["bash", "-ceu", "rocm-smi --showproductname; rocminfo | grep -E 'Marketing Name:|gfx942'"]
    resources:
      limits:
        amd.com/gpu: "1"
EOF
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/amd-rocm-validation --timeout=5m
kubectl logs amd-rocm-validation

# On the Flex Node host, validate the driver and the two service boundaries.
lsmod | grep amdgpu
ls -l /dev/kfd /dev/dri/renderD*
sudo /opt/rocm/bin/rocm-smi --showproductname
sudo /opt/rocm/bin/rocminfo | grep -E 'Marketing Name:|gfx'
systemctl is-active aks-flex-node-agent
active_machine="$(machinectl list --no-legend | awk '$1 == "kube1" || $1 == "kube2" { print $1; exit }')"
test -n "${active_machine}"
systemctl --machine="${active_machine}" is-active containerd
```

For the documented `Standard_ND96isr_MI300X_v5` SKU, expect the node to be `Ready`, `amd.com/gpu` capacity and allocatable to equal `8`, the validation Pod to complete using one assigned GPU, the host agent to be active, and containerd to be active inside the current `kube1` or `kube2` nspawn machine. Delete the validation Pod when finished with `kubectl delete pod amd-rocm-validation`.

The recorded evidence for this guide covers host preparation and post-reboot detection of 8 MI300X VFs. End-to-end Flex Node, operator/device-plugin, allocatable-resource, and workload-Pod validation still requires an authorized MI300X test environment; do not treat the expected results above as recorded test results.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Node not `Ready` | `journalctl -u aks-flex-node-agent`, API-server reachability, bootstrap creds. |
| Node `Ready`, no GPU capacity | AMD GPU Operator or device plugin installed? `/dev/kfd` and render nodes present on host? |
| Operator selects no nodes | Check node labels such as `feature.node.kubernetes.io/amd-gpu` or `feature.node.kubernetes.io/amd-vgpu`, then adjust the operator `DeviceConfig` selector. |
| Pods pending for GPU | Workload uses DRA but DRA driver is not installed, or uses standard `amd.com/gpu` extended resources but only DRA is installed. Match request style to install. |
| ROCm validation fails after kernel update | Repair or replace the external host with an image whose driver matches the running host kernel. A Flex Node nspawn repave replaces the Kubernetes worker userspace, not the host kernel or AMDGPU driver. |
| Driver version drift | Pin the image version and ROCm package versions. |

## Caveats

- AKS Flex Node does not install the AMDGPU kernel driver or ROCm packages.
- AKS Flex Node does not install AMD GPU Operator, AMD device plugin, node labeller, metrics exporter, or DRA driver.
- Workload containers must provide a compatible ROCm userspace; Flex Node only makes the host devices and sysfs data available to the Kubernetes worker and scheduled containers.
- Image + driver + kernel + containerd versions are part of the AMD GPU node contract. Record them per validation run.
- The MI300X path is the first validation target. Validate other AMD GPU families before using this document as a production runbook for them.
