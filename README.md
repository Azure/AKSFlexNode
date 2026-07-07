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

Before you begin, [create or choose an existing AKS cluster](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-cli) and a virtual machine or bare metal host to join as a Flex Node. This example assumes a Linux workstation with Azure CLI, `kubectl`, `curl`, and `python3`. The target host must run systemd, allow root installation, and reach the AKS API server over outbound HTTPS. Use a VM size with enough CPU and memory for nspawn startup and Kubernetes components; the validated quickstart used a 4-vCPU Azure VM.

For the quickstart network, place the target host in a peered or otherwise routed network with non-overlapping CIDRs. The Flex host and AKS node private IPs must have bidirectional reachability, and any NSGs or firewalls must allow the CNI's cross-node traffic (often TCP/UDP between node private IPs, and sometimes pod CIDR ranges depending on the CNI) so pod networking works after the node joins. For private AKS clusters, also ensure the host can resolve and reach the private API endpoint. For advanced network scenarios such as cross-region, gateway, or custom CNI topologies, follow the [lab guides](docs/labs/README.md).

The flow below will:

1. Apply the node bootstrap RBAC bindings on the AKS cluster.
2. Create a Kubernetes bootstrap token while generating the Flex Node config from AKS cluster metadata.
3. Install `aks-flex-node` on the target host as root.
4. Copy the generated config to `/etc/aks-flex-node/config.json`.
5. Start the host bootstrap flow and launch the `aks-flex-node-agent` systemd service.

Expected result: the target host appears in `kubectl get nodes`, and `aks-flex-node-agent` is running on the host.

On your workstation, save the config helper script, setup node RBAC permissions, then generate the bootstrap-token config from AKS cluster metadata:

```bash
RESOURCE_GROUP="<resource-group>"
CLUSTER_NAME="<cluster-name>"
SUBSCRIPTION_ID="<subscription-id>"
AGENT_POOL_NAME="${AGENT_POOL_NAME:-aksflexnodes}"

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

`generate-node-config` supports one of the following auth modes: `--bootstrap-token`, `--identity`, `--service-principal --username <client-id> --password <client-secret>`, or `--arc`.

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

Copy the generated config to the target host:

```bash
TARGET_HOST="<user>@<host>"

scp ./aks-flex-node-config.json "$TARGET_HOST:/tmp/aks-flex-node-config.json"
```

On the target host, install the agent and move the generated config into place:

```bash
sudo su
# Optional: set AKS_FLEX_NODE_VERSION=<release-tag> to install a specific release.
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash
aks-flex-node version

install -d -m 0755 /etc/aks-flex-node
install -m 0600 /tmp/aks-flex-node-config.json /etc/aks-flex-node/config.json

cat /etc/aks-flex-node/config.json
```

After reviewing the config, run preflight checks. Preflight is non-mutating and validates host prerequisites, API server reachability, rootfs image reachability, and bootstrap artifact sources before bootstrap changes the host.

```bash
aks-flex-node preflight --config /etc/aks-flex-node/config.json
```

Then bootstrap the node. This installs the long-running agent service and starts the local Kubernetes worker environment. Use a standard `022` umask so bootstrap-created nspawn rootfs paths remain traversable by non-root service users such as `dbus`; the config file remains `0600`.

```bash
umask 022
aks-flex-node start --config /etc/aks-flex-node/config.json
```

Verify the node from your workstation:

```bash
kubectl get nodes -o wide
```

Example output:

```text
NAME                   STATUS   ROLES    AGE   VERSION   INTERNAL-IP   EXTERNAL-IP   OS-IMAGE             KERNEL-VERSION      CONTAINER-RUNTIME
aks-flex-config-test   Ready    <none>   12s   v1.34.3   10.0.0.4      <none>        Ubuntu 24.04.4 LTS   6.17.0-1013-azure   containerd://2.0.4
```

The node name should match the target host's hostname unless you set `agent.nodeName` in the config.

On the target host, the agent service should be active:

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

AKS Flex Node runs the Kubernetes worker inside a local nspawn machine. You can inspect it from the host:

```bash
machinectl list
machinectl status kube1
journalctl -M kube1 -u kubelet -f
journalctl -M kube1 -u containerd -f
```

Try scheduling a test workload onto the node and watch kubelet/containerd logs to see how the nspawn-backed worker handles it.

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
