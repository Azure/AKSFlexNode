# AKS Flex Node Usage Guide

This guide provides two complete deployment paths for AKS Flex Node:

1. **[Deployment with Azure Arc](#deployment-with-azure-arc)** - Easier setup for quick start, plug and play
2. **[Deployment with Service Principal](#deployment-with-service-principal)** - More scalable for production deployments

## Comparison: Arc vs Service Principal

Use this comparison to choose the deployment path that best fits your requirements:

| Feature | With Azure Arc | With Service Principal |
|---------|---------------|----------------------|
| **Setup Complexity** | Simple (plug and play) | Moderate (requires SP setup) |
| **Scalability** | Limited (Arc overhead per node) | High (lightweight, efficient) |
| **Credential Management** | Automatic (managed identity) | Manual (SP rotation) |
| **Azure Visibility** | Full (Arc resource in portal) | Limited (just node) |
| **Authentication** | Managed identity + auto-rotation | Static SP credentials |
| **Required Permissions** | More (Arc + RBAC + AKS) | Less (AKS only) |
| **Performance** | Higher overhead (Arc agent) | Lower overhead (direct auth) |
| **Use Case** | Quick start, demos, small scale | Production, large scale |

---

## Prerequisites and System Requirements

### VM Requirements
- **Operating System:** Ubuntu 22.04 LTS or 24.04 LTS (non-Azure VM)
- **Architecture:** x86_64 (amd64) or arm64
- **Memory:** Minimum 2GB RAM (4GB recommended)
- **Storage:**
  - **Minimum:** 25GB free space
  - **Recommended:** 40GB free space
  - **Production:** 50GB+ free space
- **Network:** Outbound internet connectivity (see Network Requirements below)
- **Privileges:** Root/sudo access required

### Storage Breakdown
- **Base components:** ~3GB (containerd, runc, Kubernetes binaries, CNI plugins, Arc agent if enabled)
- **System directories:** ~5-10GB (`/var/lib/containerd`, `/var/lib/kubelet`, configurations)
- **Container images:** ~5-15GB (pause container, system images, workload images)
- **Logs:** ~2-5GB (`/var/log/pods`, `/var/log/containers`, agent logs)
- **Installation buffer:** ~5-10GB (temporary downloads, garbage collection headroom)

### Network Requirements

The VM requires outbound internet connectivity to:

- **Ubuntu APT Repositories:** Package downloads and updates
- **Binary Downloads:** Kubelet, containerd, runc, CNI plugins
- **Azure Endpoints:**
  - AKS cluster API server (port 443)
  - Azure Resource Manager APIs
  - Azure Arc services (if Arc mode enabled)
- **Container Registries:** Container image pulls (mcr.microsoft.com, etc.)

**Note:** No inbound connectivity is required from the internet. All connections are initiated outbound from the VM.

### Azure Permissions

**For Arc Mode:**
- `Azure Connected Machine Onboarding` role on the resource group
- `User Access Administrator` or `Owner` role on the AKS cluster
- `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster

**For Service Principal Mode:**
- `Azure Kubernetes Service Cluster Admin Role` on the target AKS cluster (for initial setup)
- Service Principal with `Owner` role on the AKS cluster resource

---

## Setup with Azure Arc

Azure Arc provides an easier, plug-and-play setup with managed identity.

### Cluster Setup

Create an AKS cluster with Azure AD and RBAC enabled:

```bash
az aks create \
    --resource-group <resource-group-name> \
    --name <cluster-name> \
    --enable-aad \
    --enable-azure-rbac \
    --aad-admin-group-object-ids <group-id>
```

**Note:** The `group-id` is for a group that will have cluster access. Your `az login` account must be a member of this group.

### Installation

```bash
# Install aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | sudo bash

# Verify installation
aks-flex-node version
```

### Configuration

Create the configuration file with Arc enabled:

```bash
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

**Replace these values:**
- `your-subscription-id`: Azure subscription ID
- `your-tenant-id`: Azure tenant ID
- `your-unique-node-name`: Unique name for this node
- `your-resource-group`: Resource group for Arc machine
- `your-cluster`: AKS cluster name

### Authentication for Arc Registration

You need use Azure CLI credentials for Arc registration:

```bash
# Login to Azure
az login

# The agent will use your CLI credentials
aks-flex-node agent --config /etc/aks-flex-node/config.json
```

### Running the Agent

```bash
# Direct execution
aks-flex-node agent --config /etc/aks-flex-node/config.json

# Or using systemd
sudo systemctl enable --now aks-flex-node-agent
journalctl -u aks-flex-node-agent -f
```

### Verification

After bootstrap completes, verify:

1. **Arc registration:**
   ```bash
   az connectedmachine show \
       --resource-group <resource-group> \
       --name <machine-name>
   ```

2. **Node joined cluster:**
   ```bash
   kubectl get nodes
   ```

The node should appear with "Ready" status.

### How It Works

1. Agent registers VM with Azure Arc → creates managed identity
2. Agent assigns RBAC roles to the managed identity
3. Kubelet uses Arc-managed identity for authentication
4. Tokens are automatically rotated by Azure Arc

---

## Setup with Service Principal

Use this approach for production and scalable deployments. Service Principal mode provides direct authentication without Azure Arc overhead, making it more suitable for managing large fleets of edge nodes.

### Cluster Setup

Create an AKS cluster with Azure AD enabled:

```bash
# Create AKS cluster
MY_USER_ID=$(az ad signed-in-user show --query id -o tsv)
RESOURCE_GROUP="your-resource-group"
CLUSTER_NAME="your-cluster-name"
az aks create \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \

    --enable-aad \
    --aad-admin-group-object-ids "$MY_USER_ID"
```

### Service Principal Setup

Create a Service Principal with appropriate permissions:

```bash
# Get AKS resource ID
AKS_RESOURCE_ID=$(az aks show \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --query "id" \
    --output tsv)

# Create service principal with Owner role on the cluster
SP_JSON=$(az ad sp create-for-rbac \
    --name "aks-flex-node-sp" \
    --role "Owner" \
    --scopes "$AKS_RESOURCE_ID")

SP_OBJECT_ID=$(echo "$SP_JSON" | jq -r '.id')
SP_CLIENT_ID=$(echo "$SP_JSON" | jq -r '.appId')
SP_CLIENT_SECRET=$(echo "$SP_JSON" | jq -r '.password')
TENANT_ID=$(echo "$SP_JSON" | jq -r '.tenant')
```

### Configure RBAC Roles

Apply the necessary Kubernetes RBAC roles for the Service Principal:

```bash
# Get cluster credentials
az aks get-credentials \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --admin \
    --overwrite-existing

# Create node bootstrapper role binding
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-bootstrapper
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node-bootstrapper
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: User
  name: $SP_OBJECT_ID
EOF

# Create node role binding
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-role
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: User
  name: $SP_OBJECT_ID
EOF
```

### Installation

```bash
# Install aks-flex-node
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | sudo bash

# Verify installation
aks-flex-node version
```

### Configuration

Create the configuration file with Service Principal credentials:

```bash
# Get subscription ID
SUBSCRIPTION=$(az account show --query id -o tsv)

# Create config file
sudo tee /etc/aks-flex-node/config.json > /dev/null <<EOF
{
  "azure": {
    "subscriptionId": "$SUBSCRIPTION",
    "tenantId": "$TENANT_ID",
    "cloud": "AzurePublicCloud",
    "servicePrincipal": {
      "clientId": "$SP_CLIENT_ID",
      "clientSecret": "$SP_CLIENT_SECRET"
    },
    "arc": {
      "enabled": false
    },
    "targetCluster": {
      "resourceId": "$AKS_RESOURCE_ID",
      "location": "$LOCATION"
    }
  },
  "kubernetes": {
    "version": "1.30.0"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF
```

### Running the Agent

```bash
# Direct execution
aks-flex-node agent --config /etc/aks-flex-node/config.json

# Or using systemd
sudo systemctl enable --now aks-flex-node-agent
journalctl -u aks-flex-node-agent -f
```

### Verification

After bootstrap completes, verify the node joined the cluster:

```bash
kubectl get nodes

# Check node details
kubectl describe node <node-name>
```

### How It Works

1. Service Principal authenticates directly to Azure AD
2. Agent downloads cluster configuration using SP credentials
3. Kubelet uses Service Principal for ongoing authentication
4. No Arc registration or managed identity

### Security Considerations

- **Credential Rotation:** Service Principal secrets must be manually rotated
- **Secure Storage:** Config file contains sensitive credentials - restrict permissions
- **Scope Minimization:** Use minimum required permissions for the Service Principal

---

## Common Operations

### Available Commands

| Command | Description | Usage |
|---------|-------------|-------|
| `agent` | Start agent daemon (bootstrap + monitoring) | `aks-flex-node agent --config /etc/aks-flex-node/config.json` |
| `unbootstrap` | Clean removal of all components | `aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json` |
| `version` | Show version information | `aks-flex-node version` |

### Monitoring Logs

```bash
# View agent logs (systemd)
journalctl -u aks-flex-node-agent -f

# View agent logs (file)
tail -f /var/log/aks-flex-node/aks-flex-node.log

# View kubelet logs
journalctl -u kubelet -f
```

### Unbootstrap

Remove the node from the cluster and clean up:

```bash
# Run unbootstrap
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json

# Verify node removed from cluster
kubectl get nodes
```

## Uninstallation

### Complete Removal

```bash
# First run unbootstrap
aks-flex-node unbootstrap --config /etc/aks-flex-node/config.json

# Then uninstall the agent
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | sudo bash
```

The uninstall script will:
- Stop and disable aks-flex-node agent service
- Remove the service user and permissions
- Clean up all directories and configuration files
- Remove the binary and systemd service files

### Force Uninstall

```bash
# Non-interactive mode
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | sudo bash -s -- --force
```

## Troubleshooting

### Arc Mode Issues

```bash
# Check Arc agent status
sudo systemctl status himds

# Check Arc connection
azcmagent show

# View Arc agent logs
sudo journalctl -u himds -f
```

### Service Principal Mode Issues

```bash
# Verify SP can authenticate
az login --service-principal \
    --username $SP_CLIENT_ID \
    --password $SP_CLIENT_SECRET \
    --tenant $TENANT_ID

# Check SP permissions on cluster
az aks show \
    --resource-group $RESOURCE_GROUP \
    --name $CLUSTER_NAME
```

### Kubelet Issues

```bash
# Check kubelet status
sudo systemctl status kubelet

# View kubelet logs
sudo journalctl -u kubelet -f

# Check kubelet configuration
sudo cat /var/lib/kubelet/kubeconfig
```
