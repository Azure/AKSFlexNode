# Azure Container Linux A/B update harness

`abupdate.py` tests Azure Container Linux (ACL) OS updates in a local KVM VM. It
installs a base build of ACL, then drives Trident A/B updates to a later build
and back, and checks the VM after every step: the slot it boots, the image it
runs, Trident's status, the UEFI boot order, and that files under `/etc`, `/var`,
`/opt`, and `/home` survive.

It has two modes:

- **Plain**, `up`: ACL alone. Needs no root.
- **Cluster**, `up --cluster`: also makes a local kind cluster and joins the VM
  to it as an AKS Flex Node, with the agent in containers. Every update then
  drains the node and checks the node and the agent come back. Updates can also
  be driven the way AKS will, through trident-acl-agent's Node annotations. Needs
  `sudo`; see [Cluster mode](#cluster-mode).

You can run whole scenarios, run each step by hand, and save and restore the VM,
memory included, at any point. You can also build an update of your own, an ACL
build with your files or an Image Customizer config added, and update the VM to
it; see [Custom builds](#custom-builds). It's a tool for answering how ACL Flex
nodes take OS updates; see
[OS updates](../../docs/usage/azure-container-linux.md#os-updates).

## Requirements

- Linux on x86-64, with KVM (`/dev/kvm` readable and writable), and about
  6 GiB of memory and 10 GiB of disk to spare
- `qemu-system-x86_64`, `qemu-img`, OVMF firmware (the `ovmf` or `edk2-ovmf`
  package), `ssh`, and `setsid`; and `xorriso`, `genisoimage`, `mkisofs`, or
  `bsdtar`, for the seed ISO
- ACL images, from an OCI registry or as local files; see
  [The images](#the-images). For a registry in Azure Container Registry, `az`,
  logged in with permission to pull. The harness downloads ORAS if it isn't
  installed.
- For cluster mode, also `podman`, `kind`, `kubectl`, `go`, and `sudo` without
  a password prompt
- For custom builds, `podman`, `zstd`, mtools, and `sudo` without a password
  prompt

## Quick start

Say where the images are; there are no defaults:

```bash
export ACL_AB_REGISTRY=<registry>.azurecr.io/<namespace>
export ACL_AB_ISO_REPO=<installer ISO repository>
export ACL_AB_COSI_REPO=<COSI repository>
export ACL_AB_BASE_TAG=<build to install>
export ACL_AB_UPDATE_TAG=<later build to update to>
```

Then:

```bash
hack/acl-abupdate/abupdate.py up            # or `up --cluster`
hack/acl-abupdate/abupdate.py status
hack/acl-abupdate/abupdate.py update        # one A/B update, checked
hack/acl-abupdate/abupdate.py test          # every scenario, each from a fresh snapshot
hack/acl-abupdate/abupdate.py ssh           # a shell on the VM
hack/acl-abupdate/abupdate.py down          # remove the VM, and the cluster
```

`up` remembers the settings, so later commands don't need them. The first `up`
downloads the images and runs the installer, which takes a few minutes; later
ones start from the installed base it keeps, in about a minute
(`--fresh-install` installs again). A change to the Ignition seed, such as to
[the trident-acl-agent drop-in](#trident-acl-agent), also installs again, since
the image reads the seed on its first boot only.

How it works: the installer ISO streams the base image to the VM's disk, and an
Ignition config on a volume labelled `ACLSEED` provisions SSH access on its
first boot. An update copies a later build's COSI into the VM and runs
`trident update` with an image-only Host Configuration, which writes it to the
inactive A/B slot, then boots that slot and commits it. The VM uses QEMU's user
networking, with SSH forwarded from `127.0.0.1`. Only the download uses Azure.

## Commands

Run `abupdate.py COMMAND --help` for each command's options. With `--json`
before the command, the result is printed as JSON on stdout and the log goes to
stderr.

### Images

| Command | |
|---|---|
| `image fetch [TAG...]` | Pulls the base tag's installer ISO and the base and update COSIs, or the COSIs of the tags given. `--iso` also pulls the given tags' ISOs, and `--refresh` pulls again what's cached. |
| `image list` | Lists the cached images, builds, and installed bases: each one's build or source, OS version, `/usr` verity root hash, and `metadata.json` sha384. |
| `image build NAME` | Builds a COSI from an existing one and your files or Image Customizer config; see [Custom builds](#custom-builds). |
| `image delete NAME` | Deletes a build, or a tag's cached images. |

`up` and the update commands pull anything they need that isn't cached, so
`image fetch` is only needed to download ahead of time.

### The VM

| Command | |
|---|---|
| `up` | Installs the base image in a new VM, or starts from the base installed earlier. Then it runs check B1 and saves snapshot `base`. Options: `--base-tag`, `--update-tag`, `--fresh-install`, `--memory`, `--cpus`, `--ssh-port`. |
| `start [--snapshot NAME]` | Starts the stopped VM, from a snapshot if one is given. |
| `stop [--hard]` | Powers the VM off. `--hard` stops QEMU at once, as pulling the power would. |
| `down` | Stops the VM and removes the state directory. The cache stays. |
| `status` | Shows the booted image and slot, Trident's `servicingState`, `abActiveVolume`, and `lastError`, the UEFI `BootCurrent`, `BootNext`, and `BootOrder`, `trident-acl-agent.service`'s state, and any update that's staged or committed. |
| `ssh [COMMAND]` | Opens a shell on the VM, or runs a command there. |
| `console [-f]` | Shows the VM's serial console log, and with `-f` keeps following it. |

### Snapshots

| Command | |
|---|---|
| `snapshot save NAME [--force]` | Saves the running VM, memory included. |
| `snapshot restore NAME` | Returns the VM to a snapshot in seconds, without a reboot. If the VM is stopped, it starts from the snapshot. |
| `snapshot list` | Lists the snapshots. |
| `snapshot delete NAME` | Deletes a snapshot. |

A snapshot saves both the disk and the UEFI variables. BootNext and BootOrder
are part of what an update changes, so a snapshot saved mid-update restores to
the same point in it. The harness's own state is saved with each snapshot, and
restored with it: the marker files it wrote, the staged update, and the slot
devices it has seen. Snapshots are kept in the VM's disk overlay, so `down`
removes them.

### Steps

Each step runs its checks, and on failure collects diagnostics into its run
directory; see [Run artifacts](#run-artifacts).

| Command | Checks | |
|---|---|---|
| `update [--to TARGET]` | A1–A4 | Runs `stage`, `finalize`, and `commit`. `--no-commit` stops once the new slot has booted. `--failing-health-check` runs the health-check rollback instead (H1). |
| `stage [--to TARGET]` | A1 | Writes the image to the inactive slot without rebooting. |
| `finalize` | A2, A3 | Finalizes what's staged. Trident sets BootNext and reboots into the new slot. |
| `commit` | A4 | Commits the boot into the new slot, unless a trident unit committed it at boot. |
| `rollback [--check]` | R1 | Runs `trident rollback --ab`. `--check` only reports what `trident rollback --check` says. |
| `reboot [--hard]` | | Reboots the VM, or with `--hard` resets it, and reports the slot it booted. |
| `verify` | V1 | Checks the VM's state without changing it. `--expect TARGET`, `--expect-slot a` or `b`, and `--expect-state STATE` add expectations. |

`--to` defaults to `other`. With Trident versions that have
`--allowed-operations`, staging and finalizing run as separate
`trident update` calls. A failed stage is then told apart from a failed reboot,
and the VM can be snapshotted between the two. `--single-step` runs them as one
call, and the harness falls back to that when Trident lacks the
option.

The state is kept between commands, so an update can be run by hand:

```bash
hack/acl-abupdate/abupdate.py stage --to update
hack/acl-abupdate/abupdate.py snapshot save staged
hack/acl-abupdate/abupdate.py finalize
hack/acl-abupdate/abupdate.py status
hack/acl-abupdate/abupdate.py commit
hack/acl-abupdate/abupdate.py snapshot restore staged   # and finalize again
```

### Scenarios

| Command | |
|---|---|
| `scenarios` | Lists the scenarios. |
| `run SCENARIO... [--from SNAPSHOT]` | Runs scenarios in the order given, or `all` of them. `--from` restores the snapshot before each one, so the scenarios run independently; without it they run one after another on the running VM. `--rounds N` sets pingpong's rounds (default 2), and `--to` sets the scenarios' target. `--keep-going` runs the rest after a scenario fails. |
| `test` | Same as `run all --from base --keep-going`. `--stop-on-failure` stops at the first failure. |

A scenario that needs the VM in a particular state first runs steps to get it
there, and those steps are labelled `setup`. With `--no-setup`, the scenario
fails if the VM isn't already in that state. A scenario fails if any of its
checks fails. It's skipped if the image can't run it, for example when its
Trident has no `rollback` command, or if it needs a cluster and the VM has none.

| Scenario | | Checks |
|---|---|---|
| `forward` | Updates to `--to` (default `update`): stage, boot the new slot, commit. | A1–A4 |
| `reboot-after-commit` | Reboots after a committed update; the VM must stay on the new slot. | A5 |
| `manual-rollback` | `trident rollback --ab` after a committed update. | R1 |
| `pingpong` | Updates back and forth between the base and update images, `--rounds` times. | P*n*/A1–A4 |
| `health-rollback` | An update with a health check that fails; commit must roll back. Experimental. | H1 |
| `annotation-update` | Cluster mode: an update driven by Node annotations through trident-acl-agent, with the contract's rules for a staged update checked between stage and finalize. Experimental. | N0, N1, N5, N4, N2, A2–A4 |
| `annotation-rollback` | Cluster mode: a rollback driven by annotation. Experimental. | N3, R1 |
| `annotation-rejects` | Cluster mode: requests trident-acl-agent must refuse. Experimental. | N0, N4 |
| `annotation-heartbeat` | Cluster mode: an update by annotation whose download is slowed, so the agent's heartbeat shows. Experimental. | N0, N1, N6, N2, A2–A4 |
| `annotation-target-boot-failed` | Cluster mode: an update by annotation whose reboot lands on the old slot, then a new update. Experimental. | N0, N1, N7, retry/N1–N2 |

## Targets

A `TARGET` is one of:

- `base` or `update`: the tags `up` was given
- `other`: whichever of those two the VM doesn't run
- a build's name, from [`image build`](#custom-builds)
- a build tag in `ACL_AB_COSI_REPO`, pulled if it isn't cached
- a path to a `.cosi` file

The harness identifies the booted image by comparing `/usr`'s dm-verity root
hash with the COSIs it knows.

## Custom builds

`image build` makes an update of your own: an existing build of ACL with your
files, or an [Image Customizer](https://github.com/microsoft/azure-linux-image-tools)
config, added. It's a COSI that the VM can A/B-update to like any other target,
so you can try a change to the OS image without an image pipeline. To try it:

```bash
mkdir -p my-build/usr/bin my-build/etc/hello
printf '#!/bin/sh\necho "hello from my build: $(cat /etc/hello/hello.conf)"\n' > my-build/usr/bin/hello
chmod 755 my-build/usr/bin/hello
echo greeting=hi > my-build/etc/hello/hello.conf

hack/acl-abupdate/abupdate.py image build hello --files my-build   # about a minute
hack/acl-abupdate/abupdate.py update --to hello                    # the usual checks, A1-A4
hack/acl-abupdate/abupdate.py ssh -- hello
hack/acl-abupdate/abupdate.py update --to base                     # and back
```

Building needs no VM. Once built, every command and scenario that takes a
target takes the build's name, as in `run annotation-update --from base --to
hello` in cluster mode.

| Option | |
|---|---|
| `--files DIR` | Files to add, laid out like the root: `usr/...` goes in `/usr`, and `etc/...` in `/etc`. Each keeps its mode and is owned by root, and new `/etc` files get their SELinux label from the image's policy. Other top-level directories, symlinks, and special files are refused, with the reason: `opt/`, `var/`, and `home/`, for example, are on the root partition, which both slots share and an update doesn't write. |
| `--config FILE` | An [Image Customizer config](https://github.com/microsoft/azure-linux-image-tools/blob/v1.7.0/docs/imagecustomizer/api/configuration/configuration.md) to apply too, for scripts, services, users, and more files. Paths in it are relative to its directory. |
| `--from TARGET` | The image to build on: `base` (the default), `update`, a tag, another build, or a `.cosi` file. |
| `--version VERSION` | The semver version the update server offers the build as in annotation-driven scenarios. By default, its source's, with `-NAME.N` for the Nth build of NAME. |
| `--keep-work` | Keeps the work directory, with its raw disks, to look inside. |

Give at least one of `--files` and `--config`. A config like this adds a
systemd service and enables it, and runs a script in the image:

```yaml
os:
  additionalFiles:
  - source: files/hello-world.service
    destination: /usr/lib/systemd/system/hello-world.service
  services:
    enable:
    - hello-world
scripts:
  postCustomization:
  - name: stamp
    content: |
      echo "stamped by the world build" > /usr/share/hello-stamp
```

Building a name again replaces the build. `image list` lists the builds, and
`image delete NAME` removes one. Each is kept in the cache's `builds/NAME/`,
with `info.json` (what it was built from and with), the config the harness gave
Image Customizer, and Image Customizer's log.

### How a build works

Image Customizer v1.7.0 does the customizing, in its released container, which
the harness runs with `sudo podman`, privileged, for loop devices. Its support
for ACL is a preview: it adds the files, folds changes to `/etc` into `/etc`'s
baseline in `/usr/share/distro/etc`, and re-seals `/usr`'s dm-verity. It doesn't
know ACL's A/B layout, so the harness does the rest:

1. It expands the source COSI into a sparse raw disk, since Image Customizer
   takes a disk, and gives `/usr` a new btrfs filesystem UUID, so the two slots
   don't carry the same filesystem.
2. Of Image Customizer's output, it keeps `/usr` and its hash partition, and
   checks the new root hash by recomputing the hash tree.
3. It keeps the source's boot partition, and patches the new root hash into the
   UKI addons that carry `usrhash=`: both slots' templates in
   `/acl/uki-addons`, and the UKI's own. Image Customizer would give the update
   slot an addon with slot A's arguments.
4. It repacks the source COSI with those three partitions replaced and its
   `metadata.json` updated.

The limits that follow:

- **Secure Boot:** the patched addons aren't signed, so a build boots only with
  Secure Boot off, as in the harness's VM.
- **The boot partition** is the source's, so a config can't change the kernel
  command line, the SELinux mode, the kernel, or the initramfs. The harness
  compares Image Customizer's boot partition with the source's, and stops if it
  changed more than the verity arguments.
- **Packages:** Image Customizer installs packages only on images that have
  `tdnf`, which stock ACL doesn't.
- **`/etc`:** an update changes only the overlay's lower layer, so a file the
  node changed or removed in `/etc` keeps the node's version. Files a config
  adds to `/etc`, such as a service's enablement link, aren't given SELinux
  labels, as `--files`' are; a script in the config can run `setfiles` on them.
- **Space:** `/usr` has about 1.5 GiB free. A build needs about 10 GiB free in
  the cache while it runs.

The Image Customizer container is pinned by digest; `ACL_AB_IMAGE_CUSTOMIZER`
names another, such as a later release.

## What the checks cover

| ID | Where | Checks |
|---|---|---|
| B1 | `up` | `/usr` is a read-only dm-verity device on the VM's disk, not the installer's. A root hash that differs from the base COSI's is only a warning, since the ISO may embed another build. |
| A1 | stage | The COSI's `metadata.json` sha384 in the VM is the expected one. Staging doesn't reboot the VM. `servicingState` is `ab-update-staged`. The slot and `abActiveVolume` are unchanged, and there's no `lastError`. |
| A2 | finalize | The VM booted the other slot, and `/usr`'s root hash is the target's. |
| A3 | finalize | Every marker the harness wrote under `/etc`, `/var/lib`, `/opt`, and `/home/core` is still there, and `/usr` is read-only. |
| A4 | commit | `servicingState` is `provisioned` with no `lastError`. `abActiveVolume` flipped and names the slot the VM booted. The Host Status names the update's image URL and sha384, and `BootCurrent` is first in `BootOrder`. |
| A5 | reboot-after-commit | A plain reboot comes back on the same slot and image, still `provisioned`. This shows commit changed BootOrder, not just the one-time BootNext. |
| R1 | rollback | `trident rollback --check` reports `ab`. Afterwards the VM runs the other slot and the image that slot held, `provisioned`, with `abActiveVolume` flipped back, no `lastError`, and the markers kept. |
| H1 | health-rollback | The config adds a health check that always fails. Commit runs it, and Trident reboots into the previous slot and ends `provisioned` there, on the previous image. `lastError` reports the check, and a failure log is in `/var/lib/trident`. If the journal keeps earlier boots, there were two boots since the update. |
| V1 | verify | The expectations given, the slot `abActiveVolume` names, `BootCurrent` (when `provisioned`), and the markers. |
| T1 | `up`, and after every boot the harness causes | `trident-acl-agent.service` is installed, enabled, and started this boot. The harness's drop-in is loaded, every setting it pins is in the unit's environment, and this boot's preflight checked the kubeconfig the drop-in names. See [trident-acl-agent](#trident-acl-agent). |

`provisioned` alone doesn't show an update worked: a failed
update also ends there, with `lastError` set and the active volume unchanged.
So A4 and R1 also check the slot, the root hash, and `lastError`.

The slot is identified by `/usr`'s verity data and hash devices, from
`veritysetup status usr`; `findmnt /usr` shows `/dev/dm-0` for either slot.
The harness learns which devices belong to `volume-a` and `volume-b` from the
first `provisioned` status it reads, and checks that Trident's later reports
agree with it.

Each update writes a new token to every marker file, and A3 checks that all the
tokens written so far are there. A missing token means that directory doesn't
survive an A/B update. If every token survives, the agent's state in those
directories would survive too.

## trident-acl-agent

`trident-acl-agent.service`, from Trident's `trident-acl` package, performs A/B
updates that Kubernetes Node annotations request. The seed enables it, with the
drop-in [`trident-acl-agent-override.conf`](trident-acl-agent-override.conf)
as `/etc/systemd/system/trident-acl-agent.service.d/trident-acl-agent-override.conf`.
Ignition writes the drop-in before systemd starts any unit, so the agent never
runs without it. The drop-in pins every setting the agent reads, including the
`acl.azure.com` annotation prefix and tridentd's socket. It also restarts the
agent 5 seconds after it exits, and logs a preflight line for each path the agent
needs.

The drop-in points the agent's kubeconfig at
`/etc/aks-flex-node/trident-acl-agent.kubeconfig`, not the AKS contract's
`/var/lib/kubelet/kubeconfig`. On a flex node, kubelet runs in an nspawn
machine, so the host has no kubelet kubeconfig. The flex agent owns
`/etc/aks-flex-node`, and reset removes it. In plain mode there's no cluster and
nothing writes that kubeconfig, so the agent can't watch any annotations: it
restarts every 5 seconds, and its preflight logs the kubeconfig as `MISSING`.
It doesn't affect the updates the harness runs. In cluster mode, the harness
writes it, standing in for the flex agent; see [Cluster mode](#cluster-mode).

T1 checks the agent after `up` and after every boot the harness causes. That
includes each update's boot into the new slot, so it also shows the enablement
and drop-in under `/etc` survive A/B updates. `status` shows the agent's state,
and the diagnostics of a failed step include its unit and journal.

The seed's digest is kept with the installed base. When the seed changes, as it
does when the drop-in is edited, `up` installs the base again, since the image
reads the seed on its first boot only.

## Cluster mode

```bash
sudo -v   # if your sudo caches credentials
hack/acl-abupdate/abupdate.py up --cluster
hack/acl-abupdate/abupdate.py kubectl get nodes -o wide
hack/acl-abupdate/abupdate.py test
hack/acl-abupdate/abupdate.py down
```

`up --cluster` needs `sudo` without a password prompt, for kind on root Podman
and the cluster's bridge; `podman`, `kind`, `kubectl`, and `go`; and
`fs.inotify.max_user_instances` of at least 512, which kind needs: run
`sudo sysctl fs.inotify.max_user_instances=512`. Plain `up` needs none of that.

It does what plain `up` does, and:

1. While the base installs, it creates the kind cluster `aclab` on root Podman,
   and builds and deploys the AKS Flex controller from this repository.
2. It gives the VM a second NIC, on a TAP device in the bridge `virbr-aclab`
   (`192.168.113.0/24`), which the kind node joins by a veth. The VM is
   `192.168.113.10`, and the API server `192.168.113.2:6443`. The cluster can
   reach the kubelet, for `kubectl logs` and `exec`, and route pods to the
   node. SSH, the internet, and the host's services stay on the first NIC,
   QEMU's user networking, so the host needs no NAT. The seed claims the second
   NIC on the VM's first boot, without DHCP, so that boot isn't held up waiting
   for a lease.
3. It saves snapshot `prejoin`: on the bridge, not joined.
4. It joins the VM as node `acl-abupdate`, the hostname the seed sets, with the
   agent in containers:
   - It builds the agent's image from `Dockerfile.aks-flex-node-agent` with
     rootless Podman and pushes it to the local registry `aclab-registry`, which
     only listens on `127.0.0.1:5097`. The VM pulls it by digest from
     `10.0.2.2:5097`, through its user NIC.
   - It renders [`scripts/aks-flex-node-agent-container.bu`](../../scripts/aks-flex-node-agent-container.bu)
     with a bootstrap token and applies its files and units over SSH, then
     starts bootstrap. They aren't in the seed: the token changes each time,
     and the seed would have to be installed again for each.
5. It writes trident-acl-agent's kubeconfig, with the kubelet's identity, and
   starts a probe Deployment pinned to the node.
6. It saves snapshot `base`: joined, Ready, and running the probe.

Every reboot the harness causes is then wrapped:

- before it: the agent's state is recorded, and the node cordoned and drained,
  as [OS updates](../../docs/usage/azure-container-linux.md#os-updates) says to
  before one. `--no-drain` skips the drain.
- after it: the node, the agent, and trident-acl-agent are checked (K2, K4, K5,
  T2).
- once the update is committed or rolled back: the node is uncordoned and the
  probe runs on it again (D2, K3).

Restoring a snapshot first sets the cluster's side back to what the snapshot
recorded, while the VM is paused: trident-acl-agent's annotations, the cordon,
the harness's taints and pods. A snapshot taken before the join gets no Node. It
then steps the VM's clock to the host's, since the restored clock is behind and
the node's Lease renewals would otherwise go back in time (S1), and restarts
trident-acl-agent: its watch on the Node is a connection the API server closed
while the snapshot waited, and the restored agent would otherwise wait on it
without seeing new requests. A snapshot only restores into the kind cluster it
was saved in.

The image runs no NTP client, so the harness also steps the VM's clock after
every boot it waits for: a boot after a restore starts from a clock that's
behind.

| ID | Where | Checks |
|---|---|---|
| K1 | `up`, `cluster rejoin` | Bootstrap succeeded and removed its inputs; the agent's unit is active and its container runs as `spc_t`; the active machine runs; the node is Ready at `192.168.113.10` with the cluster's kubelet version; the Node's `systemUUID`, which kubelet reads inside its nspawn machine, is the VM's `product_uuid`. |
| K2 | after each boot | The node is Ready, with a Lease renewed this boot; the agent's container and machine run. |
| K3 | `up`, after uncordoning | The probe pod runs on the node, and `kubectl logs` and `exec` reach its kubelet. |
| K4 | after each update's boot | The agent's state is as it was before the reboot: its files under `/etc`, its units and their enablement, its image in containerd, and the same machine. |
| K5 | after each boot | No AVC denial this boot names the agent, other than known ones. |
| D1, D2 | around each reboot | The node is cordoned and drained, then schedulable again. |
| S1 | each restore | The VM's clock is the host's; the node is Ready with a Lease renewed since the restore; its annotations, cordon, and taints are as recorded. |
| T2 | `up`, after each boot | trident-acl-agent's kubeconfig exists, mode 0600; `trident-acl-agent --validate-connection kubernetes` passes; the unit stays running. |
| N0 | annotation scenarios | `trident-acl-agent --validate-connection nebraska` reaches the update server over TLS. |
| N1 | annotation scenarios | A stage request ends `Success`, without a reboot, with Trident `ab-update-staged`, and tridentd downloaded the COSI from the update server. |
| N2 | annotation-update, annotation-heartbeat | A finalize request ends `Success`; the agent reboots the VM and commits the new slot (`update-commit-status` `Success`); it reports the update done (event 3/2) to the update server after the boot. A2–A4 and the node's checks follow, as for an update the harness drives. |
| N3 | annotation-rollback | A rollback request ends `Success`, and its commit too; R1 follows. |
| N4 | annotation-rejects, annotation-update | Requests the contract says to refuse get the code it gives, and none of them reboots the VM or changes Trident's state: `InvalidRequest` for a plain-HTTP server, another `schemaVersion`, no `track`, an `operationId` that isn't a UUID, a version the server doesn't offer, a rollback naming a target, and a finalize for another version than was staged; `OperationFailed` for a track the server has nothing on; `NotStaged` for a finalize with nothing staged. Requests the agent can't parse don't act either; see below. |
| N5 | annotation-update | The agent keys work by `operationId`: re-sending the stage request, written differently, leaves its status as it was, without reaching the update server or Trident again, and removing the status while the request stays makes the agent write it back unchanged. |
| N6 | annotation-heartbeat | While the stage is `InProgress`, `lastUpdatedUtc` refreshes at least twice, each within 1.5 times the pinned heartbeat interval; `startedUtc` stays; `finishedUtc` is absent until the terminal status. |
| N7 | annotation-target-boot-failed | The reset stopped the new slot's kernel; the commit after the boot that followed reports `TargetBootFailed`, with `tridentError` subkind `ab-update-reboot-check`; the VM runs its old image; Trident's Host Status names the slot the VM runs and not the update's image; then a new update, with a new `nodeUpdateId`, succeeds (retry/N1, retry/N2). The retry is skipped when the Host Status doesn't name the running slot. |

### Annotation-driven updates

The annotation scenarios drive an update the way AKS will, following
[the annotation contract](https://microsoft.github.io/trident/docs/Explanation/Trident-ACL-Agent#the-annotation-contract):
the harness writes `acl.azure.com/update-request` on the Node, and
trident-acl-agent stages, finalizes, or rolls back through tridentd, and writes
`update-status` and `update-commit-status`. For the image, the request names an
update server: the harness's own, [`omaha.py`](omaha.py), which speaks the Omaha
protocol over HTTPS as Nebraska does, and serves the COSIs too. Like Nebraska, it
offers a package per application and track: the requests' `appId` is a fixed
UUID, and their `track` is `pin-<targetVersion>`, as in the contract's example.
It listens on `127.0.0.1:8297`, which the VM reaches as `10.0.2.2:8297`, only
while a scenario runs. Its CA is made once, under the state directory, and
`up --cluster` adds it to the VM's trust store, as trident's own tests do. ACL
keeps it across A/B updates, which the agent needs: after the reboot, it reports
the update done to the server. What the server received, its requests and the
agent's events, is saved in each run's `update-server.json`.

Where the harness and the contract part:

- **Requests the agent can't parse.** The contract says malformed JSON gets
  `InvalidRequest`. trident-acl-agent v0.30.0 logs `ignoring malformed …
  annotation` and writes no status, since it can't tell which `operationId` to
  answer. That includes requests with a field the protocol doesn't have, and a
  `nodeUpdateId` that isn't a UUID, which fail to parse rather than fail
  validation. `annotation-rejects` checks that such requests don't act, and notes
  in the run's summary what the agent did, so a change on either side shows.
- **A second finalize or rollback while one waits for its commit** gets
  `InvalidRequest`, by the contract. It isn't checked: the agent only holds a
  commit pending between writing the finalize's status and the reboot it starts
  right after, and after the boot it finishes the commit before it reads any new
  request, so no request from outside can reach that window.
- **`TargetBootFailed`** is caused by resetting the VM through QEMU as the new
  slot's kernel starts, which the serial console shows. The firmware has used
  its one-time `BootNext` by then, so it boots the old slot, as after a failed
  first boot. If the firmware boots the new slot anyway, the scenario is
  skipped. **On ACL with Trident 0.30.0 the scenario fails:** after the fallback,
  `trident commit` reports the update succeeded, Trident's Host Status names the
  new slot and image, and the agent writes `update-commit-status` `Success` and
  reports the update done to the update server, while the VM runs the old slot.
  Trident checks the boot by comparing the root filesystem's device, and on
  ACL's layout only `/usr` is A/B: root is one partition both slots share, so
  the check passes whichever slot booted. Because a later update would take the
  running slot for the inactive one, the scenario then doesn't try one, and
  leaves the VM as it is; restore a snapshot before updating it again.

The VM has an SMBIOS UUID, as an Azure VM has, so K1 can check that the Node's
`systemUUID`, which kubelet reads inside its nspawn machine, is the host's
`product_uuid`: that's what trident-acl-agent's
`TRIDENT_ACL_AGENT_KUBERNETES_VALIDATE_NODE_UUID` compares, which the drop-in
leaves off until that holds wherever flex runs.

Other commands in cluster mode:

| Command | |
|---|---|
| `kubectl ARGS...` | kubectl against the cluster. `cluster env` prints the `KUBECONFIG` export instead. |
| `cluster status` | The nodes, and the pods on the VM's node. |
| `cluster rejoin [--from-prejoin]` | Resets the node with the agent's reset unit, or restores `prejoin`, and joins again with a new token. |
| `cluster push` | Rebuilds the agent's image from this repository and switches the node to it, through `/etc/aks-flex-node/agent-image.env`, as an AgentUpgrade does. |
| `agent [CMD]` | A shell in the agent's container on the VM, or a command there. |

`stop` stops the VM only; the node goes NotReady. `start` repairs the bridge,
TAP device, and kind node after a host restart before it boots the VM. `down`
removes the cluster, its network, and the local registry too. If any of them
can't be removed, it says what to remove by hand and keeps the state, unless
`--force`.

## Adding a scenario

Scenarios are in [`scenarios.py`](scenarios.py). Each is a function of the run's
context, registered with `@scenario`:

```python
@scenario("restage", "Stage the update twice before finalizing; the second staging replaces the first",
          needs=Needs.OFF_TARGET, checks=("A1", "A2", "A3", "A4"))
def restage(ctx: Context) -> None:
    ctx.stage(ctx.target("update"))
    ctx.update(ctx.target("update"))
```

The context's steps are the step commands' own: `stage`, `finalize`, `commit`,
`update`, `reboot`, `rollback`, `health_rollback`, and `verify`. They return
probes of the VM's state, and `ctx.checks.expect(ok, ID, description, detail)`
adds checks of your own. `ctx.scope(name)` prefixes the IDs of the checks run
inside it, as pingpong's `P1/A1` shows. To skip the scenario, raise `Skip`; to
stop it as failed, raise `StepError`.

`needs` declares what the scenario expects of the VM: `Needs.NONE`,
`Needs.OFF_TARGET` (the VM doesn't run the target yet), or
`Needs.COMMITTED_UPDATE`. Hard resets go through QEMU's control socket, as in
`ctx.reboot(hard=True)`, which is enough for power-loss scenarios.

## The images

The harness needs a KVM installer ISO of the base build, and COSIs of the base
and update builds, all for amd64. Don't mix in images for other hypervisors, or
Azure VHDs. In a registry, each is an ORAS artifact whose one layer is the file,
tagged with its build: the ISO in `ACL_AB_ISO_REPO`, the COSIs in
`ACL_AB_COSI_REPO`, both under `ACL_AB_REGISTRY`. The base tag's COSI is pulled
too, to update back to the base. `oras repo tags <registry>/<repository>` lists
the published builds.

For Azure Container Registry, the token comes from `az acr login
--expose-token`, in the subscription `ACL_AB_SUBSCRIPTION` names, or `az`'s
current one, and is passed to `oras login` on stdin. It's kept in the cache's
own registry config, not in `~/.docker/config.json`, and is renewed after two
hours, or when the registry refuses it. `unauthorized` after a login means the
identity can't pull from the repository.

Local files can stand in for the downloads: `ACL_AB_BASE_ISO`,
`ACL_AB_BASE_COSI`, and `ACL_AB_UPDATE_COSI`. With all three, no registry is
needed; the tags still name the builds.

The image's `sha384` in the Host Configuration is the SHA-384 of the COSI's
`metadata.json`, not of the whole file, and is computed for each target.

## Settings

| Variable | Default | |
|---|---|---|
| `ACL_AB_BASE_TAG`, `ACL_AB_UPDATE_TAG` | none | The builds `up` installs and updates to; `up --base-tag` and `--update-tag` too |
| `ACL_AB_REGISTRY` | none | The images' registry and namespace, such as `example.azurecr.io/acl` |
| `ACL_AB_ISO_REPO`, `ACL_AB_COSI_REPO` | none | The installer ISO's and the COSIs' repositories under it |
| `ACL_AB_BASE_ISO`, `ACL_AB_BASE_COSI`, `ACL_AB_UPDATE_COSI` | | Local files to use instead of the downloads |
| `ACL_AB_SUBSCRIPTION` | `az`'s current one | The registry's subscription, for `az acr login` |
| `ACL_AB_VM_MEMORY`, `ACL_AB_VM_CPUS` | `6144`, `2` | VM size, and `up`'s defaults |
| `ACL_AB_DISK_SIZE` | `64G` | Disk size for an install |
| `ACL_AB_SSH_PORT` | `2222` | Host port forwarded to the VM's SSH |
| `ACL_AB_GUEST_DIR` | `/var/tmp/acl-abupdate` | Where COSIs are copied in the VM; `/tmp` may be in memory |
| `ACL_AB_INSTALL_TIMEOUT`, `ACL_AB_BOOT_TIMEOUT`, `ACL_AB_STAGE_TIMEOUT` | `2400`, `900`, `3600` | Seconds |
| `ACL_AB_STATE_DIR` | `.vm/acl-abupdate` | VM disk overlay, UEFI variables, SSH key, snapshots, and run artifacts |
| `ACL_AB_CACHE_DIR` | `.vm/acl-abupdate-cache` | Downloads, builds, installed bases, and ORAS; `down` keeps it |
| `ACL_AB_IMAGE_CUSTOMIZER` | v1.7.0, by digest | The Image Customizer container image `image build` runs |
| `ACL_AB_KIND_CLUSTER` | `aclab` | Cluster mode's kind cluster |
| `ACL_AB_KIND_NODE_IMAGE` | kind's default | Its node image, for another Kubernetes version |
| `ACL_AB_CLUSTER_SUBNET` | `192.168.113` | First three octets of the cluster's bridge network |
| `ACL_AB_LOCAL_REGISTRY_PORT` | `5097` | Port of the local registry the VM pulls the agent's image from |
| `ACL_AB_OMAHA_PORT` | `8297` | Port of the update server for annotation-driven updates |
| `ACL_AB_JOIN_TIMEOUT`, `ACL_AB_READY_TIMEOUT` | `1200`, `300` | Seconds to join, and for the node to be Ready again after a boot |

## Run artifacts

Each command that runs checks writes a directory under
`.vm/acl-abupdate/runs/`. It holds:

- numbered snapshots of the VM's state before and after each step, with the raw
  `veritysetup`, Trident Host Status, and UEFI variables
- each Trident command's output, and the Host Configuration it was given
- `summary.json`, with every check, scenario result, and timing: copy, stage,
  finalize to SSH, and commit

When a step or scenario fails, `diagnostics-NAME/` has the current and previous
boots' journals, `trident get status` and `last-error`, the trident units,
`trident-acl-agent.service`'s unit and journal, `lsblk`, `efibootmgr -v`, a
`trident diagnose` bundle, and the end of the serial log.

## Troubleshooting

- **The install:** `console -f` follows it. `WAITING FOR CONFIGURATION MEDIA`
  means the installed image didn't find the `ACLSEED` volume; `up` gives up
  after five minutes of it. On the installer's console, Ctrl-C then `s` opens a
  diagnostic shell, which `console` can't drive. Use it only to look around,
  and don't run updates from it.
- **The installer and the disk:** the installer overwrites the smallest disk
  that fits the image. The harness's VM has only its own disk attached. After
  the install, the installer ISO and seed are detached, so a disk that stops
  booting can't fall through to the installer and be reinstalled, which would
  look like a rollback.
- **Trident's datastore:** a freshly installed image has no Host Status until
  its first update, so `status` shows none, and `trident get status` fails.
- **Tags:** a build tag isn't the OS's `VERSION_ID`. Two builds can carry the
  same OS release, so an unchanged `/etc/os-release` doesn't mean an update
  failed; the root hash shows which image runs.
- **Disk space in the VM:** COSIs are kept in `ACL_AB_GUEST_DIR` while there's
  room for the next one, so pingpong copies each only once.
