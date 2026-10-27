# Azure Container Linux harness: the agent in a container

This harness runs the AKS Flex Node agent from this repository on Azure
Container Linux (ACL), as privileged containerd containers of its own image,
and joins the VM to a Kubernetes cluster. It boots ACL in a local VM under
QEMU/KVM, provisioned only by the Ignition config rendered from
[`agent-container.bu`](agent-container.bu). It creates a local kind cluster
running `main`'s in-cluster controller, and the VM bootstraps itself into it,
with no Azure resources involved.

[DESIGN.md](DESIGN.md) explains how the agent runs on ACL and what differs from
a host install. This file is how to use the harness.

The harness boots ACL 3.0.20261026, whose SELinux policy lets the agent's
privileged containers talk to the host's systemd over D-Bus, and which labels
containerd's files, so they run as `spc_t`. See [SELinux](DESIGN.md#selinux).

## Requirements

- Linux with KVM: `/dev/kvm` readable and writable
- `qemu-system-x86_64`, `qemu-img`, `qemu-nbd`, and OVMF firmware (the `ovmf` or
  `edk2-ovmf` package)
- `podman`, or `docker` with `ACLC_CONTAINER_ENGINE=docker`, to build the agent
  image and run the registry and Butane
- `kind`, `kubectl`, and `go`, for the cluster and the controller's image. kind
  runs on root Podman, through `sudo`.
- `ip`, `iptables`, `sysctl`, `ssh`, `ssh-keygen`, `setsid`
- `sudo` without a password prompt, for kind, the bridge, TAP device,
  forwarding, and firewall rules. Run `sudo -v` first if your sudo caches
  credentials.
- An ACL image: a local qcow2, or `az`, logged in, to export the gallery image;
  see [The ACL image](#the-acl-image)

## Quick start

```bash
export ACL_IMAGE_RESOURCE_GROUP=my-westus2-group   # only for the first export
hack/acl-container/harness.py up                   # cluster, image, VM, bootstrap
hack/acl-container/harness.py status               # the VM, the agent, machines, nodes
hack/acl-container/harness.py check                # 80 checks, 7 to 10 minutes
hack/acl-container/harness.py down                 # remove the VM, cluster, network, and state
```

`up` takes under five minutes once the ACL image is exported and the image
build is cached. It:

1. creates the bridge `virbr-aclc` on `192.168.112.0/24`, with NAT, and turns
   IPv4 forwarding on if it's off (`down` restores it)
2. creates the kind cluster `aclc` on root Podman, and attaches its node to the
   bridge at `192.168.112.2`, where the VM reaches the API server
3. builds `main`'s `aks-flex-controller` and deploys it from
   `hack/controller-deployment`, with its CSR approver, the MachineOperation
   CRDs, and the bootstrap-token RBAC `scripts/aks-flex-config` applies
4. registers the VM as a machine in the controller's ConfigMap, and creates a
   bootstrap token
5. builds [`image/Containerfile`](image/Containerfile), the agent image, and
   pushes it to a registry it runs on the bridge, `192.168.112.1:5099`
6. renders `agent-container.bu` with Butane, adds bootstrap's inputs, an SSH
   user, a hostname, and a static address, and boots the VM at `192.168.112.10`
7. waits for the bootstrap unit to bootstrap the node, for the agent's container
   to run, and for the node to be Ready

`up --no-agent` boots plain ACL, without the agent or a cluster, to explore the
OS. `up` again replaces the VM and the cluster.

### What `check` covers

- The agent's container: its SELinux context, the host's root and PID
  namespace, the host paths its entrypoint mounts, that `machinectl` and
  `umount` are the host's, and that the agent has no binary of its own on the
  host
- The node: Ready, its machine running, a pod on it, and `kubectl logs`
- Repave from kube1 to kube2, as `main`'s e2e does
- A `NodeReboot` MachineOperation
- The agent's machine: in the host's `machine.slice`, reached from the agent's
  container with `systemd-run --machine` and `machinectl shell`, kept running
  across a restart of the agent's container, and back after a VM reboot
- `AgentUpgrade`: to a good candidate image, to a broken one, which rolls back,
  requests without the image's digest, with it in the URL, or naming an
  archive, which are rejected, and back to the agent's own image
- `AgentReset`, `aks-flex-node reset`, and the deletion signal, each followed by
  bootstrapping the node again

`check --no-reboot` skips the reboot. `check --only GROUP,...` runs some of its groups, in their usual order:
`container`, `cluster`, `repave`, `nodereboot`, `machines`, `upgrade`,
`agentreset`, `reset`, and `deletion`. Each starts from a joined node and
leaves one, so any of them can run alone, such as `--only upgrade,agentreset`
while working on upgrades. `check` ends with how long each group took.

The agent's unit may start 5 times in 5 minutes, and systemd counts its
restarts toward that, including the four `upgrade` causes. So `upgrade` and
`machines` first wait, if they must, for earlier starts to age out; running
`--only upgrade` again right away waits a few minutes.

## Working with the node

The cluster's kubeconfig is in the harness's state directory:

```bash
export KUBECONFIG=.vm/acl-container/kubeconfig
kubectl get nodes -o wide
```

Request MachineOperations as `main`'s demos and e2e do:

```bash
kubectl apply -f - <<'EOF'
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineOperation
metadata:
  name: reboot-1
spec:
  machineRef: acl-flex-proto
  operationKind: NodeReboot     # or AgentReset
EOF
kubectl get machineoperation reboot-1 -w
```

An `AgentUpgrade` names an image rather than an archive, in the same
parameters: `downloadURL` is `oci://` and the image's fully qualified
repository, with or without a tag, and `sha256` is the image's digest, as
`main` requires an archive's SHA-256:

```yaml
spec:
  machineRef: acl-flex-proto
  operationKind: AgentUpgrade
  parameters:
    downloadURL: oci://192.168.112.1:5099/aks-flex-node-agent
    sha256: ...
```

To reset the node by hand, and bootstrap it again:

```bash
hack/acl-container/harness.py ssh sudo systemctl start aks-flex-node-reset
kubectl delete node acl-flex-proto           # as AKS does once the agent stops
# Write /etc/aks-flex-node/bootstrap.env and its base config on the VM, and
# optionally /etc/aks-flex-node/agent-image.env to join with another image, then:
hack/acl-container/harness.py ssh sudo systemctl start aks-flex-node-bootstrap
```

`check` does the same through `bootstrap_again` in `harness.py`.

## On the VM

| Unit | |
|---|---|
| `aks-flex-node-bootstrap.service` | Pulls the image and runs `main`'s `bootstrap.sh` in a short-lived container, at boot, once bootstrap's inputs are there and until the agent is installed |
| `aks-flex-node-agent.service` | Runs the daemon in the container `aks-flex-node`. `start` installs it, and reset removes it. |
| `aks-flex-node-agent-recovery.service` | Restores the last good image when an upgraded agent fails. `start` installs it with the agent's unit. |
| `aks-flex-node-reset.service` | Runs `aks-flex-node reset` in a short-lived container |

| File | |
|---|---|
| `/etc/sysconfig/aks-flex-node-container` | The install's settings: the image, and pull flags. Ignition writes it. |
| `/etc/aks-flex-node/bootstrap.env`, `base-config.json` | Bootstrap's inputs, `AKS_FLEX_NODE_*` variables and the base config they name, with the bootstrap token |
| `/etc/aks-flex-node/agent-image.env` | Another image for the agent than the install's: one `AgentUpgrade` selected, or one bootstrap was given |
| `/etc/aks-flex-node/config.json`, `daemon-state.json` | The agent's config, which bootstrap renders, and its state |

Reset removes everything in `/etc/aks-flex-node` and leaves the settings and
the bootstrap and reset units.

```bash
systemctl status aks-flex-node-agent
journalctl -u aks-flex-node-bootstrap -b      # bootstrap.sh, preflight, and start
journalctl -u aks-flex-node-agent -b          # the entrypoint's checks and the daemon
sudo ctr --namespace aks-flex task ls         # the agent's containers
sudo ctr --namespace aks-flex images ls -q    # its images
```

Each start of a container takes a second or two before its entrypoint runs.

To change what's in the image, edit it and push it:

```bash
hack/acl-container/harness.py push   # rebuild, push, pull on the VM, restart the agent
```

`push` removes an `AgentUpgrade`'s image selection, so the rebuilt image runs.
Machines keep running across it: they belong to the host's systemd, not to the
container.

## Inside the agent's container

```bash
hack/acl-container/harness.py agent          # a shell in the container
hack/acl-container/harness.py agent CMD...   # or one command
```

The container gets the host's root at `/host`, and its entrypoint mounts the
host paths the agent uses at their own paths. The host's own tools run through
`host-exec`: `machinectl`, `systemctl`, `systemd-run`, `ctr`, and the others the
image links to it run under `chroot /host`, and `mount`, `umount`, and
`mountpoint` run in the host's mount namespace. Any other host command runs as
`host-exec COMMAND`:

```bash
machinectl list
systemctl status systemd-nspawn@kube1
systemd-run --machine=kube1 --pipe --wait -- uname -a
host-exec ip -br addr
```

## The ACL image

Without `--image` or `ACL_IMAGE`, `up` boots the Azure build of ACL from the
shared gallery image `ACL_IMAGE_GALLERY_IMAGE`, which it exports to
`.vm/acl-images/<gallery>/<image>/acl-gallery-<version>.qcow2` the first time:
a version is only unique within its gallery image. The export copies
the image to a temporary managed disk in `ACL_IMAGE_RESOURCE_GROUP`, which has
to be in `westus2`, where the image is replicated, downloads the disk's 910 MiB
of written pages, and deletes the disk. It takes about 70 seconds on a fast
link.

The identity `az` is logged in as needs, on the resource group,
`Microsoft.Compute/disks/read`, `write`, `delete`, `beginGetAccess/action` and
`endGetAccess/action`, and on the subscription, read access to the shared
gallery's images and versions. Its tenant has to be one the gallery is shared
with.

`up` exports the version `ACL_IMAGE_VERSION` names, 3.20261027.1 by default,
which has the SELinux rule the agent needs and labels containerd's files. Its
gallery excludes it from
its latest, so set `ACL_IMAGE_VERSION=` (empty) to have `az` ask the gallery
for its latest instead. Once a version is exported, `up` boots it again without
`az`.

## Settings

| Variable | Default | |
|---|---|---|
| `ACL_IMAGE` | | Local ACL qcow2, instead of `--image` |
| `ACL_IMAGE_RESOURCE_GROUP` | | Resource group in `westus2` the gallery image is exported through |
| `ACL_IMAGE_SUBSCRIPTION` | `az`'s default | Subscription of that resource group |
| `ACL_IMAGE_VERSION` | `3.20261027.1` | Gallery image version; empty for the gallery's latest |
| `ACL_IMAGE_GALLERY_IMAGE` | `/SharedGalleries/035db282-f1c8-4ce7-b78f-2a7265d5398c-ACLDEVEL/Images/acl-1es-eval-kvm` | Gallery image |
| `ACL_IMAGE_DIR` | `.vm/acl-images` | Where exported images are kept; `down` keeps them |
| `ACLC_STATE_DIR` | `.vm/acl-container` | VM disk, logs, SSH key, kubeconfig, rendered Ignition |
| `ACLC_CONTAINER_ENGINE` | `podman` if installed, else `docker` | Builds the images, runs the registry and Butane |
| `ACLC_KIND_CLUSTER` | `aclc` | The kind cluster's name |
| `ACLC_KIND_NODE_IMAGE` | kind's default | The kind node image |
| `ACLC_SUBNET` | `192.168.112` | First three octets of the bridge subnet |
| `ACLC_DNS` | `8.8.8.8 8.8.4.4` | The VM's DNS servers |
| `ACLC_REGISTRY_IMAGE` | `docker.io/library/registry:2` | The registry's image |
| `ACLC_REGISTRY_PORT` | `5099` | Registry port on the bridge and on loopback |
| `ACLC_SERVE_PORT` | `8298` | Port the Ignition config is served on during `up` |
| `ACLC_VM_MEMORY`, `ACLC_VM_CPUS` | `4096`, `4` | VM size |
| `ACLC_AGENT_TAG` | `proto` | Agent image tag |
| `ACLC_BUTANE_IMAGE` | `quay.io/coreos/butane:v0.29.0` | Butane image |

The subnet, interface names, and ports differ from the other harnesses in
`hack`, so they can run alongside this one.

## Troubleshooting

- **The VM doesn't come up.** `harness.py console` follows its serial console,
  also in `.vm/acl-container/serial.log`.
- **Bootstrap fails.** `journalctl -u aks-flex-node-bootstrap -b` on the VM
  shows the pull, `bootstrap.sh`, preflight, and `start`. The unit retries until
  bootstrap succeeds, unless retrying can't fix the failure: then it fails, with
  status 78, and `up` prints its log.
- **The agent's container doesn't start.** `journalctl -u aks-flex-node-agent -b`
  on the VM shows the entrypoint's checks. A failed check exits the container,
  and systemd retries it.
- **`machinectl` says `Access denied`.** That's SELinux: the image's policy
  lacks `allow spc_t kernel_t:dbus send_msg;`, which ACL 3.0.20261019 and
  later have, and `up` stops with that hint. List denials with
  `sudo journalctl -b | grep 'avc:  denied'` on the VM, and see
  [SELinux](DESIGN.md#selinux).
- **`up` fails exporting the image.** `az`'s sign-in has expired: run
  `az login`, or pass an exported image with `--image`.
- **The VM can't pull images.** The VM reaches the internet through NAT on
  this host, which needs IPv4 forwarding on; `up` turns it on.

## Files

| File | |
|---|---|
| `harness.py` | The harness |
| `agent-container.bu` | Butane config that installs the agent: the settings, and the bootstrap and reset units |
| `image/Containerfile` | The agent image |
| `image/bin/flex-agent-entrypoint` | The containers' entrypoint: mounts the host's paths, checks the host's D-Bus, runs its command |
| `image/bin/aks-flex-node-bootstrap` | Runs `main`'s `bootstrap.sh` with bootstrap's inputs |
| `image/bin/host-exec` | Runs the host's tools: under `chroot /host`, or for mounts in the host's mount namespace |
| `../../pkg/daemon/assets/aks-flex-node-agent-container.service`, `aks-flex-node-agent-recovery-container.service` | The agent's unit and recovery unit, which `start` installs |
| `aclimage.py` | Gallery image export |
| `ukiboot.py` | Adds first-boot kernel arguments to the UKI's addon |
