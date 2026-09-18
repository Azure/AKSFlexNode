# AKS Flex Node usage

Use these guides to plan, attach, configure, and operate an AKS Flex Node host.

> [!IMPORTANT]
> AKS Flex Node is a preview feature intended for evaluation. The primary repository walkthrough uses a public AKS cluster without a built-in network plugin, Unbounded-Net, private Layer 3 connectivity between node networks, a Flex node pool, and a host identity authorized for the target cluster.

## Start here

1. [Review the operator guide](usage/getting-started.md) for the complete cluster-to-host workflow.
2. [Choose a host identity](usage/joining-nodes.md) for Azure Machine registration and reconciliation.
3. [Review the configuration reference](usage/configuration.md) before applying overrides.
4. [Use the operations guide](usage/operations.md) after the node is attached.

## Guides

- [Bootstrap an AKS Flex Node](usage/getting-started.md) - Prepare the cluster, networking, pool, host identity, and agent, and then validate the node.
- [Joining Nodes](usage/joining-nodes.md) - Understand Azure host identity and Kubernetes bootstrap credentials.
- [AKS Flex Config Helper](usage/aks-flex-config.md) - Generate bootstrap-token configurations for repository evaluation workflows.
- [Operations](usage/operations.md) - Start, inspect, reset, uninstall, and troubleshoot a Flex Node host.
- [Command-line Reference](usage/cli.md) - Review operator commands, flags, compatibility aliases, and internal service commands.
- [Configuration](usage/configuration.md) - Review the configuration file structure, defaults, and validation rules.

## Additional scenarios

The following labs cover additional configurations for evaluation:

- [NVIDIA GPU Flex Node setup](labs/gpu-node-setup.md)
- [AMD GPU Flex Node setup](labs/amd-gpu-node-setup.md)
- [Networking, private cluster, and offline artifact labs](labs/README.md)
