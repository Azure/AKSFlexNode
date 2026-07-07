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
