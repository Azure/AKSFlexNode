# Joining nodes

Before a customer-managed host can join Azure Kubernetes Service (AKS) as a Flex node, plan two forms of access:

- an Azure identity that lets the Flex Node agent retrieve desired state and reconcile the host's AKS Machine resource;
- short-lived Kubernetes bootstrap data that lets the worker and daemon establish Kubernetes credentials.

These are separate requirements. A public AKS API endpoint, for example, provides network reachability but doesn't authenticate the host to Azure or Kubernetes.

## Before you begin

- Create a Flex node pool in the target AKS cluster.
- Prepare a dedicated Ubuntu 24.04 host that meets the requirements in the [operator guide](operator-first-boot.md#prerequisites).
- Provide network connectivity from the host to Azure Resource Manager, Microsoft Entra ID, required artifact endpoints, and the AKS API server.
- Provide the node, pod, service, DNS, and control-plane callback connectivity required by your network design.
- Use a unique lowercase hostname suitable for a Kubernetes Node, or configure `agent.nodeName`.

## Understand the two credentials

### Azure host identity

The long-running Flex Node agent uses an Azure identity to access its Machine resource. Choose one identity based on where the host runs:

| Host environment | Recommended identity | Credential responsibility |
| --- | --- | --- |
| Server already connected to Azure Arc | Azure Arc managed identity | Azure Arc provides a secretless identity. You operate the Arc connection. |
| Azure virtual machine | System-assigned or user-assigned managed identity | Azure provides a secretless identity through the Instance Metadata Service. |
| Other virtual machine or bare metal host | Service principal | You create, protect, rotate, and remove the credential. Prefer a certificate over a client secret. |

Grant the selected identity only the access required at the target AKS cluster scope. Complete role assignment before bootstrap and allow time for propagation.

### Kubernetes bootstrap data

Flex node pool bootstrap data includes a short-lived Kubernetes bootstrap token, API server address and certificate authority data, component settings, and artifact locations. Retrieve fresh data immediately before host bootstrap. Don't print it, include it in logs, or store it in a shared environment file.

For managed identity and service principal workflows, retrieve the data in the Bash environment and transfer it to the host through the protected procedure in the [operator guide](operator-first-boot.md). For Azure Arc, let the host retrieve fresh data through its Arc identity.

After TLS bootstrap, kubelet uses its issued client certificate for ongoing Kubernetes API access. The daemon uses a separate Kubernetes credential for lifecycle operations.

## Managed identity

Use managed identity when the flex node host is an Azure VM.

- Use the system-assigned identity when the identity belongs to one VM.
- Use a user-assigned identity when you need to manage or reuse the identity separately. Set `azure.managedIdentity.clientId` when the VM has more than one eligible identity.

The rendered runtime config selects managed identity with this shape:

```json
{
  "azure": {
    "managedIdentity": {},
    "arc": { "enabled": false }
  }
}
```

This fragment isn't a complete config. Merge the identity selection with current Flex node pool bootstrap data by using the version-matched bootstrap workflow.

## Azure Arc

Use Azure Arc managed identity when the host is already connected as an Arc-enabled server. Flex Node doesn't install, connect, disconnect, or remove the Azure Connected Machine agent.

Before bootstrap:

- confirm that `azcmagent show` reports `Connected`;
- confirm that `himdsd.service` is active;
- verify that the Arc machine resource represents the intended host;
- grant the Arc machine principal access at the target AKS cluster scope.

Run `scripts/bootstrap.sh --auth arc --fetch-bootstrap-data` with the target cluster and pool values. The host uses the Hybrid Instance Metadata Service (HIMDS) to authenticate and retrieve fresh pool data. The installed Flex Node service orders itself after and wants `himdsd.service`.

The rendered identity selection has this shape:

```json
{
  "azure": {
    "arc": { "enabled": true }
  }
}
```

The final runtime config also contains the short-lived Kubernetes bootstrap data returned by AKS.

## Service principal

Use a service principal when the host can't use Azure VM or Azure Arc managed identity. Prefer a certificate credential over a long-lived client secret.

Store the credential in a nonempty, root-owned regular file with no group or world access, such as mode `0600`. The file can contain:

- a client secret;
- a PEM certificate chain and RSA private key; or
- an unencrypted PFX certificate and private key, when the filename has a `.pfx` suffix.

Configure `azure.servicePrincipal.clientSecretFile` with the protected path. The agent detects the credential type from the file contents. Certificate authentication sends the leaf thumbprint and public certificate chain for directly registered certificates and Subject Name/Issuer policies.

```json
{
  "azure": {
    "servicePrincipal": {
      "tenantId": "<tenant-id>",
      "clientId": "<client-id>",
      "clientSecretFile": "/etc/aks-flex-node/credentials/sp-client-certificate"
    },
    "arc": { "enabled": false }
  }
}
```

Don't pass a service principal secret on the command line or place it directly in a checked-in or shared config. Keep the credential file available while the host remains attached because the daemon uses it for Azure reconciliation.

## Bootstrap token evaluation path

The repository config helper can create Kubernetes bootstrap credentials and a standalone config for development and lab evaluation:

1. Run [`scripts/aks-flex-config setup-node-rbac`](../../scripts/aks-flex-config) to apply bootstrap RBAC.
2. Run `scripts/aks-flex-config generate-node-config --bootstrap-token` to create a token and render cluster connection data.
3. Copy the config to the host with mode `0600`.
4. Run `aks-flex-node preflight` and `aks-flex-node start`.

A bootstrap-token-only config doesn't provide a durable Azure identity. Machine registration is best effort by default in this mode. Use the identity-backed operator workflow when the host must register and continuously reconcile an AKS Machine resource.

See [AKS Flex Config Helper](aks-flex-config.md) for helper behavior and [Operations](operations.md#preflight) for preflight options.

## Verify the selected path

Before host bootstrap, confirm all of the following conditions:

- Exactly one durable Azure identity is selected: Azure Arc, managed identity, or service principal.
- The identity is associated with the intended host and authorized at the target AKS cluster scope.
- Fresh Kubernetes bootstrap data is available through the selected workflow.
- Credential-bearing files are root-owned with mode `0600`.
- The host resolves and reaches the AKS API server and required Azure and artifact endpoints.
