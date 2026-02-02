# Private AKS Cluster - Edge Node Join/Leave

## Prerequisites

### 1. Login to Azure CLI as root

```bash
sudo az login
```

### 2. Create a Private AKS Cluster

Create a Private AKS cluster with AAD and Azure RBAC enabled, and assign the required roles to your user.

See: [create_private_cluster.md](create_private_cluster.md)

## Join Private AKS Cluster

### 1. Build the project

```bash
go build -o aks-flex-node .
```

### 2. Join the cluster

```bash
sudo ./aks-flex-node private-join --aks-resource-id "<AKS_RESOURCE_ID>"
```

Example:
```bash
sudo ./aks-flex-node private-join \
  --aks-resource-id "/subscriptions/xxx/resourcegroups/my-rg/providers/Microsoft.ContainerService/managedClusters/my-private-aks"
```

### 3. Verify

```bash
sudo kubectl get nodes
```

## Leave Private AKS Cluster

```bash
sudo ./aks-flex-node private-leave --mode=<local|full> [--aks-resource-id "<AKS_RESOURCE_ID>"]
```

### Mode Comparison

| Mode | Command | Description |
|------|---------|-------------|
| `local` | `sudo ./aks-flex-node private-leave --mode=local` | Remove node and local components, **keep Gateway** for other nodes |
| `full` | `sudo ./aks-flex-node private-leave --mode=full --aks-resource-id "..."` | Remove all components **including Gateway and Azure resources** |

### When to use each mode

- **`--mode=local`**: Other nodes are still using the Gateway, or you plan to rejoin later
- **`--mode=full`**: Last node leaving, clean up all Azure resources (Gateway VM, subnet, NSG, public IP)
