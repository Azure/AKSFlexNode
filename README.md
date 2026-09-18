# AKS Flex Node

## Overview

AKS Flex Node extends Azure Kubernetes Service (AKS) to virtual machines and bare metal hosts that you manage. Use Flex nodes when selected workloads need compute outside standard AKS node pools, such as capacity in another region, on-premises hardware, or specialized accelerators.

The Flex Node agent prepares the host, creates an isolated Kubernetes worker environment with systemd-nspawn, joins that worker to AKS, and reconciles its configuration with the state requested by AKS. You retain responsibility for the host operating system, identity, network connectivity, and workload configuration.

> [!IMPORTANT]
> AKS Flex Node is currently in [public preview](https://learn.microsoft.com/en-us/azure/aks/flex-nodes-for-aks-overview) and isn't recommended for production workloads. Review the [AKS support policy](https://learn.microsoft.com/azure/aks/support-policies) and the [Supplemental Terms of Use for Microsoft Azure Previews](https://azure.microsoft.com/support/legal/preview-supplemental-terms/) before using it.

## Key features and scenarios

- Attach customer-managed virtual machines or bare metal hosts to an AKS cluster.
- Use `amd64` or `arm64` Linux hosts running Ubuntu 24.04.
- Authenticate the host to Azure with Azure Arc managed identity, Azure VM managed identity, or a service principal.
- Run the Kubernetes worker components in an isolated nspawn environment.
- Apply node settings and lifecycle operations through an AKS Flex node pool and Machine resources.
- Use customer-managed networking to connect AKS-managed nodes, Flex nodes, pods, and Kubernetes services.
- Evaluate specialized hardware scenarios, including NVIDIA and AMD GPU hosts.

## Getting started

<a id="prerequisites"></a>
<a id="network-requirements"></a>
<a id="prepare-the-cluster"></a>
<a id="step-1-set-your-variables-and-connect-to-the-cluster"></a>
<a id="step-2-generate-the-join-configuration"></a>
<a id="step-3-copy-the-configuration-to-your-flex-node-machine"></a>
<a id="step-4-install-the-agent-on-your-flex-node-machine"></a>
<a id="step-5-run-preflight-checks"></a>
<a id="step-6-join-the-node"></a>
<a id="step-7-verify-the-node-joined"></a>
<a id="clean-up"></a>
<a id="troubleshooting"></a>
<a id="next-steps"></a>

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

1. Review the [documentation index](docs/README.md) and [authentication options](docs/usage/joining-nodes.md).
2. Plan nonoverlapping AKS node, AKS pod, Flex node, Flex pod, and Kubernetes service address ranges with your network administrator.
3. Follow the [operator guide](docs/usage/getting-started.md) to prepare the cluster, install Unbounded-Net, create the Flex node pool, prepare the host identity, bootstrap the host, and verify the result.
4. Use the [operations guide](docs/usage/operations.md) for inspection, upgrade, reset, uninstall, and troubleshooting tasks.

The operator workflow creates Azure and Kubernetes resources and requires more than an agent installation on an existing host. Review the complete prerequisites and cleanup steps before you begin.

> [!IMPORTANT]
> Create the AKS cluster with bring-your-own CNI mode (`--network-plugin none`), then install Unbounded-Net and initialize the AKS and Flex Sites before attaching a host. An AKS-managed CNI on existing node pools doesn't provide pod networking for the external Flex node.

### Attach a prepared Azure VM

After the cluster, Unbounded-Net Sites, Flex node pool, private Layer 3 path, and host managed identity are ready, the host attachment is a short operation. The identity must have **Azure Kubernetes Service Flex Node Agent Role** at the target ARM agent-pool scope. This example uses a system-assigned managed identity; see the [operator guide](docs/usage/getting-started.md#assign-the-host-identitys-pool-scoped-role) for role assignment and the [joining guide](docs/usage/joining-nodes.md) for Azure Arc or service principal authentication.

On the flex node host, download the bootstrap script from the same release as the agent. The agent installs missing host packages during bootstrap; the host only needs the script's direct tools, including Bash, `curl`, `tar`, and `jq`.

```bash
export AKS_RESOURCE_ID="<aks-resource-id>"
export FLEX_POOL_NAME="<flex-node-pool-name>"
export AKS_FLEX_NODE_VERSION="v0.1.11"

curl -fsSLo /tmp/aks-flex-node-bootstrap.sh \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/bootstrap.sh"
chmod 0700 /tmp/aks-flex-node-bootstrap.sh
bash -n /tmp/aks-flex-node-bootstrap.sh
```

Run bootstrap as root. It retrieves fresh pool data, verifies prerequisites, installs the agent, registers the Azure Machine, and starts the nspawn worker:

```bash
sudo bash /tmp/aks-flex-node-bootstrap.sh \
  --auth msi \
  --fetch-bootstrap-data \
  --cluster-resource-id "$AKS_RESOURCE_ID" \
  --agent-pool-name "$FLEX_POOL_NAME" \
  --agent-version "$AKS_FLEX_NODE_VERSION"

rm -f /tmp/aks-flex-node-bootstrap.sh
```

This minimal path downloads the agent from its GitHub release and uses the agent's default online rootfs and component sources. Use the [offline artifacts lab](docs/labs/aks-public-cluster-offline-bootstrap.md) only when you need mirrored or filesystem-backed bootstrap artifacts.

From the Bash environment, verify both the Kubernetes Node and Azure Machine:

```bash
kubectl get nodes -o wide
az aks machine list \
  --resource-group "<aks-resource-group>" \
  --cluster-name "<aks-cluster-name>" \
  --nodepool-name "$FLEX_POOL_NAME" \
  --output table
```

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
