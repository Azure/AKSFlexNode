# AKS Flex Node documentation

AKS Flex Node extends Azure Kubernetes Service (AKS) to virtual machines and bare metal hosts that you manage. Use this page to choose the documentation that matches your task.

> [!IMPORTANT]
> AKS Flex Node is a preview feature. Start with the documented deployment workflow and review its prerequisites and responsibility boundaries before you use a supplemental lab.

## Plan a deployment

Make these decisions separately before you create resources:

| Decision | Examples |
| --- | --- |
| Host identity | Azure Arc managed identity, Azure VM managed identity, or service principal |
| AKS API access | Public or private API endpoint |
| Node connectivity | Private Layer 3 connectivity or an evaluated gateway topology |

Start with [Joining nodes](usage/joining-nodes.md) to understand the Azure identity and Kubernetes bootstrap credentials. A public API endpoint doesn't provide the network paths required between nodes, pods, services, and kubelet callback endpoints.

## Deploy a Flex node

The [end-to-end operator guide](usage/getting-started.md) covers cluster preparation, Unbounded-Net, Flex node pool creation, host identity, bootstrap, and validation. Use the [operations guide](usage/operations.md) after the node is attached.

Use the Microsoft Learn Flex nodes article series as the primary Azure deployment guidance. Repository labs cover additional configurations for evaluation.

## Understand the system

- [Architecture overview](design.md) explains the host, nspawn worker, AKS, and lifecycle model.
- [AKS resource provider and agent interaction](design/agent-and-aks.md) describes machine goal state and lifecycle signals.
- [In-cluster machine flow](design/in-cluster-machine.md) describes the E2E and development backend.
- [Bootstrap script design](design/storage-backed-bootstrap.md) describes generated and runtime-fetched bootstrap data.
- [Project Unbounded documentation](https://github.com/Azure/unbounded/tree/main/docs) explains shared Unbounded components, Sites, networking resources, and operations.

## Find reference information

- [Command-line reference](usage/cli.md) lists operator commands, flags, aliases, and internal service commands.
- [Configuration](usage/configuration.md) lists the AKS Flex Node JSON configuration.
- [Operations](usage/operations.md) covers preflight, startup, agent upgrade, reset, and troubleshooting.
- [Usage guide index](usage.md) links to task-oriented guidance.

## Try supplemental scenarios

The [lab index](labs/README.md) covers private and public cluster networking, VNet peering, WireGuard, Cilium, offline artifacts, and GPU hosts. Each lab states its scope and validation status. Review those statements before you create resources.

## Contribute

- [Development guide](development.md) covers builds, tests, CI, and documentation changes.
- [E2E test guide](../hack/e2e/README.md) describes the Azure validation suite.
- [QEMU local VM guide](../hack/qemu/README.md) describes the local development VM.

## Report problems

Use [GitHub Issues](https://github.com/Azure/AKSFlexNode/issues) for reproducible product or documentation problems. Don't include access tokens, bootstrap data, kubeconfig content, private keys, certificates with private keys, service principal credentials, signed URLs, or other secrets.

Report security vulnerabilities according to the repository [security policy](../SECURITY.md), not through a public issue.
