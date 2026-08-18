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
4. Run `aks-flex-node preflight --config /etc/aks-flex-node/config.json` to validate host, cluster, rootfs, and artifact prerequisites without mutating the node.
5. Run `aks-flex-node start --config /etc/aks-flex-node/config.json`.
6. Verify with `kubectl get nodes -o wide`.

See the repository [README](../../README.md#getting-started) for the complete bootstrap token walkthrough, [AKS Flex Config Helper](aks-flex-config.md) for all helper command options, and [Operations](operations.md#preflight) for preflight options and JSON output.

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

Azure Arc mode uses the system-assigned identity of an already-connected Arc-enabled server. Arc installation, onboarding, role assignment, and removal remain operator responsibilities.

Minimal config shape:

```json
{
  "azure": {
    "subscriptionId": "<subscription-id>",
    "resourceManagerEndpoint": "https://management.azure.com",
    "targetAgentPoolName": "<agent-pool-name>",
    "arc": { "enabled": true },
    "bootstrapToken": { "token": "<fetched-bootstrap-token>" },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  },
  "node": {
    "kubelet": {
      "clusterFQDN": "<fetched-api-server-fqdn>",
      "caCertData": "<fetched-base64-ca>"
    }
  }
}
```

Use `scripts/bootstrap.sh --auth arc --fetch-bootstrap-data` to populate the token, API server FQDN, CA, and component settings. Before Flex Node bootstrap, `azcmagent show` must report `Connected`, `himdsd` must be active, and the Arc machine principal must have permission to call AKS `listBootstrapData` and operate its AKS Machine resource. For Arc configs, the installed Flex Node systemd unit orders itself after and wants `himdsd.service`.

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
      "clientSecretFile": "/run/credentials/aks-flex-node-sp"
    },
    "arc": { "enabled": false },
    "targetCluster": {
      "resourceId": "<aks-resource-id>",
      "location": "<aks-location>"
    }
  }
}
```

The credential file contains either the client secret or a PEM/unencrypted PFX application certificate and private key; the agent detects the credential type from its contents. PFX certificate files must use a `.pfx` suffix. The file must be a non-empty regular file with no group/world access (for example, mode 0600). Only one of `clientSecret` or `clientSecretFile` can be configured. Certificate authentication sends the leaf thumbprint (`x5t`) and public chain (`x5c`), supporting directly registered certificates and Subject Name/Issuer trust policies.

## Authentication Mode Selection

Only one durable Azure identity mode can be configured at a time:

- `azure.managedIdentity`
- `azure.arc.enabled: true`
- `azure.servicePrincipal`

`azure.bootstrapToken` can accompany one of these identities. It is required for Arc because kubelet consumes the short-lived join credential returned by AKS RP while the host agent uses Arc identity for ARM.
