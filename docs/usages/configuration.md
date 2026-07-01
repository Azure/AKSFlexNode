# Configuration

AKS Flex Node reads a JSON config file passed with `--config`.

```bash
aks-flex-node start --config /etc/aks-flex-node/config.json
```

## Top-Level Sections

| Name | Type | Description |
|------|------|-------------|
| `azure` | object | Azure subscription, target AKS cluster, and authentication settings. |
| `agent` | object | Local agent logging and runtime behavior. |
| `components` | object | Kubernetes, container runtime, and sandbox image settings. |
| `bootstrap` | object | Bootstrap settings such as the rootfs OCI image. |
| `networking` | object | Cluster networking settings and optional CNI plugin version override. |
| `node` | object | Kubelet, labels, taints, and node registration settings. |
| `npd` | object | Optional node-problem-detector version override. |

## Azure

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.subscriptionId` | string | Optional Azure subscription that owns the target AKS cluster. Defaults from `azure.targetCluster.resourceId` when omitted. | `44654aed-2753-4b88-9142-af7132933b6b` |
| `azure.tenantId` | string | Microsoft Entra tenant ID. Required only when Azure Arc is enabled. | `70a036f6-8e4d-4615-bad6-149c02e7720d` |
| `azure.cloud` | string | Optional Azure cloud environment label used as a fallback when `azure.resourceManagerEndpoint` is omitted. | `AzurePublicCloud` |
| `azure.resourceManagerEndpoint` | string | Optional Azure Resource Manager endpoint emitted by RP bootstrap data. When omitted, it is derived from `azure.cloud` and defaults to public Azure. | `https://management.azure.com` |
| `azure.targetCluster` | object | Target AKS cluster metadata. | `{}` |
| `azure.targetAgentPoolName` | string | Optional target AKS agent pool for FlexNode machine registration (`TargetAgentPoolName` in the agent config). Defaults to `aksflexnodes`. | `flexnode-edge` |

## Target Cluster

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.targetCluster.resourceId` | string | Full ARM resource ID of the AKS cluster. | `/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.ContainerService/managedClusters/<name>` |
| `azure.targetCluster.location` | string | Azure region of the AKS cluster. | `canadacentral` |

## Authentication

At least one join or Azure authentication method must be configured. `azure.bootstrapToken` can be combined with one Azure authentication method (`azure.arc`, `azure.managedIdentity`, or `azure.servicePrincipal`) so kubelet bootstrap and ARM Machine registration can use different credentials. Only one Azure authentication method can be enabled at a time.

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.bootstrapToken` | object | Kubernetes bootstrap token authentication. | `{ "token": "abcdef.0123456789abcdef" }` |
| `azure.managedIdentity` | object | Azure managed identity authentication for Azure VMs. | `{}` |
| `azure.arc` | object | Azure Arc machine registration and identity settings. | `{ "enabled": true }` |
| `azure.servicePrincipal` | object | Service principal authentication using static app credentials. | `{ "clientId": "<client-id>" }` |

## Bootstrap Token

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.bootstrapToken.token` | string | Kubernetes bootstrap token in `<token-id>.<token-secret>` format. | `abcdef.0123456789abcdef` |

## Managed Identity

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.managedIdentity.clientId` | string | Optional client ID for user-assigned managed identity. Omit for system-assigned identity or single-identity hosts. | `00000000-0000-0000-0000-000000000000` |

## Azure Arc

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.arc.enabled` | boolean | Enables Azure Arc registration flow. | `true` |
| `azure.arc.machineName` | string | Name of the Arc machine resource. | `edge-node-01` |
| `azure.arc.resourceGroup` | string | Resource group for the Arc machine resource. | `edge-rg` |
| `azure.arc.location` | string | Azure region for the Arc machine resource. | `westus2` |
| `azure.arc.tags` | object | Optional tags applied to the Arc machine resource. | `{ "environment": "lab" }` |

## Service Principal

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `azure.servicePrincipal.tenantId` | string | Microsoft Entra tenant ID for the service principal. | `70a036f6-8e4d-4615-bad6-149c02e7720d` |
| `azure.servicePrincipal.clientId` | string | Application client ID. | `00000000-0000-0000-0000-000000000000` |
| `azure.servicePrincipal.clientSecret` | string | Application client secret. Store carefully and rotate regularly. | `<client-secret>` |

## Agent

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `agent.logLevel` | string | Agent log verbosity. | `info` |
| `agent.logDir` | string | Host directory for agent logs. | `/var/log/aks-flex-node` |
| `agent.nodeName` | string | Optional Kubernetes node name override. Defaults to the host hostname. | `edge-node-01` |
| `agent.machineReconcileInterval` | duration string | Daemon interval for re-reading machine state. Uses Go duration syntax. | `10m` |
| `agent.e2eMode` | boolean | Uses the local file-backed machine client for E2E tests. | `false` |
| `agent.requireMachineRegistration` | boolean | Fails bootstrap when the AKS machine resource cannot be read or created. When false, registration is best-effort. | `false` |
| `agent.machineOperationMode` | string | MachineOperation handling mode. | `auto` |

## Components

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `components.kubernetes` | string | Kubernetes version for kubelet and related binaries. For AKS joins, use the target cluster version. | `1.34.3` |
| `components.containerd` | string | Optional containerd version override. | `2.0.4` |
| `components.runc` | string | Optional runc version override. | `1.1.12` |
| `components.sandboxImage` | string | Optional CRI sandbox/pause image used by containerd. When omitted, the shared agent default is used. | `mcr.microsoft.com/oss/kubernetes/pause:3.9` |

## Bootstrap

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `bootstrap.ociImage` | string | Optional nspawn rootfs OCI image used during bootstrap. When omitted, the shared agent default image selection is used. | `ghcr.io/example/aks-flex-node-rootfs:ubuntu-24.04` |
| `bootstrap.offlineArtifacts.source` | string | Optional complete offline binary artifact bundle source. Supports absolute paths, `file://`, and unauthenticated `oci://` artifact references. The value is rendered as a strict Go template with `.KubernetesVersion` and `.KubernetesVersionNoV`. | `/opt/aks-flex-node/artifacts/{{ .KubernetesVersion }}` |

## Networking

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `networking.dnsServiceIP` | string | Cluster DNS service IP. | `10.0.0.10` |
| `networking.cniVersion` | string | Optional CNI plugin version override. | `v1.6.2` |

## Node

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `node.maxPods` | integer | Maximum pods registered for the node. | `110` |
| `node.labels` | object | Labels applied during node registration. | `{ "workload": "edge" }` |
| `node.taints` | string array | Taints applied during node registration. | `["dedicated=edge:NoSchedule"]` |
| `node.kubelet` | object | Kubelet-specific settings. | `{}` |

## Kubelet

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `node.kubelet.verbosity` | integer | Kubelet log verbosity. | `2` |
| `node.kubelet.imageGCHighThreshold` | integer | Image garbage collection high threshold percentage. | `85` |
| `node.kubelet.imageGCLowThreshold` | integer | Image garbage collection low threshold percentage. | `80` |
| `node.kubelet.clusterFQDN` | string | Kubernetes API server FQDN. Required for bootstrap token mode. | `example.hcp.canadacentral.azmk8s.io` |
| `node.kubelet.caCertData` | string | Base64-encoded cluster CA data. Required for bootstrap token mode. | `<base64-ca-data>` |
| `node.kubelet.nodeIP` | string | Optional node IP override for kubelet `--node-ip`. | `10.0.0.4` |

## Component Versions

| Name | Type | Description | Sample Value |
|------|------|-------------|--------------|
| `npd.version` | string | Optional node-problem-detector version override. | `v1.35.1` |

## Legacy Config Compatibility

AKS Flex Node temporarily accepts the pre-RP config shape used by earlier builds. At load time, these legacy fields are adapted into the RP-shaped runtime config.

| Legacy Field | Runtime Config Field |
|--------------|----------------------|
| `kubernetes.version` | `components.kubernetes` |
| `containerd.version` | `components.containerd` |
| `runc.version` | `components.runc` |
| `cni.version` | `networking.cniVersion` |
| `node.kubelet.dnsServiceIP` | `networking.dnsServiceIP` |
| `node.kubelet.serverURL` | `node.kubelet.clusterFQDN` |

## Sample Configurations

### Bootstrap Token

Use this for the quickstart path where the host joins with Kubernetes TLS bootstrapping.

```json
{
  "azure": {
    "subscriptionId": "<subscription-id>",
    "tenantId": "<tenant-id>",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "<agent-pool-name>",
    "bootstrapToken": {
      "token": "<token-id>.<token-secret>"
    },
    "arc": { "enabled": false },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  },
  "node": {
    "kubelet": {
      "clusterFQDN": "<aks-api-server-fqdn>",
      "caCertData": "<base64-ca-data>"
    }
  },
  "networking": {
    "dnsServiceIP": "<cluster-dns-service-ip>"
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  },
  "components": { "kubernetes": "<aks-kubernetes-version>" }
}
```

### Managed Identity

Use this for an Azure VM with a managed identity assigned.

```json
{
  "azure": {
    "subscriptionId": "<subscription-id>",
    "tenantId": "<tenant-id>",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "<agent-pool-name>",
    "managedIdentity": {},
    "arc": { "enabled": false },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  },
  "components": { "kubernetes": "<aks-kubernetes-version>" }
}
```

### Azure Arc

Use this when the host should be registered as an Arc-enabled server.

```json
{
  "azure": {
    "subscriptionId": "<subscription-id>",
    "tenantId": "<tenant-id>",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "<agent-pool-name>",
    "arc": {
      "enabled": true,
      "machineName": "<arc-machine-name>",
      "resourceGroup": "<arc-resource-group>",
      "location": "<arc-location>",
      "tags": {
        "node-type": "flex"
      }
    },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  },
  "components": { "kubernetes": "<aks-kubernetes-version>" }
}
```

### Service Principal

Use this when the host should authenticate with static service principal credentials.

```json
{
  "azure": {
    "subscriptionId": "<subscription-id>",
    "tenantId": "<tenant-id>",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "<agent-pool-name>",
    "servicePrincipal": {
      "tenantId": "<tenant-id>",
      "clientId": "<client-id>",
      "clientSecret": "<client-secret>"
    },
    "arc": { "enabled": false },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  },
  "agent": {
    "logLevel": "info",
    "logDir": "/var/log/aks-flex-node"
  },
  "components": { "kubernetes": "<aks-kubernetes-version>" }
}
```

### Component Version Overrides

Add these sections when you need to pin runtime component versions explicitly.

```json
{
  "components": {
    "kubernetes": "1.34.3",
    "containerd": "2.0.4",
    "runc": "1.1.12",
    "sandboxImage": "mcr.microsoft.com/oss/kubernetes/pause:3.9"
  },
  "networking": {
    "cniVersion": "v1.6.2"
  },
  "npd": {
    "version": "v1.35.1"
  }
}
```

### Bootstrap Image And Offline Artifact Overrides

Add this section when you need to pin the nspawn rootfs image or use a complete offline binary artifact bundle. `offlineArtifacts.source` follows the Unbounded offline artifact bundle layout and includes `manifest.json` plus Kubernetes, containerd, runc, CNI, crictl, and optional sandbox image archive artifacts.

```json
{
  "bootstrap": {
    "ociImage": "ghcr.io/example/aks-flex-node-rootfs:ubuntu-24.04",
    "offlineArtifacts": {
      "source": "/opt/aks-flex-node/artifacts/{{ .KubernetesVersion }}"
    }
  }
}
```
