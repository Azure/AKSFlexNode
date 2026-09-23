# Operations

This guide summarizes common host and cluster operations for AKS Flex Node.

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

## Agent Service

Check the long-running agent service:

```bash
systemctl status aks-flex-node-agent
systemctl is-active aks-flex-node-agent
journalctl -u aks-flex-node-agent -f
```

## Managed Agent Upgrade

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

### Optional Machine publication

When a same-named `unbounded-cloud.io/v1alpha3` Machine is published, `NodeReboot`, `AgentReset`, and `AgentUpgrade` capture its `metadata.generation` before execution and report it as `status.observedMachineGeneration` on completion or execution failure. This is the observed **CR generation**, not evidence that ARM settings were applied. Invalid requests that never execute do not report a generation.

The daemon reads the Machine directly, without a Machine watch or startup dependency. Missing Machine objects or a missing Machine API preserve legacy operation execution and omit the generation. Other read errors (including authorization and transient failures) prevent execution and are retried; they are not treated as absence. Daemon credentials need `get` on `machines.unbounded-cloud.io`; the example controller deployment role includes this read-only permission.

AgentUpgrade stores the captured generation in its existing durable upgrade signal. Restart, rollback, and publication retries use that value even if the Machine changes. Older signals without the field remain readable and omit the generation. Terminal status-write retries within the initiating process do not repeat host operations. These in-memory results are bound to the operation UID and discarded when that operation disappears, is replaced, or is already terminal; conflict retries recheck identity before publishing.

Bootstrap adds `unbounded-cloud.io/machine=<ARM Machine name>` to the Node's registration labels. The ARM Machine name is `agent.nodeName`, not the `kube1`/`kube2` nspawn slot. This reserved relationship label overrides a conflicting custom value without changing customer goal labels or the existing AKS markers.

Machine/Node names longer than the 63-character label-value limit remain supported: the relationship label is omitted, never truncated or hashed. Use the existing equal-name lookup (`Machine.metadata.name == Node.metadata.name`) or an explicit `MachineOperation.spec.machineRef`; label-only consumers cannot match these names.

The existing Node reconciliation loop also repairs the relationship label where its credentials already permit Node patches. It preserves other labels and annotations and removes an incorrect relationship label for long names. The example daemon role remains read-only on Nodes: a denied patch is logged and does not block the AKS goal lifecycle. Such existing Nodes need an administrator-applied label or normal Node recreation to receive it; this feature does not broaden Node write privileges. Machine CR edits never drive repave/reset, and the daemon does not write Machine spec or status.

```bash
kubectl get machineoperation upgrade-agent-worker-01 -w
```

A host provisioning system that has already authenticated and staged a candidate can activate it directly without creating an Unbounded `MachineOperation`:

```bash
sudo /var/tmp/aks-flex-node-candidate agent-upgrade --preflight
sudo /var/tmp/aks-flex-node-candidate agent-upgrade
```

The candidate must be staged separately from the installed binary. Direct activation and `MachineOperation` activation share one host lock and refuse to overlap with a pending operation signal. Both paths verify the candidate, switch the same blue/green layout, and restore last-good on activation failure. If `aks-flex-node-agent.service` is active, direct activation restarts it, verifies the running executable, and synchronizes the active nspawn exec-credential binary. If the service is already inactive during reset/rejoin provisioning, activation preserves that stopped state; the subsequent bootstrap starts the service and worker.

## Nspawn Worker

Inspect the local nspawn-backed worker:

```bash
machinectl list
machinectl status kube1
journalctl -M kube1 -u kubelet -f
journalctl -M kube1 -u containerd -f
```

Repave flows use `kube1` and `kube2` as local blue-green nspawn machine names.

## Verify Node State

From your workstation:

```bash
kubectl get nodes -o wide
kubectl describe node <node-name>
```

By default, `<node-name>` is the target host hostname unless `agent.nodeName` is set.

## Reset And Uninstall

Run the uninstall script as root on the host:

```bash
curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/uninstall.sh | bash -s -- --force
```

Then remove the Kubernetes `Node` object from your workstation:

```bash
kubectl delete node <node-name>
```

## Troubleshooting Checklist

- Check `aks-flex-node-agent` logs with `journalctl -u aks-flex-node-agent -f`.
- Check kubelet logs with `journalctl -M kube1 -u kubelet -f`.
- Check container runtime logs with `journalctl -M kube1 -u containerd -f`.
- Check bootstrap token CSRs with `kubectl get csr`.
- Check node status with `kubectl describe node <node-name>`.
