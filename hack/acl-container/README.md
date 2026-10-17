# Agent container prototype on Azure Container Linux

This harness proves out running the AKS Flex Node agent as a privileged
containerd container on Azure Container Linux (ACL), instead of installing it on
the host as [PR #323](https://github.com/Azure/AKSFlexNode/pull/323) does. It
boots ACL in a local VM from an Ignition config rendered from
[`agent-container.bu`](agent-container.bu). That config starts the agent
image with the host's containerd, with the host's root mounted at `/host`.
From inside that container, the host's `machinectl` creates, starts, stops,
and opens shells in the systemd-nspawn machines the agent runs Kubernetes in.

[DESIGN.md](DESIGN.md) explains how it works, what it proved, and what is
left before the real agent can run this way. This file is how to use it.

The agent binary is in the image, but it does not run yet: the container's
entrypoint checks its access to the host and idles, and `flex-machine` stands
in for the agent's machine management. No Kubernetes cluster is involved.

## Requirements

- Linux with KVM: `/dev/kvm` readable and writable
- `qemu-system-x86_64`, `qemu-img`, `qemu-nbd`, and OVMF firmware (the `ovmf` or
  `edk2-ovmf` package)
- `podman`, or `docker` with `ACLC_CONTAINER_ENGINE=docker`, to build the agent
  image and run the registry and Butane
- `ip`, `iptables`, `sysctl`, `ssh`, `ssh-keygen`, `setsid`
- `sudo` without a password prompt, for the bridge, TAP device, forwarding, and
  firewall rules. Run `sudo -v` first if your sudo caches credentials.
- An ACL image: a local qcow2, or `az`, logged in, to export the gallery image;
  see [The ACL image](#the-acl-image)

## Quick start

```bash
export ACL_IMAGE_RESOURCE_GROUP=my-westus2-group   # only for the first export
hack/acl-container/harness.py up       # build and push the image, boot the VM
hack/acl-container/harness.py status   # the VM, the agent container, the machines
hack/acl-container/harness.py check    # the feasibility checks, about 2 minutes, with a VM reboot
hack/acl-container/harness.py down     # remove the VM, network, registry, and state
```

`up` takes about three minutes once the ACL image is exported and the image
build is cached; the first build compiles the agent from scratch and takes
several more. It:

1. builds `image/Containerfile` from this repository with the agent and
   `crane` in it, and pushes it to a registry it runs on the bridge,
   `192.168.112.1:5099`
2. creates the bridge `virbr-aclc` on `192.168.112.0/24`, with NAT, and turns
   IPv4 forwarding on if it is off (`down` turns it off again)
3. renders `agent-container.bu` with Butane, adds an SSH user, a hostname, and a
   static address, and boots the VM at `192.168.112.10`
4. waits for `aks-flex-node-container.service` on the VM to pull the image and
   for the container to report `agent container ready`

`up --no-agent` boots plain ACL, without the agent container, to explore the
OS. `up` again replaces the VM with a fresh one.

## Working with machines

Machines can be managed from three places. All three use the same host
`machinectl` and see the same machines.

### From your machine

`harness.py machine` runs `flex-machine` in the agent container:

```bash
hack/acl-container/harness.py machine create demo --start   # pull the rootfs, boot it
hack/acl-container/harness.py machine list
hack/acl-container/harness.py machine shell demo            # root shell in the machine
hack/acl-container/harness.py machine exec demo cat /etc/os-release
hack/acl-container/harness.py machine stop demo
hack/acl-container/harness.py machine remove demo
```

| Command | What it does |
|---|---|
| `list` | `machinectl list` and `machinectl list-images` |
| `create NAME [--image REF] [--start]` | Unpacks an OCI rootfs image into `/var/lib/machines/NAME` and writes `/etc/systemd/nspawn/NAME.nspawn`. The default image, `ghcr.io/azure/agent-azlinux3:v20260619`, is the one the agent uses on ACL. |
| `start NAME` | `machinectl start`, then waits for it to run |
| `stop NAME` | `machinectl poweroff`, then waits for it to stop |
| `restart NAME` | `machinectl reboot` |
| `terminate NAME` | `machinectl terminate`: stops it at once |
| `status NAME` | `machinectl status` |
| `shell NAME [CMD...]` | `machinectl shell`: a root shell, or `CMD`, with an absolute path |
| `exec NAME CMD...` | `systemd-run --machine --pipe --wait`, as the agent runs commands in its machines |
| `logs NAME [-f]` | The host's log for `systemd-nspawn@NAME.service` |
| `enable NAME`, `disable NAME` | `machinectl enable` or `disable`: start it at boot, or not |
| `remove NAME` | Stops it, and deletes its directory and `.nspawn` file |

`FLEX_MACHINE_IMAGE` in the container changes the default image.

### From inside the agent container

```bash
hack/acl-container/harness.py agent          # a shell in the container
```

In the container, `machinectl`, `systemctl`, `systemd-run`, and `journalctl` run
the host's binaries through `host-exec`, which runs them under `chroot /host`.
Any other host command runs as `host-exec COMMAND`:

```bash
machinectl list
machinectl shell demo
systemctl status systemd-nspawn@demo
systemd-run --machine=demo --pipe --wait -- uname -a
host-exec ip -br addr
ls /host/var/lib/machines
```

`harness.py agent COMMAND...` runs one command there instead.

### From the VM host

```bash
hack/acl-container/harness.py ssh            # core@192.168.112.10
sudo machinectl list
sudo machinectl shell demo
sudo systemctl status systemd-nspawn@demo
```

A shell in a running machine is also available without machined:

```bash
sudo nsenter --target "$(machinectl show demo -p Leader --value)" --all /bin/bash
```

## The agent container

On the VM, the container is `aks-flex-node` in containerd's `aks-flex`
namespace, run by `aks-flex-node-container.service`:

```bash
systemctl status aks-flex-node-container
journalctl -u aks-flex-node-container -b        # the entrypoint's checks
sudo ctr --namespace aks-flex task ls
sudo systemctl restart aks-flex-node-container  # pulls the image again first
```

The image reference is in `/etc/aks-flex-node/agent-container.env`, written by
Ignition.

Each start of the container takes about 20 seconds before its entrypoint runs:
the unit runs it on containerd's native snapshotter, which copies the image for
every start, to keep it out of an SELinux domain that can't reach systemd. See
[SELinux](DESIGN.md#selinux).

To change what is in the image, edit it and push it:

```bash
hack/acl-container/harness.py push   # rebuild, push, restart the container
```

Machines keep running across `push`: they belong to the host's systemd, not to
the container.

## The ACL image

Without `--image` or `ACL_IMAGE`, `up` boots the Azure build of ACL from the
shared gallery image `ACL_IMAGE_GALLERY_IMAGE`, which it exports to
`.vm/acl-images/acl-gallery-<version>.qcow2` the first time. The export copies
the image to a temporary managed disk in
`ACL_IMAGE_RESOURCE_GROUP`, which has to be in `westus2`, where the image is
replicated, downloads the disk's 850 MiB of written pages, and deletes the disk.
It takes about 90 seconds on a fast link.

The identity `az` is logged in as needs, on the resource group,
`Microsoft.Compute/disks/read`, `write`, `delete`, `beginGetAccess/action` and
`endGetAccess/action`, and on the subscription, read access to the shared
gallery's images and versions. Its tenant has to be one the gallery is shared
with.

The gallery's latest version is asked of `az` on each `up`. Set
`ACL_IMAGE_VERSION` to boot an exported image without `az`, or pass `--image`.

## Settings

| Variable | Default | |
|---|---|---|
| `ACL_IMAGE` | | Local ACL qcow2, instead of `--image` |
| `ACL_IMAGE_RESOURCE_GROUP` | | Resource group in `westus2` the gallery image is exported through |
| `ACL_IMAGE_SUBSCRIPTION` | `az`'s default | Subscription of that resource group |
| `ACL_IMAGE_VERSION` | the gallery's latest | Gallery image version |
| `ACL_IMAGE_GALLERY_IMAGE` | `/SharedGalleries/b3e01d89-bd55-414f-bbb4-cdfeb2628caa-ACL/Images/acl-1es-eval` | Gallery image |
| `ACL_IMAGE_DIR` | `.vm/acl-images` | Where exported images are kept; `down` keeps them |
| `ACLC_STATE_DIR` | `.vm/acl-container` | VM disk, logs, SSH key, rendered Ignition |
| `ACLC_CONTAINER_ENGINE` | `podman` if installed, else `docker` | Builds the image, runs the registry and Butane |
| `ACLC_SUBNET` | `192.168.112` | First three octets of the bridge subnet |
| `ACLC_DNS` | `8.8.8.8 8.8.4.4` | The VM's DNS servers |
| `ACLC_REGISTRY_PORT` | `5099` | Registry port on the bridge and on loopback |
| `ACLC_SERVE_PORT` | `8298` | Port the Ignition config is served on during `up` |
| `ACLC_VM_MEMORY`, `ACLC_VM_CPUS` | `4096`, `4` | VM size |
| `ACLC_AGENT_TAG` | `proto` | Agent image tag |
| `ACLC_BUTANE_IMAGE` | `quay.io/coreos/butane:v0.29.0` | Butane image |

The subnet, interface names, and ports differ from PR #323's harness and the
unbounded one, so they can run alongside this one.

## Troubleshooting

- **The VM does not come up.** `harness.py console` follows its serial console,
  also in `.vm/acl-container/serial.log`.
- **The container does not start.** `journalctl -u aks-flex-node-container -b`
  on the VM shows the pull and the entrypoint's checks. A failed check exits the
  container, and systemd retries it every five seconds.
- **`machinectl` says `Access denied`.** That is SELinux. List denials with
  `sudo journalctl -b | grep 'avc:  denied'` on the VM, and see
  [SELinux](DESIGN.md#selinux).
- **The VM cannot pull images.** The VM reaches the internet through NAT on
  this host, which needs IPv4 forwarding on; `up` turns it on.

## Files

| File | |
|---|---|
| `harness.py` | The harness |
| `agent-container.bu` | Butane config for the agent container unit |
| `image/Containerfile` | The agent image |
| `image/bin/flex-agent-entrypoint` | The container's entrypoint |
| `image/bin/host-exec` | Runs host binaries under `chroot /host` |
| `image/bin/flex-machine` | Machine management |
| `aclimage.py` | Gallery image export |
| `ukiboot.py` | Adds first-boot kernel arguments to the UKI's addon |
| `SELINUX.md` | The SELinux finding, written up for the ACL team |
| `selinux-repro.sh` | Standalone reproduction of the SELinux finding, for any ACL host |
| `selinux-verify.sh` | Builds candidate SELinux policies from an ACL host's own, and runs `selinux-repro.sh` against them |
