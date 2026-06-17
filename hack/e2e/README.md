# AKS Flex Node E2E Tests

The E2E suite provisions an AKS cluster and three Ubuntu VMs in Azure, joins the VMs as Flex Nodes, validates workloads, exercises unjoin/rejoin behavior, validates repave, collects logs, and tears down the resources.

## Prerequisites

| Tool | Purpose |
|------|---------|
| `az` | Azure CLI, authenticated with `az login`. |
| `jq` | JSON processing. |
| `kubectl` | Kubernetes operations. |
| `ssh` / `scp` | VM access and artifact copy. |
| `openssl` | Bootstrap token generation. |
| `go` | Build the agent binary unless `--binary` is supplied. |

## Quick Start

```bash
export E2E_RESOURCE_GROUP=rg-aks-flex-node-e2e
export E2E_LOCATION=westus2

./hack/e2e/run.sh
# or
make e2e
```

The default `all` command runs:

1. Build the local `aks-flex-node` binary unless `--binary` or `--skip-build` is used.
2. Deploy AKS and three VMs with Bicep.
3. Join all three VMs.
4. Validate node readiness, node-problem-detector status, and run smoke workloads.
5. Unjoin all Flex Nodes and verify they are absent.
6. Rejoin all Flex Nodes and validate again.
7. Run local-machine-driven repave validation.
8. Collect logs and clean up Azure resources.

## Commands

`run.sh` accepts a command as its first positional argument. When omitted, it defaults to `all`.

| Command | Description |
|---------|-------------|
| `all` | Full flow: build, infra, join, validate, unjoin, validate absent, rejoin, validate, repave, logs, cleanup. |
| `infra` | Deploy AKS cluster and three VMs via Bicep. |
| `join` | Join all Flex Node VMs. |
| `join-msi` | Join only the managed-identity node. |
| `join-token` | Join only the bootstrap-token node. |
| `join-kubeadm` | Join only the kubeadm-style bootstrap-token node. |
| `unjoin` | Unjoin all Flex Node VMs. |
| `unjoin-msi` | Unjoin only the managed-identity node. |
| `unjoin-token` | Unjoin only the bootstrap-token node. |
| `unjoin-kubeadm` | Unjoin only the kubeadm-style node. |
| `validate` | Verify joined nodes, node-problem-detector status, and run smoke tests. |
| `validate-absent` | Verify Flex Node objects are absent after unjoin. |
| `smoke` | Run smoke workloads only. |
| `upgrade-drift` | Validate local-machine-driven repave to the alternate nspawn side. |
| `logs` | Collect logs from VMs. |
| `cleanup` | Collect logs and delete Azure resources. |
| `status` | Print the current E2E state file. |

## Options

| Flag | Env Var | Description |
|------|---------|-------------|
| `-g`, `--resource-group` | `E2E_RESOURCE_GROUP` | Azure resource group for test resources. Required. |
| `-l`, `--location` | `E2E_LOCATION` | Azure region. Required. |
| `-b`, `--binary` | `E2E_BINARY` | Path to a pre-built `aks-flex-node` binary. |
| `-s`, `--suffix` | `E2E_NAME_SUFFIX` | Unique suffix for resource names. Defaults to epoch seconds. |
| `--skip-cleanup` | `E2E_SKIP_CLEANUP=1` | Keep Azure resources after the run. |
| `--skip-build` | `E2E_SKIP_BUILD=1` | Skip local build. Requires `--binary` or `E2E_BINARY`. |
| `--debug` | `E2E_DEBUG=1` | Enable verbose debug logging. |

Additional environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `E2E_SSH_KEY_FILE` | auto-detected | SSH public key used for VM access. |
| `E2E_WORK_DIR` | `/tmp/aks-flex-node-e2e` | Working directory for state, configs, and logs. |
| `E2E_KUBERNETES_VERSION` | `1.35.0` | Kubernetes version used in generated node configs. |
| `E2E_CONTAINERD_VERSION` | `2.0.4` | Containerd version used in generated node configs. |
| `E2E_RUNC_VERSION` | `1.1.12` | Runc version used in generated node configs. |
| `E2E_TARGET_AGENT_POOL_NAME` | `aksflexnodes` | Target AKS agent pool name written to generated node configs. |
| `E2E_SSH_WAIT_TIMEOUT` | `300` | Timeout in seconds while waiting for SSH. |
| `E2E_NODE_JOIN_TIMEOUT` | `300` | Timeout in seconds while waiting for node bootstrap. |
| `E2E_POD_READY_TIMEOUT` | `120` | Timeout in seconds while waiting for smoke pods. |
| `E2E_DRIFT_UPGRADE_TIMEOUT` | `900` | Timeout in seconds while waiting for repave. |
| `AZURE_SUBSCRIPTION_ID` | auto-detected | Azure subscription. |
| `AZURE_TENANT_ID` | auto-detected | Azure tenant. |

## Join Modes

The suite validates three join paths:

| VM | Auth Mode | Join Path |
|----|-----------|-----------|
| `vm-e2e-msi-*` | Managed Identity | Generated managed-identity config and `aks-flex-node start` flow. |
| `vm-e2e-token-*` | Bootstrap Token | Kubernetes bootstrap token, RBAC, generated config, and `aks-flex-node start` flow. |
| `vm-e2e-kubeadm-*` | Bootstrap Token | Kubeadm-style bootstrap resources plus generated config and `aks-flex-node start` flow. |

The bootstrap-token VM is provisioned with an uppercase guest OS hostname while
its Azure resource name remains lowercase. This verifies that an omitted
`agent.nodeName` is derived from the normalized hostname and still joins the
cluster under the lowercase VM name.

Each join path uploads the locally built binary, renders a config file, installs the binary through `scripts/install.sh` with `AKS_FLEX_NODE_LOCAL_BINARY`, and starts the node through a transient systemd unit. The installed agent service is then validated with systemd checks.

## Repave Validation

The `upgrade-drift` command validates the local-machine-driven repave path:

1. Ensure the selected mode is joined.
2. Update the local machine goal on the VM.
3. Delete the Kubernetes `Node` object to trigger repave.
4. Wait for the active nspawn side to report the desired kubelet version.
5. Wait for the Kubernetes `Node` to become `Ready` again.
6. Run a smoke workload on the repaved node.

Run it after infrastructure is deployed:

```bash
./hack/e2e/run.sh infra
./hack/e2e/run.sh join
./hack/e2e/run.sh upgrade-drift
```

## Iterative Development

Use subcommands to deploy infrastructure once and iterate quickly:

```bash
./hack/e2e/run.sh infra

./hack/e2e/run.sh join-msi
./hack/e2e/run.sh join-token
./hack/e2e/run.sh join-kubeadm

./hack/e2e/run.sh validate
./hack/e2e/run.sh logs
./hack/e2e/run.sh cleanup
```

Use a local binary without rebuilding:

```bash
make build
./hack/e2e/run.sh --binary ./aks-flex-node --skip-build join-token
```

Keep resources for debugging:

```bash
./hack/e2e/run.sh --skip-cleanup all
```

## Makefile Targets

```bash
make e2e          # Full E2E run.
make e2e-infra    # Deploy infrastructure only.
make e2e-cleanup  # Clean up E2E resources.
```

## Project Layout

```text
hack/e2e/
  run.sh                  Main entry point and command dispatcher.
  infra/
    main.bicep            AKS, VNet, NSG, VMs, identities, and role assignments.
  lib/
    common.sh             Logging, prerequisites, config, state, and SSH helpers.
    infra.sh              Bicep deployment, outputs, and kubeconfig setup.
    node-join.sh          Shared join/unjoin orchestration and remote install helper.
    node-join-msi.sh      Managed identity join/unjoin.
    node-join-token.sh    Bootstrap token join/unjoin.
    node-join-kubeadm.sh  Kubeadm-style bootstrap-token join/unjoin.
    upgrade-drift.sh      Local machine goal repave validation.
    validate.sh           Node readiness and smoke tests.
    cleanup.sh            Log collection and Azure resource cleanup.
```

## State And Logs

`run.sh` persists deployment outputs to `$E2E_WORK_DIR/state.json`. This lets later commands reuse the same infrastructure.

Useful commands:

```bash
./hack/e2e/run.sh status
./hack/e2e/run.sh logs
```

Logs are collected under `$E2E_WORK_DIR/logs/`.

## Troubleshooting

- **Missing prerequisites:** run `./hack/e2e/run.sh --help` and confirm `az`, `jq`, `kubectl`, `ssh`, `scp`, and `openssl` are available.
- **Azure auth failures:** run `az account show` and `az login` if needed.
- **SSH failures:** inspect `state.json` for VM public IPs and confirm the SSH key configured by `E2E_SSH_KEY_FILE` is available.
- **Node join failures:** run `./hack/e2e/run.sh logs` and inspect agent, bootstrap unit, kubelet, containerd, and node-problem-detector logs.
- **Repave failures:** check `aks-flex-node-agent` logs, `machinectl list`, and kubelet versions inside `kube1` and `kube2`.
- **Leftover resources:** run `E2E_RESOURCE_GROUP=<rg> ./hack/e2e/run.sh cleanup`.
