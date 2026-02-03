# AKS Flex Node Usage Guide

This guide will walk you through installing and configuring AKS Flex Node to transform your Ubuntu VM into an AKS worker node.

## Prerequisites

### VM Requirements
- Ubuntu 22.04 LTS or 24.04 LTS VM (non-Azure)
- Architecture: x86_64 (amd64) or arm64
- Minimum 2GB RAM, 25GB free disk space
- Sudo access on the VM

### AKS Cluster Requirements
- Azure RBAC enabled AKS cluster
- Network connectivity from edge VM to cluster API server (port 443)

### Azure Authentication & Permissions

**Required for all deployments:**
- **AKS Access:** `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster

**Additional permissions (only if using Azure Arc):**
- **Arc Registration:** `Azure Connected Machine Onboarding` role on the resource group
- **RBAC Assignment:** `User Access Administrator` or `Owner` role on the AKS cluster to assign roles to the Arc managed identity

> **Note:** Azure Arc integration is optional. When Arc is disabled, kubelet authentication uses Service Principal credentials instead of Arc managed identity.

### Cluster Prerequisites

The cluster needs to be created or updated with command line similar to the following:

```bash
az aks create \
    --resource-group <resource group name> \
    --name <cluster name> \
    --enable-aad \
    --enable-azure-rbac \
    --aad-admin-group-object-ids <group ID>
```

**Note:** `group ID` is the ID of a group that will have access to the cluster. Later on you'll use `az login` to log into Azure. The account you use to log in needs to be a member of this group.

## Installation

```bash
# Install aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | sudo bash

# Verify installation
aks-flex-node version
```

## Configuration

### Configuration Options

AKS Flex Node supports two authentication modes:

1. **With Azure Arc** - Provides managed identity and enhanced cloud management
2. **Without Azure Arc** - Uses Service Principal for kubelet authentication

Choose the configuration that matches your requirements below.

### Option 1: Configuration with Azure Arc

```bash
# Create configuration file with Arc enabled
sudo tee /etc/aks-flex-node/config.json > /dev/null << 'EOF'
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "cloud": "AzurePublicCloud",
    "arc": {
      "enabled": true,
      "machineName": "your-unique-node-name",
      "tags": {
        "environment": "edge",
        "node-type": "worker"
      },
      "resourceGroup": "your-resource-group",
      "location": "westus",
      "autoRoleAssignment": true
    },
    "targetCluster": {
      "resourceId": "/subscriptions/your-subscription-id/resourceGroups/your-rg/providers/Microsoft.ContainerService/managedClusters/your-cluster",
      "location": "westus"
    }
  },
  "kubernetes": {
    "version": "your-kubernetes-version"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF
```

### Option 2: Configuration without Azure Arc

When Arc is disabled, you must provide Service Principal credentials for kubelet authentication:

```bash
# Create configuration file without Arc
sudo tee /etc/aks-flex-node/config.json > /dev/null << 'EOF'
{
  "azure": {
    "subscriptionId": "your-subscription-id",
    "tenantId": "your-tenant-id",
    "cloud": "AzurePublicCloud",
    "servicePrincipal": {
      "clientId": "your-service-principal-client-id",
      "clientSecret": "your-service-principal-client-secret"
    },
    "arc": {
      "enabled": false
    },
    "targetCluster": {
      "resourceId": "/subscriptions/your-subscription-id/resourceGroups/your-rg/providers/Microsoft.ContainerService/managedClusters/your-cluster",
      "location": "westus"
    }
  },
  "kubernetes": {
    "version": "your-kubernetes-version"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF
```

**Important:** Replace the placeholder values with your actual Azure resource information:
- `your-subscription-id`: Your Azure subscription ID
- `your-tenant-id`: Your Azure tenant ID
- `your-unique-node-name`: A unique name for this edge node (Arc mode only)
- `your-resource-group`: Resource group where Arc machine and AKS cluster are located (Arc mode only)
- `your-cluster`: Your AKS cluster name
- `your-service-principal-client-id`: Service Principal client ID (non-Arc mode only)
- `your-service-principal-client-secret`: Service Principal client secret (non-Arc mode only)

## Usage

### Available Commands

| Command | Description | Usage |
|---------|-------------|-------|
| `agent` | Start agent daemon (bootstrap + monitoring) | `aks-flex-node agent --config /etc/aks-flex-node/config.json` |
| `unbootstrap` | Clean removal of all components | `aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json` |
| `version` | Show version information | `aks-flex-node version` |

### Running the Agent

```bash
# Option 1: Direct command execution
aks-flex-node agent --config /etc/aks-flex-node/config.json
cat /var/log/aks-flex-node/aks-flex-node.log

# Option 2: Using systemd service
sudo systemctl enable --now aks-flex-node-agent
journalctl -u aks-flex-node-agent --since "1 minutes ago" -f
```

After you've set the correct config and started the agent, it takes a while to finish all the steps. If you used systemd service, as mentioned above, you can use:

```bash
journalctl -u aks-flex-node-agent --since "1 minutes ago" -f
```

to view logs and see if anything goes wrong.

### Verifying Success

If everything works fine, after a while, you should see:

- **With Arc enabled:** In the resource group you specified in the config file, you should see a new resource added by Azure Arc with type `Microsoft.HybridCompute/machines`
- **All modes:** Running `kubectl get nodes` against your cluster should see the new node added and in "Ready" state

### Unbootstrap

```bash
# Direct command execution
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json
cat /var/log/aks-flex-node/aks-flex-node.log
```

## Authentication Methods

AKS Flex Node supports multiple authentication methods depending on your configuration:

### Bootstrap Authentication (Arc Registration)

**Only applies when Arc is enabled (`arc.enabled: true`)**

For Arc registration and role assignment operations, you can use:

#### Option 1: CLI Credential

When Service Principal isn't configured, the service will use `az login` credential for Arc-related operations (joining the VM to Azure as an ARC machine). If you haven't run `az login` or your token is expired, the bootstrap process will automatically prompt you to login interactively.

- The login prompt will appear in your terminal with device code authentication when needed
- Once authenticated, the service will use your Azure CLI credentials for Arc join and role assignments

#### Option 2: Service Principal (Bootstrap)

Configure a service principal for Arc operations:

```json
{
  "azure": {
    "servicePrincipal": {
      "clientId": "your-service-principal-client-id",
      "clientSecret": "your-service-principal-client-secret"
    },
    // ... rest of config
  }
}
```

**Required permissions for Arc mode:**
- `Azure Connected Machine Onboarding` role on the resource group
- `User Access Administrator` or `Owner` role on the AKS cluster
- `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster

### Runtime Authentication (Kubelet)

How kubelet authenticates to the AKS cluster depends on your Arc configuration:

#### With Arc Enabled
- Kubelet uses Arc-managed identity for authentication
- Tokens are automatically managed and rotated by Azure Arc
- No manual credential management required

#### Without Arc (Service Principal Required)
- Kubelet uses Service Principal credentials for authentication
- Service Principal must be configured in the config file
- Service Principal needs `Azure Kubernetes Service Cluster User Role` on the AKS cluster

## Uninstallation

### Complete Removal

```bash
# First run unbootstrap to cleanly disconnect from Arc and AKS cluster
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json

# Then run automated uninstall to remove all components
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | sudo bash
```

The uninstall script will:
- Stop and disable aks-flex-node agent service
- Remove the service user and permissions
- Clean up all directories and configuration files
- Remove the binary and systemd service files

### Force Uninstall (Non-interactive)

```bash
# For automated environments where confirmation prompts should be skipped
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | sudo bash -s -- --force
```

**⚠️ Important Notes:**
- Run `aks-flex-node unbootstrap` first to properly disconnect from Arc and clean up Azure resources
- The uninstall script will NOT disconnect from Arc - this ensures proper cleanup order
- The Azure Arc agent remains installed but can be removed manually if not needed
- Backup any important data before uninstalling

## System Requirements

- **Operating System:** Ubuntu 22.04 LTS or 24.04 LTS
- **Architecture:** x86_64 (amd64) or arm64
- **Memory:** Minimum 2GB RAM (4GB recommended)
- **Storage:**
  - **Minimum:** 25GB free space
  - **Recommended:** 40GB free space
  - **Production:** 50GB+ free space
- **Network:** Internet connectivity to Azure endpoints
- **Privileges:** Root/sudo access required
- **Build Dependencies:** Go 1.24+ (if building from source)

### Storage Breakdown

- **Base components:** ~3GB (Arc agent, runc, containerd, Kubernetes binaries, CNI plugins)
- **System directories:** ~5-10GB (`/var/lib/containerd`, `/var/lib/kubelet`, configurations)
- **Container images:** ~5-15GB (pause container, system images, workload images)
- **Logs:** ~2-5GB (`/var/log/pods`, `/var/log/containers`, agent logs)
- **Installation buffer:** ~5-10GB (temporary downloads, garbage collection headroom)
