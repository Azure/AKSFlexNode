# Azure Container Linux

> [!WARNING]
> **Experimental.** Azure Container Linux support is experimental, whether the agent runs in containers, as this page describes, or [on the host](#the-agent-on-the-host). Don't use it for production nodes. Its settings, units, and `AgentUpgrade` parameters may change between releases without a migration path.
>
> - The agent in containers needs Azure Container Linux 3.0.20261026 or later, which isn't yet in the image AKS uses.
> - It has run only on local VMs, not on Azure VMs, so bootstrap with a managed identity or a service principal is untested ([#399](https://github.com/Azure/AKSFlexNode/issues/399)).
> - Not supported yet: offline install, GPU hosts ([#416](https://github.com/Azure/AKSFlexNode/issues/416)), and pulling the agent's image from a registry that needs credentials.

Azure Container Linux mounts `/usr` read-only and is provisioned only by Ignition. On it, the agent runs as privileged containerd containers of its own image, which each release publishes. Nothing of the agent is installed on the host: its Ignition config installs only systemd units and settings, and the units run containers of the image. Installing the agent on the host instead, as [Hosts provisioned by Ignition](getting-started.md#hosts-provisioned-by-ignition) describes, also works, as an interim; see [The agent on the host](#the-agent-on-the-host).

The agent keeps its config, commands, unit name, MachineOperations, and reset: it joins, repaves, restarts its machine, upgrades, and resets as on any host.

## Requirements

- **Azure Container Linux 3.0.20261026 or later.** Its SELinux policy lets the agent's containers reach the host's systemd, and it labels containerd's files, so the containers run in the domain that policy applies to. The agent doesn't run correctly on earlier builds.
- **A release that publishes the agent's image.** Its release notes have an Azure Container Linux section, which gives the image, `ghcr.io/azure/aks-flex-node-agent`, pinned by digest, for `linux/amd64` and `linux/arm64`.
- **Access to `ghcr.io`.** The host pulls the image at every bootstrap, so a host that can't reach it can't join for the first time.

## Install and join

[`scripts/aks-flex-node-agent-container.bu`](../../scripts/aks-flex-node-agent-container.bu) is a [Butane](https://coreos.github.io/butane/) config that installs the agent, from two files in the same directory:

- `agent-container.env`, the install's settings: `AKS_FLEX_NODE_IMAGE`, the image pinned by digest, as the release notes give it.
- `bootstrap.env`, bootstrap's settings: one `AKS_FLEX_NODE_*` variable per line, as `bootstrap.sh --help` lists them.

In your Bash environment, with `AKS_RESOURCE_ID` and `FLEX_POOL_NAME` set as in [Getting started](getting-started.md):

```bash
export AKS_FLEX_NODE_VERSION="<release>"
# The image pinned by digest, from the release notes.
export AKS_FLEX_NODE_IMAGE="ghcr.io/azure/aks-flex-node-agent:${AKS_FLEX_NODE_VERSION}@sha256:<digest>"

mkdir -m 0700 node-ignition && cd node-ignition

curl -fsSLO "https://raw.githubusercontent.com/Azure/AKSFlexNode/${AKS_FLEX_NODE_VERSION}/scripts/aks-flex-node-agent-container.bu"

echo "AKS_FLEX_NODE_IMAGE=${AKS_FLEX_NODE_IMAGE}" > agent-container.env

cat > bootstrap.env <<SETTINGS
AKS_FLEX_NODE_AUTH=msi
AKS_FLEX_NODE_MSI_CLIENT_ID=<user-assigned-managed-identity-client-id>
AKS_FLEX_NODE_FETCH_BOOTSTRAP_DATA=true
AKS_FLEX_NODE_CLUSTER_RESOURCE_ID=${AKS_RESOURCE_ID}
AKS_FLEX_NODE_AGENT_POOL_NAME=${FLEX_POOL_NAME}
SETTINGS

butane --strict --files-dir . aks-flex-node-agent-container.bu > node.ign
```

Without Butane installed, run its container image instead:

```bash
podman run --rm -i -v "$PWD:/files:ro,z" quay.io/coreos/butane:release \
  --strict --files-dir /files < aks-flex-node-agent-container.bu > node.ign
```

Use `node.ign` as the host's Ignition config; on Azure, that's the VM's custom data. It carries bootstrap's settings, so keep it as private as they are.

`bootstrap.env` is read one `VARIABLE=value` per line:

- Values are taken exactly as written, quotes included, and never expanded, so don't quote them.
- Don't set the agent's version, URL, or digest, `AKS_FLEX_NODE_CONFIG_PATH`, or `AKS_FLEX_NODE_INSTALL_DIR`: the image is the agent, and sets the rest. They're ignored, with a warning.
- For a base config, uncomment its entry in the template, put `base-config.json` next to it, and set `AKS_FLEX_NODE_BASE_CONFIG_FILE=/etc/aks-flex-node/base-config.json`.
- For a service principal, uncomment the credential entries in the template, put the secret or certificate file next to it, and set `AKS_FLEX_NODE_SP_CLIENT_SECRET_FILE` or `AKS_FLEX_NODE_SP_CLIENT_CERTIFICATE_FILE` to its path under `/etc/aks-flex-node/credentials/`. Don't put the secret itself in `bootstrap.env`.

On first boot, Ignition writes the settings and enables `aks-flex-node-bootstrap.service`. Once the network is online, the unit pulls the image and runs `bootstrap.sh` in a container of it. Bootstrap installs and starts `aks-flex-node-agent.service`, which runs the agent's daemon in a container of the image.

The unit retries failures with a delay that grows to five minutes. It doesn't retry those that retrying can't fix, for which bootstrap exits with status 78: a setting that can't work, or a `start` that failed after changing the host, which then needs a [reset](#reset-and-rejoining). Those leave the unit failed. Once the agent's unit is installed, later boots skip it. Follow progress on the host with `journalctl -u aks-flex-node-bootstrap.service`.

## Upgrading the agent

An [AgentUpgrade](operations.md#managed-agent-upgrade) names an image in the parameters that name a release archive on other hosts:

- `downloadURL` is `oci://` and the image's repository, with or without a tag.
- `sha256` is the image's digest.

Both are in the release notes:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: upgrade-agent-worker-01
spec:
  machineRef: worker-01
  operationKind: AgentUpgrade
  parameters:
    downloadURL: oci://ghcr.io/azure/aks-flex-node-agent:<release>
    sha256: <digest>
```

The digest comes only from `sha256`. A request without it, with it in the URL as well, or naming a release archive fails with `InvalidParameters` before anything is pulled.

The agent pulls the image, which containerd verifies against the digest, and checks that its agent runs. Then it switches to the image and restarts on it. If the new agent can't keep running, the recovery unit restores the last-good image, and the operation ends `Failed`. At its next start, the agent removes the images upgrades pulled that it won't run again.

## Reset and rejoining

`AgentReset`, the deletion signal, and `aks-flex-node reset` reset the node as on other hosts. On the host, run the reset with `systemctl start aks-flex-node-reset.service`, which runs `aks-flex-node reset` in a container of the image. Afterwards nothing of the agent runs. The install stays: its settings, its bootstrap and reset units, and its image.

To join again, write new inputs to `/etc/aks-flex-node/bootstrap.env`, mode `0600`, and any file they name, and run `systemctl start aks-flex-node-bootstrap.service`. To join with another image than the install's, name it as `AKS_FLEX_NODE_IMAGE` in `/etc/aks-flex-node/agent-image.env`; reset removes it.

To remove the agent, reprovision the host without its Ignition config.

## Checking the agent

On the host:

- `systemctl status aks-flex-node-agent.service`, and `journalctl -u aks-flex-node-agent.service` for the agent's log.
- `ctr --namespace aks-flex task ls` lists the agent's running containers. The daemon's is `aks-flex-node`.
- To run the agent's commands, run them in its container:

  ```bash
  sudo ctr --namespace aks-flex task exec --exec-id status aks-flex-node aks-flex-node --help
  ```

## OS updates

How Azure Container Linux flex nodes take OS updates, in place with a reboot or by reimaging, and what cordons and drains the node first, isn't defined yet; it's tracked in [#379](https://github.com/Azure/AKSFlexNode/issues/379). The agent's state is under `/etc`, `/opt`, and `/var`, which an in-place image update keeps, but that hasn't been tested. Until it is, treat an OS update like any other disruptive operation: cordon and drain the node first, and check that it's `Ready` and `aks-flex-node-agent.service` is active afterwards.

## The agent on the host

Installing the agent on the host, as [Hosts provisioned by Ignition](getting-started.md#hosts-provisioned-by-ignition) describes, also works on Azure Container Linux, and on other hosts whose `/usr` is read-only. Agent upgrades work there as they do on other hosts, with every file under `/opt/unbounded/agent`. An AgentUpgrade or a direct activation stages the candidate in the blue/green slot under `/opt/unbounded/agent/lib/aks-flex-node` that isn't in use, switches `aks-flex-node-current` to it, synchronizes the active nspawn machine's binary, and restores last-good the same way when the candidate can't keep running. No earlier release could install on these hosts, so the [move from `/usr/local`](migration.md) never runs there, and nothing is written under `/usr/local`.

- **Minimum version.** The first release that installs under `/opt/unbounded/agent` is the oldest that runs on these hosts. An AgentUpgrade to an earlier release is accepted, but that release looks for its files under `/usr/local`, and its daemon can't start. As for any upgrade whose daemon can't start, systemd runs the recovery unit as soon as the daemon fails, which restores the last-good binary, and the operation ends `Failed`. For a daemon that exits at once, that took about 10 seconds; the kubelet keeps running. Don't target an earlier release.
- **Start limit.** On Azure Container Linux, the SELinux policy denies root `systemctl reset-failed`, which the recovery script runs before it restarts the agent, as the daemon does before an upgrade's restart or the host root's move. The recovery unit then reports `failed` even when the rollback worked, and nothing clears the agent unit's start limit, five starts in five minutes: restarts beyond it leave the agent stopped until it's started by hand or the host reboots. Space out AgentUpgrades on these hosts. This is tracked in [#385](https://github.com/Azure/AKSFlexNode/issues/385). The agent in its containers isn't affected.
- **Reprovisioning.** A host provisioned or reimaged from its Ignition config installs the version the config pins, which may be older than the rest of the pool. Upgrade it once it joins; see [Agent version](getting-started.md#agent-version).
- **Rejoining after a reset.** Reset removes the first-boot unit, so the host doesn't join again on its own. Reimage it, or create it again, from its Ignition config; see [Rejoining after a reset](getting-started.md#rejoining-after-a-reset).
