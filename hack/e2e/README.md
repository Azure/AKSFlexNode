# AKS Flex Node E2E Tests

End-to-end tests that provision an AKS cluster and three Ubuntu VMs in Azure,
join them as flex nodes (one via MSI, one via bootstrap token, one via kubeadm
join using `apply -f`), and run smoke tests.

## Prerequisites

| Tool | Purpose |
|------|---------|
| `az` | Azure CLI, authenticated (`az login`) |
| `jq` | JSON processing |
| `kubectl` | Kubernetes operations |
| `ssh` / `scp` | VM access |
| `openssl` | Bootstrap token generation |
| `go` | Build the agent binary (unless `--binary` is supplied) |

## Quick Start

```bash
# 1. Set required variables (or copy .env.example to .env and fill in values)
export E2E_RESOURCE_GROUP=rg-e2e-test
export E2E_LOCATION=westus2

# 2. Run the full suite
./hack/e2e/run.sh
# or
make e2e
```

This will build the agent binary, deploy infrastructure via Bicep, join all
three nodes, run validations, collect logs, and tear everything down.

## Commands

`run.sh` accepts a single command as its first positional argument. When
omitted it defaults to `all`.

| Command | Description |
|---------|-------------|
| `all` | Full flow: build, infra, join, validate, cleanup (default) |
| `infra` | Deploy AKS cluster + 3 VMs via Bicep |
| `join` | Join all nodes to the cluster |
| `join-msi` | Join only the MSI-authenticated node |
| `join-token` | Join only the bootstrap-token node |
| `join-kubeadm` | Join only the kubeadm node (`apply -f` with `KubeadmNodeJoin`) |
| `validate` | Verify nodes joined and run smoke tests |
| `smoke` | Run smoke tests only (nginx pods on flex nodes) |
| `logs` | Collect logs from VMs |
| `cleanup` | Collect logs then delete Azure resources |
| `status` | Print the current state file (deployment outputs) |

## Options

| Flag | Env Var | Description |
|------|---------|-------------|
| `-g`, `--resource-group` | `E2E_RESOURCE_GROUP` | Azure resource group (required) |
| `-l`, `--location` | `E2E_LOCATION` | Azure region (required) |
| `-b`, `--binary` | `E2E_BINARY` | Path to a pre-built `aks-flex-node` binary |
| `-s`, `--suffix` | `E2E_NAME_SUFFIX` | Unique suffix for resource names (default: epoch) |
| `--skip-cleanup` | `E2E_SKIP_CLEANUP=1` | Keep resources after tests finish |
| `--skip-build` | `E2E_SKIP_BUILD=1` | Skip building the binary (requires `--binary`) |
| `--debug` | `E2E_DEBUG=1` | Enable verbose debug logging |

Additional environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `E2E_SSH_KEY_FILE` | (auto) | SSH public key for VM access |
| `E2E_WORK_DIR` | `/tmp/aks-flex-node-e2e` | Working directory for state and artifacts |
| `E2E_KUBERNETES_VERSION` | `1.35.0` | Kubernetes version for the node config |
| `E2E_CONTAINERD_VERSION` | `2.0.4` | Containerd version |
| `E2E_RUNC_VERSION` | `1.1.12` | Runc version |
| `AZURE_SUBSCRIPTION_ID` | (auto-detected) | Azure subscription |
| `AZURE_TENANT_ID` | (auto-detected) | Azure tenant |

## Node Join Modes

The E2E suite tests three node join methods:

| VM | Auth Mode | Join Method |
|----|-----------|-------------|
| `vm-e2e-msi-*` | Managed Identity (MSI) | `aks-flex-node agent --config config.json` |
| `vm-e2e-token-*` | Bootstrap Token | `aks-flex-node agent --config config.json` |
| `vm-e2e-kubeadm-*` | Bootstrap Token | `aks-flex-node apply -f kubeadm-join.json` |

The kubeadm VM uses the `apply -f` command with a JSON action file that
contains a sequence of component actions (configure OS, download CRI/kube/CNI
binaries, start containerd, then `KubeadmNodeJoin`) to join the cluster using
the kubeadm join flow.

## Iterative Development

The subcommands make it easy to deploy infrastructure once and iterate on the
join or validation steps without re-provisioning every time.

```bash
# Deploy infrastructure (keeps it running)
./hack/e2e/run.sh infra

# Iterate on the join logic
./hack/e2e/run.sh join-msi
./hack/e2e/run.sh join-token
./hack/e2e/run.sh join-kubeadm

# Run validation
./hack/e2e/run.sh validate

# Check deployment state at any point
./hack/e2e/run.sh status

# Collect VM logs for debugging
./hack/e2e/run.sh logs

# Clean up when done
./hack/e2e/run.sh cleanup
# or
make e2e-cleanup
```

## Makefile Targets

```bash
make e2e          # Full E2E run (same as ./hack/e2e/run.sh all)
make e2e-infra    # Deploy infrastructure only
make e2e-cleanup  # Tear down resources
```

## Project Layout

```
hack/e2e/
  run.sh              Main entry point / orchestrator
  infra/
    main.bicep        Bicep template (AKS + VNet + NSG + 3 VMs + role assignments)
  lib/
    common.sh             Logging, prereqs, config, state management, SSH helpers
    infra.sh              Bicep deployment, output extraction, kubeconfig fetch
    node-join.sh          Shared helper (_deploy_and_start_agent) + node_join_all orchestration
    node-join-msi.sh      MSI auth node join (node_join_msi)
    node-join-token.sh    Bootstrap token node join (node_join_token)
    node-join-kubeadm.sh  Kubeadm apply -f node join (node_join_kubeadm)
    validate.sh           Node-ready checks and smoke tests (nginx pods)
    cleanup.sh            Log collection and Azure resource teardown
```

## State File

`run.sh` persists deployment outputs (IPs, cluster name, etc.) to a JSON state
file at `$E2E_WORK_DIR/state.json`. This lets each subcommand pick up where the
previous one left off. Use `run.sh status` to inspect it.

## Troubleshooting

- **SSH failures**: The Bicep template creates an NSG allowing port 22. Ensure
  your SSH key is available (defaults to `~/.ssh/id_rsa.pub`). Check the state
  file for the correct VM public IPs with `run.sh status`.
- **Node not joining**: Run `run.sh logs` to pull `journalctl` and agent logs
  from all VMs. Logs are saved to `$E2E_WORK_DIR/logs/`.
- **Kubeadm join failures**: Check `kubeadm-agent-journal.log` and
  `kubeadm-kubelet.log` in the logs directory. The `apply -f` approach runs
  sequentially; each action step must succeed before the next one starts.
- **Timeouts**: Adjust `E2E_SSH_WAIT_TIMEOUT`, `E2E_NODE_JOIN_TIMEOUT`, or
  `E2E_POD_READY_TIMEOUT` environment variables (in seconds).
- **Leftover resources**: If a previous run didn't clean up, run
  `E2E_RESOURCE_GROUP=<rg> ./hack/e2e/run.sh cleanup` to delete the resource
  group.
