# AKS Flex Node

## Overview

AKS Flex Node extends Azure Kubernetes Service (AKS) to customer-managed virtual machines and bare metal hosts, enabling them to run as AKS worker nodes outside standard AKS node pools. It is built on top of [Azure Unbounded](https://github.com/Azure/unbounded), which provides the host-side foundation for running and reconciling isolated Kubernetes node environments.

> **Status:** AKS Flex Node is currently alpha software.

## Key Features And Scenarios

- Bootstrap and join virtual machines or bare metal hosts for both amd64 and arm64 as AKS worker nodes.
- Support hybrid, lab, and specialized hardware scenarios.
- Use flexible authentication modes, including Azure Arc, managed identity (MSI), and Kubernetes bootstrap token.
- Automatically detect NVIDIA GPU devices and configure the container runtime for accelerated workloads.
- Run blue-green in-place updates and upgrades while retaining the existing host.
- Manage your Flex Node fleet through AKS management APIs for upgrade, repair, reset, and related lifecycle operations.
- Remediate and repair agent and node state through first-class lifecycle operations.

## Getting Started

This quickstart walks you through joining your first **Flex Node machine**, an on-premises physical server or virtual machine you own, to an existing AKS cluster, then running a workload on it. Plan about 10 minutes once the prerequisites are in place.

### Prerequisites

This quickstart involves two machines. Keep them distinct:

- **Your workstation** is the computer you drive the setup from, using the Azure CLI and `kubectl` (for example, your laptop). It is not joined to the cluster; it only generates the configuration and runs commands. Every step labeled **Run on: your workstation** happens here.
- **Your Flex Node machine** is the on-premises physical server or virtual machine you are joining to the cluster as a worker node. Every step labeled **Run on: your Flex Node machine** happens there.

> **Tip**
> Steps 4 to 6 use a single interactive SSH session: you connect once in Step 4, stay on the Flex Node machine through Step 6, then return to your workstation for Step 7. As an alternative, you can skip the interactive session and run each host command from your workstation by wrapping it, for example `ssh "$TARGET_HOST" "sudo aks-flex-node preflight --config /etc/aks-flex-node/config.json"`. Use one approach or the other, not both. Wrapping requires passwordless sudo on the Flex Node machine.

Before you begin, make sure you have:

- An Azure subscription. [Create one for free](https://azure.microsoft.com/free/).
- **Your workstation**, with the [Azure CLI](https://learn.microsoft.com/cli/azure/install-azure-cli), `kubectl`, `curl`, and `python3` installed. Sign in with `az login`, then `az account set --subscription "<subscription-id>"`.
- **An existing AKS cluster** with a Linux node pool. Azure CNI is recommended so pods are reachable across your network, and you need permission to configure RBAC on the cluster. If you don't have a cluster yet, see [create an AKS cluster](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-cli).
- **A Flex Node machine you own**: an on-premises physical server or a virtual machine to join as the worker node. It must:
  - run `systemd` and allow root installation,
  - have at least 4 vCPUs and enough memory for nspawn startup and the Kubernetes components,
  - be reachable from your workstation over SSH.

### Network requirements

Your Flex Node machine and the AKS cluster must be able to reach each other. Confirm each item before you start:

- **Outbound HTTPS (TCP 443)** from the Flex Node machine to the AKS API server.
- **Bidirectional reachability** between the Flex Node machine's private IP and the AKS node private IPs.
- **Non-overlapping CIDRs** between the host network and the cluster's node and pod address ranges.
- **NSG and firewall rules** that allow the CNI's cross-node traffic (typically TCP/UDP between node private IPs, and pod CIDR ranges depending on the CNI).
- **For private AKS clusters:** the Flex Node machine can resolve and reach the private API server endpoint.

> **Note**
> On-premises machines usually reach the cluster's private node network through a site-to-site VPN, ExpressRoute, or equivalent routed connectivity. Establishing that link is a prerequisite for this quickstart. For advanced network scenarios such as cross-region, gateway, or custom CNI topologies, follow the [lab guides](docs/labs/README.md).

### Step 1: Set your variables and connect to the cluster

**Run on: your workstation**

```bash
export SUBSCRIPTION_ID="<subscription-id>"
export RESOURCE_GROUP="<resource-group-of-your-aks-cluster>"
export CLUSTER_NAME="<cluster-name>"
export AGENT_POOL_NAME="${AGENT_POOL_NAME:-aksflexnodes}"

# Your Flex Node machine, as an SSH target (for example: azureuser@203.0.113.10)
export TARGET_HOST="<user>@<flex-node-machine-ip-or-hostname>"

az aks get-credentials --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME"
kubectl get nodes
```

You should see your existing cluster nodes in `Ready` state.

### Step 2: Generate the join configuration

**Run on: your workstation**

Download the helper, apply the node bootstrap RBAC bindings, and generate a bootstrap-token config from your cluster's metadata.

```bash
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

This produces `aks-flex-node-config.json` in your current folder. `generate-node-config` supports one of the following auth modes: `--bootstrap-token`, `--identity`, `--service-principal --username <client-id> --password <client-secret>`, or `--arc`. This quickstart uses `--bootstrap-token`.

<details>
<summary>Example Config With Field Notes</summary>

The rendered config should look like this. Comments are shown here only to explain the fields; do not add comments to `/etc/aks-flex-node/config.json`.

```jsonc
{
  "azure": {
    "subscriptionId": "<subscription-id>", // Azure subscription that owns the AKS cluster.
    "tenantId": "<tenant-id>", // Microsoft Entra tenant for the subscription.
    "resourceManagerEndpoint": "https://management.azure.com", // Azure Resource Manager endpoint for ARM calls.
    "targetAgentPoolName": "<agent-pool-name>", // AKS agent pool used for FlexNode machine registration.
    "bootstrapToken": {
      "token": "<token-id>.<token-secret>" // Kubernetes bootstrap token created by generate-node-config.
    },
    "arc": { "enabled": false }, // Arc is disabled for this bootstrap-token flow.
    "targetCluster": {
      "resourceId": "<aks-resource-id>", // Full ARM resource ID of the AKS cluster.
      "location": "<aks-location>" // Azure region of the AKS cluster.
    }
  },
  "node": {
    "kubelet": {
      "clusterFQDN": "<aks-api-server-fqdn>", // AKS API server FQDN.
      "caCertData": "<base64-ca-data>" // Cluster CA bundle from kubeconfig.
    }
  },
  "networking": {
    "dnsServiceIP": "<cluster-dns-service-ip>" // Cluster DNS service IP from the AKS network profile.
  },
  "agent": {
    "logLevel": "info", // Agent log verbosity.
    "logDir": "/var/log/aks-flex-node" // Host log directory.
  },
  "components": { "kubernetes": "<aks-kubernetes-version>" } // Kubelet version to install.
}
```

</details>

### Step 3: Copy the configuration to your Flex Node machine

**Run on: your workstation**

```bash
scp ./aks-flex-node-config.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

> **Note**
> The config is staged in `/tmp` because your SSH user cannot write to root-owned `/etc`, and `/etc/aks-flex-node` does not exist yet. The next step creates that directory and installs the file as root with owner-only `0600` permissions.

### Step 4: Install the agent on your Flex Node machine

First, from your workstation, open an SSH session on the Flex Node machine.

**Run on: your workstation**

```bash
ssh "$TARGET_HOST"
```

You are now connected to the Flex Node machine. Install the agent and move the generated config into place, as root.

**Run on: your Flex Node machine**

```bash
sudo su
# Optional: set AKS_FLEX_NODE_VERSION=<release-tag> to install a specific release.
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash
aks-flex-node version

install -d -m 0755 /etc/aks-flex-node
install -m 0600 /tmp/aks-flex-node-config.json /etc/aks-flex-node/config.json

cat /etc/aks-flex-node/config.json
```

### Step 5: Run preflight checks

**Run on: your Flex Node machine**

Preflight is non-mutating. It validates host prerequisites, API server reachability, rootfs image reachability, and bootstrap artifact sources before bootstrap changes the host.

```bash
aks-flex-node preflight --config /etc/aks-flex-node/config.json
```

Confirm all checks pass before continuing.

### Step 6: Join the node

**Run on: your Flex Node machine**

This installs the long-running agent service and starts the local Kubernetes worker environment.

```bash
umask 022
aks-flex-node start --config /etc/aks-flex-node/config.json
```

> **Note**
> `umask 022` sets standard default permissions (directories `755`, files `644`). The bootstrap creates the node's nspawn rootfs directories, and `755` lets non-root service users such as `dbus` traverse them; without it, the node fails to start. Your credentials are unaffected: the config file remains `0600`.

Confirm the agent is running:

```bash
systemctl is-active aks-flex-node-agent
journalctl -u aks-flex-node-agent -f
```

Example output:

```text
active
```

Example logs:

```text
Started aks-flex-node-agent.service - AKS Flex Node Agent.
aks-flex-node[3800]: level=INFO msg="running agent daemon" nodeName=aks-flex-config-test
aks-flex-node[3800]: level=INFO msg="machine state reconciled" status=healthy
```

When you are finished, press `Ctrl+C` to stop following the logs, then run `exit` twice, once to leave the root shell and once to close the SSH session, to return to your workstation.

### Step 7: Verify the node joined

**Run on: your workstation**

```bash
kubectl get nodes -o wide
```

Example output:

```text
NAME                   STATUS   ROLES    AGE   VERSION   INTERNAL-IP   EXTERNAL-IP   OS-IMAGE             KERNEL-VERSION      CONTAINER-RUNTIME
aks-flex-config-test   Ready    <none>   12s   v1.34.3   10.0.0.4      <none>        Ubuntu 24.04.4 LTS   6.17.0-1013-azure   containerd://2.0.4
```

The node name matches the Flex Node machine's hostname unless you set `agent.nodeName` in the config. Schedule a test workload onto it to confirm it can run pods (replace `<node-name>` with the name shown above):

```bash
kubectl run hello --image=mcr.microsoft.com/azuredocs/aks-helloworld:v1 \
  --overrides='{"spec":{"nodeName":"<node-name>"}}'
kubectl get pod hello -o wide
```

<details>
<summary>Inspect the nspawn worker (optional)</summary>

AKS Flex Node runs the Kubernetes worker inside a local nspawn machine. You can inspect it from the Flex Node machine:

```bash
machinectl list
machinectl status kube1
journalctl -M kube1 -u kubelet -f
journalctl -M kube1 -u containerd -f
```

Watch the kubelet and containerd logs to see how the nspawn-backed worker handles scheduled workloads.

</details>

### Clean up

Remove the test workload from your workstation:

```bash
kubectl delete pod hello --ignore-not-found
```

<details>
<summary>Reset And Uninstall</summary>

To remove AKS Flex Node from the host, run the uninstall script as root:

```bash
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | bash -s -- --force
```

Example summary:

```text
SUCCESS: Reset completed
SUCCESS: Removed directory: /var/lib/aks-flex-node
SUCCESS: Removed binary: /usr/local/bin/aks-flex-node
SUCCESS: Azure CLI removed successfully
SUCCESS: AKS Flex Node uninstallation completed!
```

Example reset details:

```text
level=INFO msg="systemd service uninstalled" unit=aks-flex-node-agent.service
level=INFO msg="removing machine rootfs" machine=kube1 dir=/var/lib/machines/kube1
level=INFO msg="removed runtime directory" path=/etc/aks-flex-node
level=INFO msg="removed runtime directory" path=/var/log/aks-flex-node
```

After uninstall, the host should no longer have the agent service or nspawn machines:

```bash
systemctl is-active aks-flex-node-agent
machinectl list
```

Example output:

```text
inactive
No machines.
```

Finally, remove the Kubernetes `Node` object from your workstation:

```bash
kubectl delete node <node-name>
```

</details>

### Next steps

- **Other authentication modes** (managed identity, service principal, Azure Arc): see the [Usage Guide](docs/usage.md).
- **Advanced networking** (cross-region, gateway, custom CNI): see the [lab guides](docs/labs/README.md).
- **GPU workloads:** Flex Node auto-detects NVIDIA GPU devices; see the [Usage Guide](docs/usage.md).

## Usage Guides And Topics

- [Usage Guide](docs/usage.md) - Installation, configuration, authentication modes, operations, and troubleshooting.
- [Labs](docs/labs/README.md) - Hands-on Azure scenarios for trying AKS Flex Node end to end.
- [Design Documentation](docs/design.md) - Architecture, lifecycle, Azure integration, and security model.

## Development And Security

- [Development Guide](docs/development.md) - To learn more about build, development, and contribution workflow.
- [Security Policy](SECURITY.md) - How to report security vulnerabilities.

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE.MD) for details.

---

<div align="center">

**🚀 Built with ❤️ for the Kubernetes community**

![Made with Go](https://img.shields.io/badge/Made%20with-Go-00ADD8?style=flat-square&logo=go)
![Kubernetes](https://img.shields.io/badge/Kubernetes-Ready-326CE5?style=flat-square&logo=kubernetes)
![Azure](https://img.shields.io/badge/Azure-Integrated-0078D4?style=flat-square&logo=microsoftazure)

</div>
