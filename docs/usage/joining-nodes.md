# Joining nodes

Before a customer-managed host can join Azure Kubernetes Service (AKS) as a Flex node, plan two forms of access:

- an Azure identity that lets the Flex Node agent retrieve desired state and reconcile the host's AKS Machine resource;
- short-lived Kubernetes bootstrap data that lets the worker and daemon establish Kubernetes credentials.

These are separate requirements. A public AKS API endpoint, for example, provides network reachability but doesn't authenticate the host to Azure or Kubernetes.

## Before you begin

- Create a Flex node pool in the target AKS cluster.
- Prepare a dedicated Ubuntu 24.04 host that meets the requirements in the [operator guide](getting-started.md#prerequisites).
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

Assign Azure Kubernetes Service Flex Node Agent Role to the selected identity at the target ARM agent-pool scope. Complete role assignment before bootstrap and allow time for propagation.

### Kubernetes bootstrap data

Flex node pool bootstrap data includes a short-lived Kubernetes bootstrap token, API server address and certificate authority data, component settings, and artifact locations. Retrieve fresh data immediately before host bootstrap. Don't print it, include it in logs, or store it in a shared environment file.

After TLS bootstrap, kubelet uses its issued client certificate for ongoing Kubernetes API access. The daemon uses a separate Kubernetes credential for lifecycle operations.

## Managed identity

Use managed identity when the Flex node host is an Azure VM. Use the system-assigned identity when it belongs to one VM, or a user-assigned identity when it must be managed or reused separately.

Identity settings (merge with fetched bootstrap data; not a complete bootstrap config):

```json
{
  "azure": {
    "managedIdentity": {},
    "arc": { "enabled": false }
  }
}
```

This fragment isn't a complete config. Merge the identity selection with current Flex node pool bootstrap data by using the version-matched bootstrap workflow.

For managed identity, service principal, and Arc modes, the operator must assign
**Azure Kubernetes Service Flex Node Agent Role**
to the host principal at the target agent-pool scope. See
[role assignment guidance](getting-started.md#assign-the-host-identitys-pool-scoped-role).

For this least-privilege flow, use `scripts/bootstrap.sh --auth msi
--fetch-bootstrap-data` with the target cluster and pool arguments as shown in
the [first-boot walkthrough](getting-started.md#5-download-and-run-the-bootstrap-script).
Keep the returned bootstrap token, API server FQDN, and CA in the config:
kubelet and the daemon use separate Kubernetes token/CSR credentials.

## Azure Arc

Use Azure Arc managed identity when the host is already connected as an Arc-enabled server. Flex Node doesn't install, connect, disconnect, or remove the Azure Connected Machine agent.

Before bootstrap:

- confirm that `azcmagent show` reports `Connected`;
- confirm that `himdsd.service` is active;
- verify that the Arc machine resource represents the intended host;
- assign the Arc machine principal the Flex Node Agent Role at the target ARM agent-pool scope.

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

Store the credential in a nonempty, root-owned regular file with no group or world access, such as mode `0600`. The file can contain a client secret, a PEM certificate chain and RSA private key, or an unencrypted PFX certificate and private key when the filename has a `.pfx` suffix.

Identity settings (merge with fetched bootstrap data; not a complete bootstrap config):

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

Use `scripts/bootstrap.sh --auth service-principal --fetch-bootstrap-data` with the target cluster, pool, and protected credential arguments from the [first-boot walkthrough](getting-started.md#5-download-and-run-the-bootstrap-script). Retain the fetched Kubernetes bootstrap settings; the pool-scoped role authorizes ARM operations, not Kubernetes API access.

<a id="bootstrap-token-evaluation-path"></a>
## Retrieve pool-issued bootstrap data

Use the version-matched `scripts/bootstrap.sh --fetch-bootstrap-data` flow shown in the [operator guide](getting-started.md#5-download-and-run-the-bootstrap-script). AKS supplies the bootstrap token, API endpoint, CA, component settings, and artifact locations for the selected Flex node pool. No client-side bootstrap RBAC setup is required.

## Verify the selected path

Before host bootstrap, confirm all of the following conditions:

- Exactly one durable Azure identity is selected: Azure Arc, managed identity, or service principal.
- The identity is associated with the intended host and assigned the Flex Node Agent Role at the target ARM agent-pool scope.
- Fresh Kubernetes bootstrap data is available through the selected workflow.
- Credential-bearing files are root-owned with mode `0600`.
- The host resolves and reaches the AKS API server and required Azure and artifact endpoints.
