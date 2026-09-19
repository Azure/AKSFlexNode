# Labs

Use these hands-on labs to evaluate AKS Flex Node in end-to-end Azure scenarios.

> [!IMPORTANT]
> These labs cover additional configurations for evaluation. Review the status, prerequisites, versions, limitations, and operational responsibilities in each lab before you begin.

Existing files under `docs/labs/` are stable public documentation paths. Update labs in place so links from Microsoft Learn, issues, and external content continue to work.

The current refresh baseline is Unbounded `v0.8.0`, AKS Flex Node `v0.1.11`, Azure CLI `2.90.0` or later, and `aks-preview` `22.0.0b8` where Flex node pool commands are required. A lab's status block records any scenario-specific exception or incomplete validation.

## Common Prerequisites

Before starting a lab, prepare:

- An Azure subscription where you can create resource groups, virtual networks, virtual machines, AKS clusters, and private DNS links.
- Azure CLI 2.90.0 or later, signed in to the target subscription.
- `kubectl`, Helm, `curl`, and SSH/SCP tools in the Bash environment or admin VM that runs the lab commands.
- Nonoverlapping CIDR ranges for the AKS virtual network, Flex node host network, pod networks, service CIDR, and any connected networks.
- Network access from the Bash environment and flex node host to the AKS API server. For private AKS labs, run Azure CLI, `kubectl`, Helm, and bootstrap configuration commands from an environment that can resolve and reach the private API endpoint.

A lab can require additional tools and permissions. Complete its `Before you begin` or `Prerequisites` section before you create resources.

## Available Labs

| Lab | API access | Node connectivity | Networking or hardware | Status |
| --- | --- | --- | --- | --- |
| [NVIDIA GPU Flex Node setup](gpu-node-setup.md) | Determined by the base cluster | Determined by the base cluster | NVIDIA GPU | Experimental |
| [AMD GPU Flex Node setup](amd-gpu-node-setup.md) | Determined by the base cluster | Determined by the base cluster | AMD Instinct GPU | Experimental |
| [Private AKS cluster with unmanaged Cilium and a cross-region Flex node](aks-private-cluster-cilium.md) | Private | Cross-region VNet peering | Unmanaged Cilium with VXLAN | Experimental pending revalidation |
| [Private AKS cluster with Unbounded-Net and a cross-region Flex node](aks-private-cluster-unbounded-net.md) | Private | Cross-region VNet peering | Unbounded-Net private Layer 3 peering | Partially validated with `v0.8.0` |
| [Public AKS cluster with Unbounded-Net and a cross-region VNet-peered Flex node](aks-public-cluster-unbounded-net-vnet-peering.md) | Public | Cross-region VNet peering | Unbounded-Net private Layer 3 peering | Experimental pending `v0.8.0` validation |
| [AKS Flex Node with offline bootstrap artifacts](aks-public-cluster-offline-bootstrap.md) | Public in the walkthrough | Cross-region VNet peering | Filesystem or local-registry artifacts | Experimental |
| [Public AKS cluster with an Unbounded-Net WireGuard Flex node](aks-public-cluster-unbounded-net-wireguard.md) | Public | No VNet peering | Unbounded-Net public WireGuard gateway | Experimental pending `v0.8.0` validation |

## Topic Matrix

<!-- LLM agents: update this table when adding new lab docs. -->

| Topic | Lab |
| --- | --- |
| NVIDIA GPU workloads | [NVIDIA GPU Flex Node setup](gpu-node-setup.md) |
| AMD GPU workloads | [AMD GPU Flex Node setup](amd-gpu-node-setup.md) |
| Cilium CNI | [Private AKS with unmanaged Cilium](aks-private-cluster-cilium.md) |
| Private AKS API access | [Private AKS with unmanaged Cilium](aks-private-cluster-cilium.md), [Private AKS with Unbounded-Net](aks-private-cluster-unbounded-net.md) |
| Cross-region VNet peering | [Private AKS with unmanaged Cilium](aks-private-cluster-cilium.md), [Private AKS with Unbounded-Net](aks-private-cluster-unbounded-net.md), [Public AKS with Unbounded-Net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md) |
| Unbounded-Net CNI | [Private AKS with Unbounded-Net](aks-private-cluster-unbounded-net.md), [Public AKS with Unbounded-Net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md), [Public AKS with Unbounded-Net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| Public AKS API access | [Public AKS with Unbounded-Net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md), [Offline bootstrap artifacts](aks-public-cluster-offline-bootstrap.md), [Public AKS with Unbounded-Net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| WireGuard gateway connectivity | [Public AKS with Unbounded-Net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| No VNet peering | [Public AKS with Unbounded-Net WireGuard](aks-public-cluster-unbounded-net-wireguard.md) |
| Offline bootstrap artifacts | [AKS Flex Node with offline bootstrap artifacts](aks-public-cluster-offline-bootstrap.md) |
| Private Layer 3 `SitePeering` | [Private AKS with Unbounded-Net](aks-private-cluster-unbounded-net.md), [Public AKS with Unbounded-Net VNet peering](aks-public-cluster-unbounded-net-vnet-peering.md), [Offline bootstrap artifacts](aks-public-cluster-offline-bootstrap.md) |

## Lab maintenance

When you update a lab:

- Keep its existing file path and scenario identity.
- Preserve existing heading anchors when possible. If a heading changes, retain the old anchor with an explicit HTML anchor.
- Update its status and validation information only after the complete scenario passes.
- Pin or record the versions used for validation.
- Verify that cleanup removes resources created by the lab.
- Don't print or commit bootstrap data, kubeconfig content, private keys, service principal credentials, or signed URLs.
