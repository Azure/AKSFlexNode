# Operations

This guide covers host inspection, current lifecycle operations, reset, and troubleshooting for AKS Flex Node.

> [!NOTE]
> The agent implements direct Azure Machine create and read operations, but direct ARM status updates are currently skipped. Unbounded `MachineOperation` handling is available only when the corresponding Kubernetes custom resource is installed and `agent.machineOperationMode` isn't `disable`.

## Current lifecycle capabilities

| Operation | Interface | Current behavior |
| --- | --- | --- |
| Kubernetes settings repave | Azure Machine goal plus Kubernetes Node deletion | Provisions the alternate nspawn side and applies the changed settings version. Automatic nspawn rollback isn't implemented. |
| Node restart | Unbounded `NodeReboot` MachineOperation | Restarts the active nspawn worker and waits for kubelet. |
| Agent upgrade | Unbounded `AgentUpgrade` MachineOperation or direct candidate activation | Uses blue-green host binaries and restores the last-known-good binary when activation fails. |
| Agent reset | Unbounded `AgentReset` MachineOperation | Removes local node runtime, marks the operation complete, and stops the daemon. |
| Local reset | `aks-flex-node reset` or the uninstall script | Removes agent-managed host runtime. Cluster-side Node and Azure Machine cleanup are separate operations. |

AKS or the operator owns workload disruption decisions. Cordon and drain the Kubernetes Node before a disruptive operation when the surrounding control-plane workflow hasn't already done so.

## Preflight

Run preflight before mutating the host. The command validates the config, resolves the nspawn goal state, and checks host prerequisites, API server reachability, rootfs image reachability, and bootstrap artifact sources.

```bash
aks-flex-node preflight --config /etc/aks-flex-node/config.json
```

Preflight exits non-zero when a fatal check fails. Use JSON output for automation:

```bash
aks-flex-node preflight --config /etc/aks-flex-node/config.json --output json
```

Useful options:

```bash
aks-flex-node preflight \
  --config /etc/aks-flex-node/config.json \
  --ignore-preflight-errors=<check-name>[,<check-name>...] \
  --fail-on-warnings
```

When `bootstrap.offlineArtifacts.source` is configured, missing host packages are fatal because offline bootstrap cannot rely on package installation during `start`.

## Start

Start installs host components, starts the nspawn-backed worker, installs the systemd unit, and starts the agent daemon.

```bash
aks-flex-node start --config /etc/aks-flex-node/config.json
```

`bootstrap` is currently an alias for `start`, but new docs should prefer `start`.

## Agent service

Check the long-running agent service:

```bash
systemctl status aks-flex-node-agent
systemctl is-active aks-flex-node-agent
journalctl -u aks-flex-node-agent -f
```


## Managed agent upgrade

When the Unbounded `MachineOperation` API is installed, submit an `AgentUpgrade` with an HTTP or HTTPS release archive and, when available, the SHA-256 of the compressed archive:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-agent-worker-01
spec:
  machineRef: worker-01
  operationKind: AgentUpgrade
  parameters:
    downloadURL: https://example.com/aks-flex-node-linux-amd64.tar.gz
    sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

The archive must contain exactly the architecture-specific release member used by AKS Flex Node (`aks-flex-node-linux-amd64` or `aks-flex-node-linux-arm64`). The `sha256` parameter is optional; when supplied, the daemon verifies the compressed archive digest. Prefer HTTPS and a digest for production downloads. Plain HTTP is intended for explicitly trusted networks such as a VM-local loopback server; omit the digest only when both the archive source and transport path are trusted. The daemon always verifies the candidate `version` command before switching its blue/green binary links. It also atomically updates the binary in the active nspawn rootfs so kubelet exec authentication uses the same version.

The restarted daemon marks the operation `Complete`. If the candidate cannot remain running, systemd restores the last-known-good host and nspawn binaries and marks the operation `Failed`. URL query strings, which may contain SAS credentials, are omitted from logs and operation status.

MachineOperations are cluster-scoped. The daemon group requires cluster-wide read access to MachineOperations and Nodes, plus MachineOperation status update access, so restrict who can create operations and treat parameter values as sensitive API data. Prefer short-lived, read-only download credentials.

```bash
kubectl get machineoperation upgrade-agent-worker-01 -w
```

A host provisioning system that has already authenticated and staged a candidate can activate it directly without creating an Unbounded `MachineOperation`:

```bash
sudo /var/tmp/aks-flex-node-candidate agent-upgrade --preflight
sudo /var/tmp/aks-flex-node-candidate agent-upgrade
```

The candidate must be staged separately from the installed binary. Direct activation and `MachineOperation` activation share one host lock and refuse to overlap with a pending operation signal. Both paths verify the candidate, switch the same blue/green layout, and restore last-good on activation failure. If `aks-flex-node-agent.service` is active, direct activation restarts it, verifies the running executable, and synchronizes the active nspawn exec-credential binary. If the service is already inactive during reset/rejoin provisioning, activation preserves that stopped state; the subsequent bootstrap starts the service and worker.

## Managed node restart and reset

When the Unbounded MachineOperation API is installed, the daemon handles `NodeReboot` and `AgentReset` operations in addition to `AgentUpgrade`.

A `NodeReboot` restarts the active nspawn worker. An `AgentReset` removes both nspawn sides, host networking artifacts, local daemon state, and agent-managed runtime directories, publishes the MachineOperation result, and stops the daemon service. Neither operation drains workloads; complete cluster-side disruption orchestration before creating the operation.

These operations use a cluster-scoped API. Restrict who can create MachineOperations and monitor the operation status until it reaches `Complete` or `Failed`.

## Nspawn worker

Inspect the local nspawn-backed worker:

```bash
ACTIVE_MACHINE=$(sudo jq -r .activeMachine /etc/aks-flex-node/daemon-state.json)
case "$ACTIVE_MACHINE" in
  kube1|kube2) ;;
  *) echo "Invalid active machine: $ACTIVE_MACHINE" >&2; exit 1 ;;
esac

machinectl list
machinectl status "$ACTIVE_MACHINE"
journalctl -M "$ACTIVE_MACHINE" -u kubelet -f
journalctl -M "$ACTIVE_MACHINE" -u containerd -f
```

The daemon state file contains operational state, not bootstrap credentials, but it is root-owned and protected from modification. Repave flows alternate between `kube1` and `kube2`. The current flow stops the active side before it provisions and starts the alternate side.

## Verify node state

From your workstation:

```bash
kubectl get nodes -o wide
kubectl describe node <node-name>
```

By default, `<node-name>` is the target host hostname unless `agent.nodeName` is set.

## Reset and uninstall

Before local removal, cordon and drain the Kubernetes Node unless the controlling AKS workflow has already done so:

```bash
kubectl cordon <node-name>
kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
```

Run the version-matched uninstall script as root on the host:

```bash
# Use the full immutable commit SHA recorded for the installed release.
AKS_FLEX_NODE_COMMIT="<40-character-installed-release-commit-sha>"
if ! [[ "$AKS_FLEX_NODE_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then
  echo "AKS_FLEX_NODE_COMMIT must be a full commit SHA" >&2
  exit 1
fi

UNINSTALL_SCRIPT="$(mktemp)"
trap 'rm -f "$UNINSTALL_SCRIPT"' EXIT
if ! curl -fsSLo "$UNINSTALL_SCRIPT" \
  "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_COMMIT}/scripts/uninstall.sh"; then
  echo "Failed to download the uninstall script" >&2
  exit 1
fi
chmod 0700 "$UNINSTALL_SCRIPT"
sudo "$UNINSTALL_SCRIPT" --force
rm -f "$UNINSTALL_SCRIPT"
trap - EXIT
```

The uninstall script runs the local reset, removes the installed binary and agent-managed directories, and preserves externally managed Azure Arc software. It doesn't remove the Kubernetes Node, Azure Machine, Flex node pool, host VM, Azure identity, or role assignments.

After local cleanup, remove or verify removal of the Kubernetes Node and Azure Machine through the controlling AKS workflow. For a standalone evaluation flow, remove the Node explicitly:

```bash
kubectl delete node <node-name>
```

## Troubleshooting checklist

- Check `aks-flex-node-agent` state and restart count with `systemctl show aks-flex-node-agent -p ActiveState -p SubState -p NRestarts`.
- Check agent logs with `journalctl -u aks-flex-node-agent --no-pager -n 200`.
- Check kubelet logs with `journalctl -M <active-machine> -u kubelet --no-pager -n 200`.
- Check container runtime logs with `journalctl -M <active-machine> -u containerd --no-pager -n 200`.
- Determine the active side from `/etc/aks-flex-node/daemon-state.json`; don't assume it is always `kube1` after repave.
- Check Node conditions and events with `kubectl describe node <node-name>`.
- Check the Azure Machine with `az aks machine show` when the preview CLI supports that command.
- Treat repeated HTTP 401 or 403 responses from the Machine API as an Azure identity, role assignment, pool, or resource-name problem even when the Kubernetes Node is `Ready`.
- Don't include tokens, kubeconfig content, private keys, certificates with private keys, or signed URLs in diagnostic output or public issues.
