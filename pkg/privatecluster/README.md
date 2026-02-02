# Private AKS Cluster - Edge Node Join/Leave

## Prerequisites

### 1. Login to Azure CLI

```bash
az login
```

> **Note:** When running the agent with `sudo`, use `sudo -E` to preserve your Azure CLI token.

### 2. Create a Private AKS Cluster

Create a Private AKS cluster with AAD and Azure RBAC enabled, and assign the required roles to your user.

See: [create_private_cluster.md](create_private_cluster.md)

### 3. Prepare Configuration File

Create a `config.json` with `"private": true` in the `targetCluster` section:

```json
{
  "azure": {
    "subscriptionId": "<SUBSCRIPTION_ID>",
    "tenantId": "<TENANT_ID>",
    "targetCluster": {
      "resourceId": "/subscriptions/<SUB_ID>/resourceGroups/<RG>/providers/Microsoft.ContainerService/managedClusters/<CLUSTER_NAME>",
      "location": "eastus2",
      "private": true
    },
    "arc": {
      "enabled": true,
      "resourceGroup": "<RG>",
      "location": "eastus2"
    }
  },
  "kubernetes": {
    "version": "1.33.0"
  },
  "containerd": {
    "version": "1.7.11",
    "pauseImage": "mcr.microsoft.com/oss/kubernetes/pause:3.6"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
```

## Join Private AKS Cluster

### 1. Build the project

```bash
go build -o aks-flex-node .
```

### 2. Join the cluster

When the config has `"private": true`, the `agent` command automatically sets up the Gateway/VPN before bootstrapping:

```bash
sudo -E ./aks-flex-node agent --config config.json
```

This will:
1. Detect private cluster from config
2. Set up Gateway VM and VPN tunnel (WireGuard)
3. Run normal bootstrap (Arc, containerd, kubelet, etc.)
4. Enter daemon mode for status monitoring

### 3. Verify

```bash
kubectl get nodes
```

## Leave Private AKS Cluster

When the config has `"private": true`, the `unbootstrap` command automatically handles VPN/Gateway cleanup:

```bash
sudo -E ./aks-flex-node unbootstrap --config config.json [--cleanup-mode <local|full>]
```

### Mode Comparison

| Mode | Command | Description |
|------|---------|-------------|
| `local` (default) | `sudo -E ./aks-flex-node unbootstrap --config config.json` | Remove node and local VPN config, **keep Gateway** for other nodes |
| `full` | `sudo -E ./aks-flex-node unbootstrap --config config.json --cleanup-mode full` | Remove all components **including Gateway VM and Azure resources** |

### When to use each mode

- **`--cleanup-mode=local`** (default): Other nodes are still using the Gateway, or you plan to rejoin later
- **`--cleanup-mode=full`**: Last node leaving, clean up all Azure resources (Gateway VM, subnet, NSG, public IP)
