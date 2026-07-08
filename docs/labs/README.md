# Labs

Hands-on labs for trying AKS Flex Node in end-to-end Azure scenarios.

## Common Prerequisites

Before starting a lab, prepare:

- An Azure subscription where you can create resource groups, VNets, VMs, AKS clusters, and private DNS links.
- Azure CLI logged in to the target subscription.
- `kubectl`, Helm, `curl`, and SSH/SCP tooling on the workstation or admin VM that will run the lab commands.
- Non-overlapping CIDR ranges for the AKS VNet, Flex VM VNet, pod networks, service CIDR, and any connected networks.
- Network access from the command runner and Flex VM to the AKS API server. For private AKS labs, run `kubectl`, Helm, and bootstrap config commands from a machine that can resolve and reach the private API endpoint.

## Available Labs

- [NVIDIA GPU Flex Node setup](gpu-node-setup.md) - NVIDIA host image and driver contract, cluster GPU stack, validation, and troubleshooting.
- [AMD GPU Flex Node setup](amd-gpu-node-setup.md) - AMD Instinct and ROCm host preparation contract, cluster GPU stack, validation, and troubleshooting.
- [Private AKS Cluster With Unmanaged Cilium And Cross-Region Flex Node](aks-private-cluster-cilium.md) - Private AKS with `--network-plugin none`, unmanaged Cilium, and VXLAN pod networking.
- [Private AKS Cluster With Unbounded-Net And Cross-Region Flex Node](aks-private-cluster-unbounded-net.md) - Private AKS with `--network-plugin none`, `unbounded-net`, and private-L3 site peering.
- [Public AKS Cluster With Unbounded-Net And Cross-Region VNet-Peered Flex Node](aks-public-cluster-unbounded-net-vnet-peering.md) - Public AKS with `--network-plugin none`, `unbounded-net`, and private-L3 site peering over cross-region VNet peering.
- [Public AKS Cluster With Unbounded-Net WireGuard Flex Node](aks-public-cluster-unbounded-net-wireguard.md) - Public AKS with `--network-plugin none`, `unbounded-net`, and WireGuard gateway connectivity without VNet peering.

## Topic Matrix

<!-- LLM agents: update this table when adding new lab docs. -->

| Topic | Lab |
| --- | --- |
| NVIDIA GPU workloads | [NVIDIA GPU Flex Node setup](gpu-node-setup.md) |
| AMD GPU workloads | [AMD GPU Flex Node setup](amd-gpu-node-setup.md) |
| Cilium CNI | [Private AKS with unmanaged Cilium](aks-private-cluster-cilium.md) |
| Private AKS API access | [Private AKS with unmanaged Cilium](aks-private-cluster-cilium.md), [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md) |
| Cross-region VNet peering | [Private AKS with unmanaged Cilium](aks-private-cluster-cilium.md), [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md), [Public AKS with unbounded-net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md) |
| `unbounded-net` CNI | [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md), [Public AKS with unbounded-net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md), [Public AKS with unbounded-net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| Public AKS API access | [Public AKS with unbounded-net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md), [Public AKS with unbounded-net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| WireGuard gateway connectivity | [Public AKS with unbounded-net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| No VNet peering | [Public AKS with unbounded-net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| Private-L3 `SitePeering` | [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md), [Public AKS with unbounded-net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md) |
