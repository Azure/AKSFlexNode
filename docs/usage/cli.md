# Command-line reference

The `aks-flex-node` binary provides operator commands and commands used internally by systemd or Kubernetes authentication. Run operator commands as root on the flex node host unless a procedure says otherwise.

```bash
aks-flex-node --help
aks-flex-node <command> --help
```

## Operator commands

| Command | Purpose |
| --- | --- |
| `preflight` | Validate the configuration, host, API server, rootfs image, and bootstrap artifact sources without changing the host. |
| `start` | Bootstrap the nspawn worker, install the agent systemd unit, and start the daemon. |
| `fetch-bootstrap-data` | Retrieve current FlexNodes join data from the AKS resource provider and write it to a protected file. |
| `reset` | Remove AKS Flex Node runtime state while preserving externally managed Azure Arc state. |
| `version` | Print the build version, commit, and build time. |
| `completion` | Generate shell completion output through Cobra. |

### Preflight

```text
aks-flex-node preflight --config <path> [flags]
```

| Flag | Description | Default |
| --- | --- | --- |
| `--config` | Path to the configuration JSON file. Required. | None |
| `--output` | Output format: `text` or `json`. | `text` |
| `--ignore-preflight-errors` | Comma-separated check names whose errors should be reported as warnings. | None |
| `--fail-on-warnings` | Return a failure when any check reports a warning. | `false` |

Use ignored errors only for an explicitly reviewed exception. The output still identifies the affected checks.

### Start

```text
aks-flex-node start --config <path>
```

`bootstrap` is a compatibility alias for `start`. Use `start` in new procedures and automation.

### Fetch bootstrap data

```text
aks-flex-node fetch-bootstrap-data \
  --cluster-resource-id <aks-resource-id> \
  --agent-pool-name <flex-node-pool> \
  --auth <arc|msi|service-principal> \
  --output <protected-json-path> \
  [flags]
```

| Flag | Description | Default |
| --- | --- | --- |
| `--cluster-resource-id` | Target AKS managed cluster resource ID. Required. | None |
| `--agent-pool-name` | Target FlexNodes agent pool name. Required. | None |
| `--auth` | Authentication mode: `arc`, `msi`, or `service-principal`. Required. | None |
| `--output`, `-o` | Protected output path for the returned JSON. Required. | None |
| `--msi-client-id` | Client ID for a user-assigned managed identity. | System-assigned or single available identity |
| `--sp-tenant-id` | Microsoft Entra tenant ID for service principal authentication. | None |
| `--sp-client-id` | Application client ID for service principal authentication. | None |
| `--sp-client-secret-file` | Protected file containing a service principal client secret. | None |
| `--sp-client-certificate-file` | Protected PEM or unencrypted PFX certificate file. | None |
| `--sp-client-credential-file` | Protected secret or certificate file whose type is detected from its contents. | None |
| `--resource-manager-endpoint` | Azure Resource Manager endpoint. | `https://management.azure.com` |
| `--authority-host` | Microsoft Entra authority host. | `https://login.microsoftonline.com` |
| `--api-version` | API version for the AKS `listBootstrapData` action. | `2026-05-02-preview` |

Don't print the output or write it to a group- or world-readable path. It contains short-lived Kubernetes bootstrap credentials. Prefer a protected credential file for service principal authentication. The `AKS_FLEX_NODE_SP_CLIENT_SECRET` environment variable is available for compatibility but is less desirable than a file managed with mode `0600`.

### Reset

```text
aks-flex-node reset
```

`unbootstrap` is a compatibility alias for `reset`. Reset removes local AKS Flex Node state; follow [Reset and uninstall](operations.md#reset-and-uninstall) for the complete host and cluster cleanup sequence.

### Version

```text
aks-flex-node version
```

Use this output when recording a validation run or collecting diagnostics.

## Service and integration commands

These commands are invoked by systemd, generated worker configuration, or managed lifecycle procedures. Don't call them manually unless the relevant procedure explicitly directs you to do so.

| Command | Invoker and purpose |
| --- | --- |
| `daemon --config <path>` | `aks-flex-node-agent.service` runs the long-lived reconciliation daemon. `agent` is a compatibility alias. |
| `token kubelogin` | Kubelet exec authentication obtains an Azure token. Generated kubelet configuration supplies the required flags. |
| `agent-upgrade [--preflight]` | A provisioning or managed upgrade workflow activates the candidate executable. Follow the managed agent upgrade procedure rather than running the installed binary directly. |
| `recover-agent-upgrade` | The systemd recovery unit restores the last-known-good agent after failed activation. |
| `nspawn-lifecycle <pre-start|post-start|reconcile> <kube1|kube2>` | Generated systemd units invoke lifecycle hooks for the active nspawn worker. |

The last three commands are hidden from the top-level help because they are lifecycle implementation interfaces rather than general operator entry points.

## Generated command surface

The following table is generated from the Cobra command tree. Run `make docs-cli-generate` after changing a command, alias, flag, required setting, or default.

<!-- BEGIN GENERATED CLI REFERENCE -->
<!-- Run make docs-cli-generate; do not edit this section manually. -->

| Usage | Description | Aliases | Cobra visibility | Local flags |
| --- | --- | --- | --- | --- |
| `aks-flex-node agent-upgrade [flags]` | Activate this executable as the host agent daemon | — | hidden | `--preflight` |
| `aks-flex-node completion` | Generate the autocompletion script for the specified shell | — | listed | — |
| `aks-flex-node completion bash` | Generate the autocompletion script for bash | — | listed | `--no-descriptions` |
| `aks-flex-node completion fish [flags]` | Generate the autocompletion script for fish | — | listed | `--no-descriptions` |
| `aks-flex-node completion powershell [flags]` | Generate the autocompletion script for powershell | — | listed | `--no-descriptions` |
| `aks-flex-node completion zsh [flags]` | Generate the autocompletion script for zsh | — | listed | `--no-descriptions` |
| `aks-flex-node daemon [flags]` | Run the AKS Flex Node daemon | `agent` | listed | `--config` (required) |
| `aks-flex-node fetch-bootstrap-data [flags]` | Fetch current FlexNodes join data from AKS RP | — | listed | `--agent-pool-name` (required); `--api-version` (default: `2026-05-02-preview`); `--auth` (required); `--authority-host` (default: `https://login.microsoftonline.com`); `--cluster-resource-id` (required); `--msi-client-id`; `--output` (required); `--resource-manager-endpoint` (default: `https://management.azure.com`); `--sp-client-certificate-file`; `--sp-client-credential-file`; `--sp-client-id`; `--sp-client-secret-file`; `--sp-tenant-id` |
| `aks-flex-node help [command]` | Help about any command | — | listed | — |
| `aks-flex-node nspawn-lifecycle` | Run internal nspawn lifecycle hooks | — | hidden | — |
| `aks-flex-node nspawn-lifecycle post-start MACHINE` | Reconcile in-machine state after machine start | — | hidden | — |
| `aks-flex-node nspawn-lifecycle pre-start MACHINE` | Refresh host-side nspawn state before machine start | — | hidden | — |
| `aks-flex-node nspawn-lifecycle reconcile MACHINE` | Restart a machine and run its lifecycle reconciliation | — | hidden | — |
| `aks-flex-node preflight [flags]` | Run non-mutating preflight checks | — | listed | `--config` (required); `--fail-on-warnings`; `--ignore-preflight-errors`; `--output` (default: `text`) |
| `aks-flex-node recover-agent-upgrade [flags]` | Restore the last-known-good agent after a failed upgrade | — | hidden | `--message` |
| `aks-flex-node reset` | Remove AKS node configuration | `unbootstrap` | listed | — |
| `aks-flex-node start [flags]` | Bootstrap the node and start the agent service | `bootstrap` | listed | `--config` (required) |
| `aks-flex-node token` | Kubernetes exec based authentication provider. | — | listed | — |
| `aks-flex-node token kubelogin [flags]` | Retrieves token via Azure/kubelogin. | — | listed | `--client-certificate-file`; `--pop-claims`; `--pop-enabled`; `--server-id` (default: `6dae42f8-4368-4678-94ff-3960e28e3630`) |
| `aks-flex-node version` | Show version information | — | listed | — |
<!-- END GENERATED CLI REFERENCE -->

## See also

- [Configuration](configuration.md)
- [Operations](operations.md)
- [Joining nodes](joining-nodes.md)
