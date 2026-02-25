# Create Private AKS Cluster

This guide shows how to create a Private AKS Cluster with AAD and Azure RBAC enabled for edge node testing.

## Prerequisites

### 1. Login to Azure CLI

```bash
az login
```

### 2. Set variables

```bash
# Required
CLUSTER_NAME="my-private-aks"
RESOURCE_GROUP="my-rg"
LOCATION="eastus2"

# Optional (defaults)
VNET_NAME="${CLUSTER_NAME}-vnet"
VNET_CIDR="10.224.0.0/12"
SUBNET_NAME="aks-subnet"
SUBNET_CIDR="10.224.0.0/16"
NODE_COUNT=1
NODE_VM_SIZE="Standard_D2s_v3"
```

## Step 1: Create Resource Group

```bash
az group create \
  --name "$RESOURCE_GROUP" \
  --location "$LOCATION"
```

## Step 2: Create VNet and Subnet

```bash
# Create VNet
az network vnet create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$VNET_NAME" \
  --address-prefix "$VNET_CIDR"

# Create Subnet
az network vnet subnet create \
  --resource-group "$RESOURCE_GROUP" \
  --vnet-name "$VNET_NAME" \
  --name "$SUBNET_NAME" \
  --address-prefix "$SUBNET_CIDR"
```

## Step 3: Create Private AKS Cluster

```bash
# Get Subnet ID
SUBNET_ID=$(az network vnet subnet show \
  --resource-group "$RESOURCE_GROUP" \
  --vnet-name "$VNET_NAME" \
  --name "$SUBNET_NAME" \
  --query id -o tsv)

# Create Private AKS Cluster
az aks create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --location "$LOCATION" \
  --node-count "$NODE_COUNT" \
  --node-vm-size "$NODE_VM_SIZE" \
  --network-plugin azure \
  --vnet-subnet-id "$SUBNET_ID" \
  --enable-private-cluster \
  --enable-aad \
  --enable-azure-rbac \
  --generate-ssh-keys
```

> **Note:** This may take 5-10 minutes.

## Step 4: Assign RBAC Roles to Current User

The current user needs two roles to manage the cluster:

| Role | Purpose |
|------|---------|
| Azure Kubernetes Service Cluster Admin Role | Get kubectl credentials |
| Azure Kubernetes Service RBAC Cluster Admin | Perform cluster operations |

```bash
# Get current user's Object ID
USER_OBJECT_ID=$(az ad signed-in-user show --query id -o tsv)

# Get AKS Resource ID
AKS_RESOURCE_ID=$(az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query id -o tsv)

# Assign Role 1: Azure Kubernetes Service Cluster Admin Role
az role assignment create \
  --assignee "$USER_OBJECT_ID" \
  --role "Azure Kubernetes Service Cluster Admin Role" \
  --scope "$AKS_RESOURCE_ID"

# Assign Role 2: Azure Kubernetes Service RBAC Cluster Admin
az role assignment create \
  --assignee "$USER_OBJECT_ID" \
  --role "Azure Kubernetes Service RBAC Cluster Admin" \
  --scope "$AKS_RESOURCE_ID"
```

## Step 5: Get Kubectl Credentials

```bash
# Create kubeconfig directory
sudo mkdir -p /root/.kube

# Get credentials (use sudo -E to preserve Azure CLI token)
sudo -E az aks get-credentials \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --overwrite-existing \
  --file /root/.kube/config

# Convert kubeconfig for Azure CLI auth
sudo -E kubelogin convert-kubeconfig -l azurecli --kubeconfig /root/.kube/config
```

## Step 6: Get Cluster Resource ID

Save this for use in the `config.json` file's `targetCluster.resourceId` field:

```bash
az aks show \
  --resource-group "$RESOURCE_GROUP" \
  --name "$CLUSTER_NAME" \
  --query id -o tsv
```

Example output:
```
/subscriptions/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx/resourcegroups/my-rg/providers/Microsoft.ContainerService/managedClusters/my-private-aks
```

## Next Steps

### Join an edge node to the private cluster

Set `"private": true` in your `config.json`, then run:

```bash
sudo -E ./aks-flex-node agent --config config.json
```

### Leave the private cluster

```bash
# Local cleanup (keep Gateway for other nodes)
sudo -E ./aks-flex-node unbootstrap --config config.json

# Full cleanup (remove Gateway and all Azure resources)
sudo -E ./aks-flex-node unbootstrap --config config.json --cleanup-mode full
```
