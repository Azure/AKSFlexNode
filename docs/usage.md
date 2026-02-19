# AKS Flex Node Usage Guide

This guide provides three complete setup paths for AKS Flex Node:

1. **[Setup with Azure Arc](#setup-with-azure-arc)** - Easier setup for quick start, plug and play
2. **[Setup with Service Principal](#setup-with-service-principal)** - More scalable for secure production environment
3. **[Setup with Bootstrap Token](#setup-with-bootstrap-token)** - Simplest setup with minimum dependancy for dynamic hyperscale environments

## Comparison: Arc vs Service Principal vs Bootstrap Token

Use this comparison to choose the deployment path that best fits your requirements:

| Feature | With Azure Arc | With Service Principal | With Bootstrap Token |
|---------|---------------|----------------------|---------------------|
| **Setup Complexity** | Simple (plug and play) | Moderate (requires SP setup) | Very simple (just token) |
| **Scalability** | Low (Arc overhead per node) | High (lightweight, efficient) | Highest (minimal overhead) |
| **Credential Management** | Automatic (managed identity) | Manual (SP rotation) | Manual (token rotation) |
| **Azure Visibility** | Full (Arc resource in portal) | Limited (just node) | Limited (just node) |
| **Authentication** | Managed identity + auto-rotation | Static SP credentials | Bootstrap token (time-limited) |
| **Required Permissions** | More (Arc + RBAC + AKS) | Less (AKS only) | Minimal (token creation) |
| **Performance** | Higher overhead (Arc agent) | Lower overhead (direct auth) | Minimum overhead |
| **Use Case** | Quick start, demos, small scale | Production, large scale | Dynamic, hyperscale |

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
    "logDir": "/var/log/aks-flex-node",
    "enableDriftDetectionAndRemediation": true
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
    "logDir": "/var/log/aks-flex-node",
    "enableDriftDetectionAndRemediation": true
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

## Setup with Bootstrap Token

Use this approach for temporary setup, hyperscale environments. Bootstrap tokens are native Kubernetes authentication tokens with configurable time-to-live (TTL), making them ideal for short-lived nodes.

**Why Bootstrap Tokens?**
- **Simplest setup** - No Azure Arc registration needed
- **Native Kubernetes** - Uses standard K8s authentication (TLS bootstrapping)
- **Time-limited** - Tokens expire automatically after configured TTL
- **Quick provisioning** - Generate tokens with minimal dependencies

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

### Bootstrap Token Creation

Create a bootstrap token by creating a Kubernetes secret:

```bash
# Get cluster credentials
az aks get-credentials \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --admin \
    --overwrite-existing

# Generate a valid bootstrap token (format: 6 chars . 16 chars)
TOKEN_ID=$(openssl rand -hex 3)
TOKEN_SECRET=$(openssl rand -hex 8)
BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"

# Set expiration (e.g., 24 hours from now)
EXPIRATION=$(date -u -d "+24 hours" +"%Y-%m-%dT%H:%M:%SZ")

# Create the bootstrap token secret
kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${TOKEN_ID}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  description: "AKS Flex Node bootstrap token"
  token-id: "${TOKEN_ID}"
  token-secret: "${TOKEN_SECRET}"
  expiration: "${EXPIRATION}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:aks-flex-node"
EOF
```

**Important Notes:**
- Token format must be exactly 6 + 16 lowercase alphanumeric characters (total: 23 chars with dot)
- The secret must be in the `kube-system` namespace
- The secret type must be `bootstrap.kubernetes.io/token`
- Secret name must be `bootstrap-token-<token-id>`
- Bootstrap tokens follow the [Kubernetes bootstrap token specification](https://kubernetes.io/docs/reference/access-authn-authz/bootstrap-tokens/)

### Configure RBAC Roles

Apply the necessary Kubernetes RBAC roles for bootstrap token authentication:

```bash
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
  kind: Group
  name: system:bootstrappers:aks-flex-node
EOF

# Create node role binding for kubelet certificate creation
kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-auto-approve-csr
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
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

Create the configuration file with Bootstrap Token:

```bash
# Get subscription ID and cluster resource ID
TENANT_ID=$(az account show --query tenantId -o tsv)
SUBSCRIPTION=$(az account show --query id -o tsv)
AKS_RESOURCE_ID=$(az aks show \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --query "id" \
    --output tsv)
LOCATION=$(az aks show \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --query "location" \
    --output tsv)

SERVER_URL=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA_CERT_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
```

# Create config file (with bootstrap token)

```
sudo tee /etc/aks-flex-node/config.json > /dev/null <<EOF
{
  "azure": {
    "subscriptionId": "$SUBSCRIPTION",
    "tenantId": "$TENANT_ID",
    "cloud": "AzurePublicCloud",
    "bootstrapToken": {
      "token": "$BOOTSTRAP_TOKEN",
      "serverURL": "$SERVER_URL",
      "caCertData": "$CA_CERT_DATA"
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
    "logDir": "/var/log/aks-flex-node",
    "enableDriftDetectionAndRemediation": true
  }
}
EOF

You can disable automated drift detection and remediation if you prefer to self-managed node updates:

```json
{
  "agent": {
    "enableDriftDetectionAndRemediation": false
  }
}
```
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

Bootstrap token authentication follows the [Kubernetes TLS Bootstrapping](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/) process:

1. **Token Creation**: A bootstrap token secret is created in the `kube-system` namespace with proper RBAC bindings
2. **Initial Authentication**: The node uses the bootstrap token to authenticate to the Kubernetes API server
3. **Certificate Request**: Kubelet generates a private key and submits a Certificate Signing Request (CSR) to the cluster
4. **Certificate Approval**: The CSR is automatically approved through the configured RBAC roles
5. **Certificate Issuance**: The cluster issues a signed client certificate for the kubelet
6. **Ongoing Authentication**: After bootstrap, kubelet uses its client certificate for all future API requests
7. **Token Expiration**: The bootstrap token expires after the configured TTL, but the node continues to use its certificate

This approach is more secure than long-lived credentials because:
- Bootstrap tokens are short-lived (typically 24 hours to 7 days)
- After successful bootstrap, the token is no longer needed
- Ongoing authentication uses auto-rotating kubelet certificates (rotated by kubelet automatically)
- Each node gets its own unique client certificate
- No manual credential rotation required after initial bootstrap

### Token vs Certificate Lifecycle

#### Bootstrap Token (One-Time Use)
- **Purpose**: Initial node authentication only
- **Usage**: Used once during node bootstrap
- **Expiration**: Token expires after configured TTL
- **Rotation**: Not needed - token is no longer used after successful bootstrap

#### Kubelet Certificates (Auto-Rotated)
- **Purpose**: Ongoing node authentication after bootstrap
- **Usage**: Used for all API server communication
- **Expiration**: Typically valid for ~1 year
- **Rotation**: **Automatically rotated by kubelet** before expiration

**Key Point:** Once a node successfully bootstraps:
1. The bootstrap token is no longer needed (can expire safely)
2. The node uses a client certificate for all future authentication
3. Kubelet automatically rotates this certificate (built-in feature since Kubernetes 1.8+)

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

### Bootstrap Token Mode Issues

```bash
# Verify bootstrap token exists and is valid
kubectl get secret -n kube-system | grep bootstrap-token

# Check bootstrap token details (decode base64 values)
kubectl get secret bootstrap-token-<token-id> -n kube-system -o yaml

# Verify RBAC bindings
kubectl get clusterrolebinding aks-flex-node-bootstrapper
kubectl get clusterrolebinding aks-flex-node-auto-approve-csr

# Check certificate signing requests
kubectl get csr

# Approve pending CSR if needed
kubectl certificate approve <csr-name>
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
