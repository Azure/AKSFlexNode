# AKS Flex Node Join Lab

This lab walks through joining a VM or bare-metal (BM) host to an existing AKS
cluster using [AKS Flex Node](https://github.com/Azure/AKSFlexNode). AKS Flex
Node allows you to extend your AKS cluster beyond Azure by attaching external
compute resources as worker nodes.

> **Warning:** AKS Flex Node is currently an experimental feature and is subject
> to change. APIs, CLI flags, and behaviors may change without notice in future
> releases. Do not use this in production environments without understanding the
> risks.

## Prerequisites

### Create an AKS Cluster

Create an AKS cluster with Azure AD enabled and fetch the kubeconfig to your
local machine:

```bash
# Set variables
RESOURCE_GROUP="my-resource-group"
CLUSTER_NAME="my-aks-cluster"

# Create the AKS cluster
az aks create \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME"

# Fetch the admin kubeconfig to your local machine
az aks get-credentials \
    --resource-group "$RESOURCE_GROUP" \
    --name "$CLUSTER_NAME" \
    --admin \
    --overwrite-existing
```

Verify connectivity:

```bash
kubectl get nodes
```

Expected output:

```
NAME                                STATUS   ROLES    AGE   VERSION
aks-nodepool1-12345678-vmss000000   Ready    <none>   5m    v1.30.0
```

### Download the aks-flex-node Binary

Download the latest `aks-flex-node` binary from the
[releases page](https://github.com/Azure/AKSFlexNode/releases) and upload it
to the VM or BM host:

```bash
# SSH into the target VM / BM host
ssh <user>@<vm-ip>

# Switch to root and run the install script
curl -LO https://github.com/Azure/AKSFlexNode/releases/download/v0.0.10/aks-flex-node-linux-amd64.tar.gz
sudo su
tar -xzf aks-flex-node-linux-amd64.tar.gz
cp aks-flex-node-linux-amd64 /usr/local/bin/aks-flex-node
chmod +x /usr/local/bin/aks-flex-node

# Verify installation
aks-flex-node version
```

Expected output:

```
AKS Flex Node Agent
Version: v0.0.10
Git Commit: b53bf43
Build Time: 2026-02-18T22:58:16Z
```

## Steps

In this lab we will join the VM using a kubeadm-style bootstrapping flow. This
flow uses [Kubernetes TLS bootstrapping](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/)
with bootstrap tokens — the same mechanism that `kubeadm join` relies on. The
node authenticates with a short-lived bootstrap token, obtains a client
certificate via a Certificate Signing Request (CSR), and then uses that
certificate for all subsequent communication with the API server.

### Cluster Preparation

This step creates the necessary configuration on the cluster side to allow the
new node to join. You will create a bootstrap token and the required RBAC
bindings so the node can authenticate and register itself.

Run the following commands from your local machine (where you have `kubectl`
access to the cluster):

#### 1. Generate a bootstrap token

```bash
# Generate token components
TOKEN_ID=$(openssl rand -hex 3)
TOKEN_SECRET=$(openssl rand -hex 8)
BOOTSTRAP_TOKEN="${TOKEN_ID}.${TOKEN_SECRET}"

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
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:aks-flex-node"
EOF
```

Expected output:

```
secret/bootstrap-token-<token-id> created
```

#### 2. Create RBAC bindings

```bash
# Allow the bootstrap group to create certificate signing requests
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

# Auto-approve CSRs from the bootstrap group
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

# Grant node permissions to the bootstrap group
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
  kind: Group
  name: system:bootstrappers:aks-flex-node
EOF
```

Expected output:

```
clusterrolebinding.rbac.authorization.k8s.io/aks-flex-node-bootstrapper created
clusterrolebinding.rbac.authorization.k8s.io/aks-flex-node-auto-approve-csr created
clusterrolebinding.rbac.authorization.k8s.io/aks-flex-node-role created
```

#### 3. Capture cluster connection details

```bash
# Get the API server URL and CA certificate
SERVER_URL=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA_CERT_DATA=$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

# Print the values — you will need these on the node
echo "BOOTSTRAP_TOKEN=${BOOTSTRAP_TOKEN}"
echo "SERVER_URL=${SERVER_URL}"
echo "CA_CERT_DATA=${CA_CERT_DATA}"
```

Save these values. You will use them in the next step when configuring the node.

### Node Preparation

This step runs on the VM or BM host that you want to join to the cluster. You
will create a configuration file with the cluster connection details and the
bootstrap token, then start the `aks-flex-node` agent to bootstrap the node.

#### 1. Create the configuration file

SSH into the target node and create the config file (as root):

```bash
# Set the variables from the previous step
BOOTSTRAP_TOKEN="<token-id>.<token-secret>"
SERVER_URL="<api-server-url>"
CA_CERT_DATA="<base64-ca-cert>"

# Set Azure subscription details
TENANT_ID=$(az account show --query tenantId -o tsv)
SUBSCRIPTION=$(az account show --query id -o tsv)
AKS_RESOURCE_ID="/subscriptions/$SUBSCRIPTION/resourceGroups/<resource-group>/providers/Microsoft.ContainerService/managedClusters/<cluster-name>"
LOCATION="<cluster-location>"

# Create the config directory and file
mkdir -p /etc/aks-flex-node
tee /etc/aks-flex-node/config.json > /dev/null <<EOF
{
  "azure": {
    "subscriptionId": "$SUBSCRIPTION",
    "tenantId": "$TENANT_ID",
    "cloud": "AzurePublicCloud",
    "bootstrapToken": {
      "token": "$BOOTSTRAP_TOKEN"
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
  "node": {
    "kubelet": {
      "serverURL": "$SERVER_URL",
      "caCertData": "$CA_CERT_DATA"
    }
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  }
}
EOF
```

#### 2. Start the agent

```bash
aks-flex-node agent --config /etc/aks-flex-node/config.json
```

The agent will execute the full bootstrap pipeline: install containerd, download
Kubernetes binaries, configure kubelet with the bootstrap token, set up CNI
networking, and start all services. You should see output similar to:

```
...
Created symlink /etc/systemd/system/multi-user.target.wants/containerd.service → /etc/systemd/system/containerd.service.
level=info msg="Restarting containerd service to apply CNI configuration" func="[services_installer.go:44]"
level=info msg="Enabling and starting kubelet service" func="[services_installer.go:51]"
Created symlink /etc/systemd/system/multi-user.target.wants/kubelet.service → /etc/systemd/system/kubelet.service.
level=info msg="Waiting for kubelet to start..." func="[services_installer.go:58]"
active
level=info msg="Enabling and starting node-problem-detector service" func="[services_installer.go:63]"
Created symlink /etc/systemd/system/multi-user.target.wants/node-problem-detector.service → /etc/systemd/system/node-problem-detector.service.
level=info msg="All services enabled and started successfully" func="[services_installer.go:69]"
level=info msg="bootstrap step: ServicesEnabled completed successfully with duration 3.043020178s" func="[executor.go:147]"
level=info msg="AKS node bootstrap completed successfully (duration: 13.929720781s, stepCount: 10)" func="[executor.go:106]"
level=info msg="bootstrap completed successfully (duration: 13.929720781s, steps: 10)" func="[commands.go:321]"
level=info msg="Bootstrap completed successfully, transitioning to daemon mode..." func="[commands.go:126]"
level=info msg="Starting periodic status collection daemon (status: 1 minutes, bootstrap check: 2 minute)" func="[commands.go:177]"
level=error msg="kubectl command failed: exit status 1 with output: Error from server (NotFound): nodes \"<node-name>\" not found\n" func="[collector.go:217]"
level=info msg="Collecting managed cluster spec for <rg>/<cluster-name>" func="[collector.go:115]"
```

#### 3. Verify the node joined

From your local machine, check that the new node appears in the cluster:

```bash
kubectl get nodes
```

Expected output:

```
NAME                                STATUS   ROLES    AGE   VERSION
aks-nodepool1-12345678-vmss000000   Ready    <none>   1h    v1.30.0
<node-name>                         Ready    <none>   30s   v1.30.0
```

The new node should appear with `Ready` status after a short time.
