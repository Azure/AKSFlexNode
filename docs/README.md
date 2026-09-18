# AKS Flex Node documentation

AKS Flex Node extends Azure Kubernetes Service (AKS) to virtual machines and bare metal hosts that you manage. Use this page to choose the documentation that matches your task.

> [!IMPORTANT]
> AKS Flex Node is a preview feature. Start with the documented deployment workflow and review its prerequisites and responsibility boundaries before you use a supplemental lab.

## Deploy a Flex node

The end-to-end operator workflow covers cluster preparation, Unbounded-Net, Flex node pool creation, host identity, bootstrap, and validation:

1. [Review the operator workflow](usage/getting-started.md).
2. [Compare node authentication methods](usage/joining-nodes.md).
3. [Generate a node configuration](usage/aks-flex-config.md) when the selected workflow calls for the repository helper.
4. [Validate and operate the host](usage/operations.md).

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
