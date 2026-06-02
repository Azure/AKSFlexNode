# Labs

Hands-on labs for trying AKS Flex Node in end-to-end Azure scenarios.

## Available Labs

- [GPU Flex Node setup](gpu-node-setup.md) - GPU host image and driver contract, cluster GPU stack, validation, and troubleshooting.
- [Private AKS Cluster With Cross-Region Flex Node](aks-private-cluster-kubenet.md) - Private AKS with `kubenet`, cross-region VNet peering, and AKS-managed `kube-proxy` on the Flex Node.
- [Private AKS Cluster With Unbounded-Net And Cross-Region Flex Node](aks-private-cluster-unbounded-net.md) - Private AKS with `--network-plugin none`, `unbounded-net`, and private-L3 site peering.

## Topic Matrix

<!-- LLM agents: update this table when adding new lab docs. -->

| Topic | Lab |
| --- | --- |
| GPU workloads | [GPU Flex Node setup](gpu-node-setup.md) |
| Private AKS API access | [Private AKS with kubenet](aks-private-cluster-kubenet.md), [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md) |
| Cross-region VNet peering | [Private AKS with kubenet](aks-private-cluster-kubenet.md), [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md) |
| AKS-managed `kube-proxy` on Flex Nodes | [Private AKS with kubenet](aks-private-cluster-kubenet.md) |
| `unbounded-net` CNI | [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md) |
| Private-L3 `SitePeering` | [Private AKS with unbounded-net](aks-private-cluster-unbounded-net.md) |
