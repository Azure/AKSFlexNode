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

Before you begin, [create or choose an existing AKS cluster](https://learn.microsoft.com/azure/aks/learn/quick-kubernetes-deploy-cli) and a virtual machine or bare metal host to join as a Flex Node. This example assumes a Linux workstation with Azure CLI, `kubectl`, `openssl`, `curl`, `envsubst`, and `python3`. The target host must run systemd, allow root installation, and reach the AKS API server over outbound HTTPS.

The flow below will:

1. Create a short-lived Kubernetes bootstrap token and RBAC bindings on the AKS cluster.
2. Collect the AKS cluster connection values needed by the Flex Node config.
3. Install `aks-flex-node` on the target host as root.
4. Render `/etc/aks-flex-node/config.json` from the bootstrap-token example.
5. Start the host bootstrap flow and launch the `aks-flex-node-agent` systemd service.

Expected result: the target host appears in `kubectl get nodes`, and `aks-flex-node-agent` is running on the host.

On your workstation, get AKS admin credentials and create a short-lived Kubernetes bootstrap token:

```bash
RESOURCE_GROUP="<resource-group>"
CLUSTER_NAME="<cluster-name>"

az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --admin \
  --overwrite-existing

TOKEN_ID="$(openssl rand -hex 3 | tr '[:upper:]' '[:lower:]')"
TOKEN_SECRET="$(openssl rand -hex 8 | tr '[:upper:]' '[:lower:]')"
BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"
EXPIRATION="$(python3 - <<'PY'
from datetime import datetime, timedelta, timezone
print((datetime.now(timezone.utc) + timedelta(hours=24)).strftime('%Y-%m-%dT%H:%M:%SZ'))
PY
)"

curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/docs/examples/bootstrap-token-rbac.yaml \
  | envsubst \
  | kubectl apply -f -
```

Collect the values needed by the host config:

```bash
SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
TENANT_ID="$(az account show --query tenantId -o tsv)"
AKS_RESOURCE_ID="$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query id -o tsv)"
LOCATION="$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query location -o tsv)"
KUBERNETES_VERSION="$(az aks show --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" --query kubernetesVersion -o tsv)"
SERVER_URL="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
CA_CERT_DATA="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
```

On the target host, copy the collected values into the root shell, then install the agent, write the config, and bootstrap the node:

```bash
sudo su
# Optional: set AKS_FLEX_NODE_VERSION=<release-tag> to install a specific release.
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash
aks-flex-node version

umask 077
mkdir -p /etc/aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/docs/examples/bootstrap-token-config.json \
  | envsubst \
  > /etc/aks-flex-node/config.json

cat /etc/aks-flex-node/config.json
```

<details>
<summary>Example Config With Field Notes</summary>

The rendered config should look like this. Comments are shown here only to explain the fields; do not add comments to `/etc/aks-flex-node/config.json`.

```jsonc
{
  "azure": {
    "subscriptionId": "<subscription-id>", // Azure subscription that owns the AKS cluster.
    "tenantId": "<tenant-id>", // Microsoft Entra tenant for the subscription.
    "cloud": "AzurePublicCloud", // Azure cloud environment.
    "bootstrapToken": {
      "token": "<token-id>.<token-secret>" // Short-lived Kubernetes bootstrap token created above.
    },
    "arc": { "enabled": false }, // Arc is disabled for this bootstrap-token flow.
    "targetCluster": {
      "resourceId": "<aks-resource-id>", // Full ARM resource ID of the AKS cluster.
      "location": "<aks-location>" // Azure region of the AKS cluster.
    }
  },
  "node": {
    "kubelet": {
      "serverURL": "https://<aks-api-server>", // AKS API server endpoint.
      "caCertData": "<base64-ca-data>" // Cluster CA bundle from kubeconfig.
    }
  },
  "agent": {
    "logLevel": "info", // Agent log verbosity.
    "logDir": "/var/log/aks-flex-node" // Host log directory.
  },
  "kubernetes": { "version": "<aks-kubernetes-version>" } // Kubelet version to install.
}
```

</details>

```bash
aks-flex-node start --config /etc/aks-flex-node/config.json
```

Verify the node from your workstation:

```bash
kubectl get nodes -o wide
```

Example output:

```text
NAME                   STATUS   ROLES    AGE   VERSION   INTERNAL-IP   EXTERNAL-IP   OS-IMAGE             KERNEL-VERSION      CONTAINER-RUNTIME
aks-flex-readme-test   Ready    <none>   9s    v1.34.3   10.0.0.4      <none>        Ubuntu 24.04.4 LTS   6.17.0-1013-azure   containerd://2.0.4
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
aks-flex-node[3800]: level=INFO msg="running agent daemon" nodeName=aks-flex-readme-test
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
