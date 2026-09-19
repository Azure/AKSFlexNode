# AKS Flex Node

## Overview

AKS Flex Node extends Azure Kubernetes Service (AKS) to virtual machines and bare metal hosts that you manage. Use Flex nodes when selected workloads need compute outside standard AKS node pools, such as capacity in another region, on-premises hardware, or specialized accelerators.

The Flex Node agent prepares the host, creates an isolated Kubernetes worker environment with systemd-nspawn, joins that worker to AKS, and reconciles its configuration with the state requested by AKS. You retain responsibility for the host operating system, identity, network connectivity, and workload configuration.

> [!IMPORTANT]
> AKS Flex Node is a preview feature intended for evaluation and isn't recommended for production workloads. Review the [AKS support policy](https://learn.microsoft.com/azure/aks/support-policies) and the [Supplemental Terms of Use for Microsoft Azure Previews](https://azure.microsoft.com/support/legal/preview-supplemental-terms/) before using it.

## Key features and scenarios

- Attach customer-managed virtual machines or bare metal hosts to an AKS cluster.
- Use `amd64` or `arm64` Linux hosts running Ubuntu 24.04.
- Authenticate the host to Azure with Azure Arc managed identity, Azure VM managed identity, or a service principal.
- Run the Kubernetes worker components in an isolated nspawn environment.
- Apply node settings and lifecycle operations through an AKS Flex node pool and Machine resources.
- Use customer-managed networking to connect AKS-managed nodes, Flex nodes, pods, and Kubernetes services.
- Evaluate specialized hardware scenarios, including NVIDIA and AMD GPU hosts.

## Getting started

The primary deployment workflow uses:

- a public AKS cluster for the repository's end-to-end walkthrough;
- an AKS cluster created without a built-in network plugin;
- Unbounded-Net on the AKS-managed nodes and Flex nodes;
- private Layer 3 connectivity between the AKS-managed node network and the Flex node host network;
- a Flex node pool;
- a prepared Ubuntu 24.04 host with an Azure Arc managed identity, Azure VM managed identity, or service principal.

Plan AKS API access and node connectivity separately. A public API endpoint allows the host to contact the Kubernetes API server, but it doesn't provide connectivity between AKS-managed nodes, Flex nodes, pods, or kubelet callback endpoints.

### Understand the command environments

The deployment uses two machines:

- **Bash environment:** The workstation or admin host where you run Azure CLI, `kubectl`, artifact download, and SSH commands. For a private AKS cluster, this environment must resolve and reach the private API endpoint.
- **Flex node host:** The customer-managed Ubuntu machine that joins the cluster. Run host installation and bootstrap commands here only when the procedure explicitly directs you to.

### Complete the deployment

1. Review the [documentation index](docs/README.md) and [authentication options](docs/usages/joining-nodes.md).
2. Plan nonoverlapping AKS node, AKS pod, Flex node, Flex pod, and Kubernetes service address ranges with your network administrator.
3. Follow the [operator guide](docs/usages/operator-first-boot.md) to prepare the cluster, install Unbounded-Net, create the Flex node pool, prepare the host identity, bootstrap the host, and verify the result.
4. Use the [operations guide](docs/usages/operations.md) for inspection, upgrade, reset, uninstall, and troubleshooting tasks.

The operator workflow creates Azure and Kubernetes resources and requires more than an agent installation on an existing host. Review the complete prerequisites and cleanup steps before you begin.

### Choose a deployment option

| Decision | Options | What it controls |
| --- | --- | --- |
| Host identity | Azure Arc managed identity, Azure VM managed identity, or service principal | How the Flex Node agent authenticates to Azure and accesses its AKS Machine resource. |
| AKS API access | Public or private; the primary repository walkthrough uses public access | How the Bash environment and flex node host resolve and reach the Kubernetes API server. |
| Node connectivity | Private Layer 3 or an evaluated alternative | How AKS-managed nodes, Flex nodes, pods, services, and control-plane callbacks communicate. |

A Kubernetes bootstrap token establishes initial Kubernetes trust. It doesn't replace the Azure identity required by the primary workflow for Machine registration and reconciliation.

### Explore additional configurations

The [labs](docs/labs/README.md) cover additional configurations for evaluation, including cross-region virtual network peering, public WireGuard gateways, unmanaged Cilium, offline artifacts, and GPU hosts. Review each lab's status and version scope before use.

## Usage guides and topics

- [Documentation](docs/README.md) - Choose deployment, architecture, reference, lab, or contributor guidance.
- [Usage guide](docs/usage.md) - Find joining, configuration, command, and operations documentation.
- [Labs](docs/labs/README.md) - Evaluate additional Azure, networking, offline, and GPU scenarios.
- [Design documentation](docs/design.md) - Understand architecture, lifecycle, Azure integration, and local state.

## Development and security

- [Development guide](docs/development.md) - Build, test, and contribute to the project.
- [Security policy](SECURITY.md) - Report security vulnerabilities privately.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
