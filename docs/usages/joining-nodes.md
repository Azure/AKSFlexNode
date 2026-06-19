# Joining Nodes

This guide summarizes the supported ways to join a virtual machine or bare metal host to an existing AKS cluster as a Flex Node.

## Before You Begin

- Create or choose an existing AKS cluster.
- Prepare a host that can reach the AKS API server over outbound HTTPS.
- Use a host size with enough CPU and memory for nspawn startup and Kubernetes components; the validated quickstart used a 4-vCPU Azure VM.
- Run host-side install and start commands as root.
- Make the host hostname match the Kubernetes node name you expect, or set `agent.nodeName` in the config.

## Bootstrap Token

Bootstrap token mode is the recommended quickstart path. It uses Kubernetes TLS bootstrapping and does not require Azure credentials on the host after the config is rendered.

High-level flow:

1. Run [`scripts/aks-flex-config setup-node-rbac`](../../scripts/aks-flex-config) to setup required node bootstrap RBAC permissions.
2. Run `scripts/aks-flex-config generate-node-config --bootstrap-token` to create a bootstrap token, fetch AKS cluster metadata, and render the host config.
3. Copy the generated config to `/etc/aks-flex-node/config.json` on the target host.
4. Run `aks-flex-node start --config /etc/aks-flex-node/config.json`.
5. Verify with `kubectl get nodes -o wide`.

See the repository [README](../../README.md#getting-started) for the complete bootstrap token walkthrough, and [AKS Flex Config Helper](aks-flex-config.md) for all helper command options.

## Managed Identity

Managed identity mode is intended for Azure VMs that already have a managed identity assigned.

Minimal config shape:

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
  }
}
```

Use `managedIdentity.clientId` when the VM has multiple user-assigned identities.

## Azure Arc

Azure Arc mode registers the host as an Arc-enabled server and uses Arc-managed identity for Azure integration.

Minimal config shape:

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
      "tags": {}
    },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  }
}
```

Arc mode requires Azure permissions for Arc onboarding and any required role assignment work.

## Service Principal

Service principal mode uses static Azure application credentials.

Minimal config shape:

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
  }
}
```

Store service principal credentials carefully and rotate them regularly.

## Authentication Mode Selection

Only one authentication mode can be configured at a time:

- `azure.bootstrapToken`
- `azure.managedIdentity`
- `azure.arc.enabled: true`
- `azure.servicePrincipal`
