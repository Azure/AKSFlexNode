# Azure Container Linux harness

`acl.py` boots an Azure Container Linux VM from the Ignition config that
[`scripts/aks-flex-node-bootstrap.bu`](../../scripts/aks-flex-node-bootstrap.bu)
renders, set up as
[Hosts provisioned by Ignition](../../docs/usage/getting-started.md#hosts-provisioned-by-ignition)
describes, and joins it to a local kind cluster. It exercises the path an
Ignition-provisioned host takes: the first-boot unit runs
`bootstrap.sh`, the agent is installed under `/opt/unbounded/agent`, which is
writable while `/usr` is not, and reset removes what bootstrap installed.

The node uses no Azure resources. It joins with a bootstrap token, and the
agent's machine client talks to an AKS Flex controller deployed in the kind
cluster, reached through the API server's service proxy. Only getting the
image, when no local one is given, uses Azure; see [Image](#image).

## Requirements

- Linux with KVM (`/dev/kvm` readable and writable)
- `docker`, `kind`, `kubectl`, `go`. Butane runs from its container image,
  `quay.io/coreos/butane`, which docker pulls on first use.
- `qemu-system-x86_64`, `qemu-img`, `qemu-nbd`, and OVMF firmware (the `ovmf`
  or `edk2-ovmf` package)
- `ip`, `iptables`, `nsenter`, `ssh`, `ssh-keygen`
- `sudo` without a password prompt, for the bridge, TAP device, and firewall
  rules. Run `sudo -v` first if your sudo caches credentials.
- An Azure Container Linux image: a local qcow2, or `az`, logged in, to export
  the Azure build from its gallery; see [Image](#image)

## Usage

```bash
ACL_IMAGE_RESOURCE_GROUP=my-westus2-group hack/acl/acl.py up
hack/acl/acl.py test
hack/acl/acl.py down
```

`up --image /path/to/acl.qcow2` boots a local image instead of the gallery's.

`up` builds the agent and controller, creates the kind cluster and a bridge on
`192.168.111.0/24`, deploys the controller, and renders the Ignition config from
the template, the repository's `bootstrap.sh`, a base config with a bootstrap
token, and a `bootstrap.env` with the agent URL and digest. It then boots the
VM. It returns once the first-boot unit has finished and the node is
Ready. The Ignition config and agent archive are served only while `up` runs, so
later boots cannot depend on them.

`test` checks that:

- the first-boot unit succeeded, removed `/etc/aks-flex-node/first-boot`, whose
  files carry the bootstrap token, and installed the agent under `/opt/unbounded/agent`, in an
  `/opt/unbounded` created 0755, and nothing under `/usr/local`
- a setting containing `$`, `%`, quotes, and backslashes, quoted in
  `bootstrap.env` as the getting-started guide says, reached `bootstrap.sh`
  unchanged
- a pod runs on the node and `kubectl logs` reaches its kubelet
- after a reboot, the first-boot unit is skipped and the node is Ready again
- reset removes the units, config, host helpers, LocalDNS state, and machines,
  each of which is first checked to exist, and leaves `/opt/unbounded`, which
  belongs to the host

`--reset agent-reset` (the default) resets through an AgentReset
MachineOperation, `--reset cli` runs `aks-flex-node reset` on the VM, and
`--reset none` skips it. With the reboot, the first-boot unit is already
inactive when reset runs, so `test --no-reboot` is needed to check that reset
stops it.

`acl.py ssh [COMMAND]` opens a shell on the VM or runs a command there.

`down` stops the VM and removes the network, cluster, and `.vm/acl`. Run it
after an interrupted `up` as well, since it also removes leftover interfaces
and firewall rules. It keeps the exported images in `.vm/acl-images`.

## Image

Without `--image`, `up` boots the Azure build of the image, published to the
shared compute gallery image `ACL_IMAGE_GALLERY_IMAGE`. It is the image the
unbounded agent e2e boots, and the export is ported from that harness.

A gallery image can't be downloaded directly, so `up` exports it:

1. It creates a temporary managed disk from the version in
   `ACL_IMAGE_RESOURCE_GROUP`. The disk has to be in `ACL_IMAGE_GALLERY_LOCATION`,
   the one region the image is replicated to.
2. It reads the disk's written pages, about 850 MiB of 32.5 GiB, through a
   read SAS into a sparse VHD. The SAS lasts thirty minutes, so the download
   has to finish within that.
3. It revokes the SAS and deletes the disk, whether or not the export worked.
4. It converts the raw disk inside the VHD to qcow2, and checks that the qcow2
   has the disk's exact size. Converting the VHD itself would lose its last
   640 KiB, and the GPT's backup header with it, with `qemu-img` before 10.0.

More details:

- **Versions:** the version is the gallery's latest unless `ACL_IMAGE_VERSION`
  pins one, so every `up` without either setting asks `az` for it. The image is
  saved as `.vm/acl-images/acl-gallery-<version>.qcow2` and reused, after a
  `qemu-img check`, while that version is current, so a newer one is exported
  without a change here. To boot a saved image without `az`, pin its version
  with `ACL_IMAGE_VERSION`, or pass it with `--image`.
- **No digest:** the gallery publishes none. The VHD footer and the qcow2 are
  checked, and the qcow2's sha256 is logged.
- **Leftover disks:** each export first deletes export disks older than six
  hours, which an interrupted run can leave behind. Only disks tagged
  `purpose=aks-flex-node-acl-harness` are touched, so a resource group shared
  with the unbounded harness, which tags its own, is safe. A disk whose
  creation time can't be read is left alone.
- **The SAS** is on no command line and in no error message. In GitHub
  Actions it is also masked in the log, along with its signature on its own.

The identity `az` is logged in as needs:

- on the resource group: `Microsoft.Compute/disks/read`, `write`, `delete`,
  `beginGetAccess/action` and `endGetAccess/action`, and
  `Microsoft.Resources/subscriptions/resourceGroups/read`;
- on the subscription: `Microsoft.Compute/locations/sharedGalleries/read`,
  `.../images/read` and `.../images/versions/read`, which resolve the version
  and let a disk be made from it.

It has to be in a tenant the gallery is shared with. `ACL_IMAGE_SUBSCRIPTION`
overrides `az`'s default subscription.

## Settings

| Variable | Default | |
|---|---|---|
| `ACL_IMAGE` | | Local image, instead of `--image` |
| `ACL_IMAGE_RESOURCE_GROUP` | | Resource group the gallery image is exported through; required to export |
| `ACL_IMAGE_SUBSCRIPTION` | `az`'s default | Subscription of that resource group |
| `ACL_IMAGE_VERSION` | the gallery's latest | Gallery image version |
| `ACL_IMAGE_GALLERY_IMAGE` | `/SharedGalleries/b3e01d89-bd55-414f-bbb4-cdfeb2628caa-ACL/Images/acl-1es-eval` | Gallery image |
| `ACL_IMAGE_GALLERY_LOCATION` | `westus2` | Region the gallery image is replicated to |
| `ACL_IMAGE_DIR` | `.vm/acl-images` | Where exported images are kept |
| `ACL_KIND_NODE_IMAGE` | kind's default | kind node image, for another Kubernetes version |
| `ACL_KIND_CLUSTER` | `aks-flex-acl` | kind cluster name |
| `ACL_SUBNET` | `192.168.111` | First three octets of the bridge subnet |
| `ACL_SERVE_PORT` | `8299` | Port for the Ignition config and agent archive |
| `ACL_VM_MEMORY`, `ACL_VM_CPUS` | `4096`, `2` | VM size |
| `ACL_STATE_DIR` | `.vm/acl` | Build output, disk overlay, logs, and SSH key |
| `ACL_BUTANE_IMAGE` | `quay.io/coreos/butane:v0.29.0` | Butane image that renders the template |

## Troubleshooting

The VM's serial console is logged to `.vm/acl/serial.log`. On the VM,
`journalctl -u aks-flex-node-bootstrap.service` shows each bootstrap attempt.

The bridge networking, VM launch, UKI patching, and gallery export are adapted
from the unbounded repository's `hack/agent/e2e-kind` harness. `ukiboot.py` is copied
from it unchanged. It adds the Ignition config URL, the initramfs address, and
a mask for the Azure build's metadata hostname service to the image's
first-boot addon. `ignition-quench` deletes that addon after the first boot, so
Ignition runs only once and later boots use the image's own command line.
