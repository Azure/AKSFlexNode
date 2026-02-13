# QEMU Local VM

A self-contained toolkit for creating, managing, and tearing down local QEMU virtual machines running Ubuntu 24.04 (Noble). Useful as a local development and testing environment for AKS FlexNode.

## Prerequisites

| Dependency | macOS | Linux (Debian/Ubuntu) |
|---|---|---|
| QEMU | `brew install qemu` | `sudo apt-get install qemu-system` |
| `qemu-img` | Included with QEMU | Included with `qemu-utils` |
| ISO tool | `hdiutil` (built-in) or `brew install cdrtools` | `sudo apt-get install genisoimage` |
| `curl` | Pre-installed | `sudo apt-get install curl` |

Hardware virtualization is strongly recommended (HVF on macOS, KVM on Linux).

On arm64, UEFI firmware (`edk2-aarch64-code.fd`) must be available at a standard path.

## Quick Start

```bash
# Start a VM with defaults (2 CPUs, 2GB RAM, 20G disk, SSH on port 2222)
./hack/qemu/vm.sh start

# SSH into the VM
ssh -o StrictHostKeyChecking=no -p 2222 ubuntu@localhost

# View serial console output
./hack/qemu/vm.sh logs

# Stop the VM
./hack/qemu/vm.sh stop

# Stop and clean up all artifacts
./hack/qemu/vm.sh stop --clean
```

## Commands

### `start`

Downloads the Ubuntu 24.04 cloud image (if needed), creates a qcow2 disk, renders cloud-init user data with your SSH key, builds a seed ISO, and launches QEMU in the background.

| Flag | Long Form | Default | Description |
|---|---|---|---|
| `-n` | `--name` | `flexnode-vm` | VM instance name |
| `-m` | `--memory` | `2048` | Guest RAM in MB |
| `-c` | `--cpus` | `2` | Number of virtual CPUs |
| `-d` | `--disk-size` | `20G` | Disk size |
| `-p` | `--ssh-port` | `2222` | Host port forwarded to guest SSH |
| `-i` | `--image` | auto-downloaded | Path to an existing Ubuntu cloud image |
| `-u` | `--user-data` | `hack/qemu/user-data.yaml` | Path to cloud-init user-data template |
| | `--no-snapshot` | off | Copy the base image instead of creating a qcow2 snapshot |

### `stop`

Sends SIGTERM (or SIGKILL with `--force`) to the QEMU process and optionally removes generated artifacts.

| Flag | Long Form | Default | Description |
|---|---|---|---|
| `-n` | `--name` | `flexnode-vm` | VM name to stop |
| `-f` | `--force` | off | Send SIGKILL immediately |
| | `--clean` | off | Remove disk, seed ISO, log, and rendered cloud-init files |

### `logs`

Displays the VM's serial console log.

| Flag | Long Form | Default | Description |
|---|---|---|---|
| `-n` | `--name` | `flexnode-vm` | VM name |
| `-f` | `--follow` | off | Tail the log continuously |

## Examples

**Custom VM with more resources:**

```bash
./hack/qemu/vm.sh start --name my-vm --memory 4096 --cpus 4 --disk-size 40G --ssh-port 2223
```

**Run multiple VMs simultaneously** (use different names and SSH ports):

```bash
./hack/qemu/vm.sh start -n vm1 -p 2222
./hack/qemu/vm.sh start -n vm2 -p 2223

# Stop individually
./hack/qemu/vm.sh stop -n vm1
./hack/qemu/vm.sh stop -n vm2 --clean
```

**Follow serial console output:**

```bash
./hack/qemu/vm.sh logs -n my-vm --follow
```

## Development Workflow

The repository root is shared into the guest at `/flex-node` via virtio-9p. This means you can compile the binary on your host machine and immediately run it inside the VM -- no need to copy files or install a Go toolchain in the guest.

```bash
# On the host: build the binary (targeting Linux)
GOOS=linux go build -o bin/aks-flex-node ./cmd/aks-flex-node

# SSH into the VM
ssh -p 2222 ubuntu@localhost

# Inside the VM: the repo is already mounted
cd /flex-node
sudo su
./bin/aks-flex-node agent --config <path-to-config>
```

Edit code and rebuild on the host as usual. Since `/flex-node` is a live mount of the repository, the updated binary is available inside the VM immediately after each build.

## How It Works

1. **Image download** -- Fetches the Ubuntu 24.04 Minimal cloud image from `cloud-images.ubuntu.com` into `.vm/ubuntu-cloud.img` (skipped if already present).
2. **Disk creation** -- Creates a qcow2 snapshot backed by the base image (or a full copy with `--no-snapshot`).
3. **SSH key injection** -- Scans `~/.ssh/` for public keys (`id_ed25519.pub`, `id_rsa.pub`, `id_ecdsa.pub`) and substitutes them into the user-data template.
4. **Seed ISO** -- Packs rendered `user-data` and generated `meta-data` into a NoCloud-compatible ISO.
5. **QEMU launch** -- Starts QEMU daemonized with virtio disk, seed ISO, SSH port forwarding, hardware acceleration, and a **9p shared filesystem** that mounts the repository root into the guest at `/flex-node`.
6. **SSH readiness** -- Polls SSH connectivity for up to 3 minutes before printing connection instructions.

## Guest Environment

The VM is provisioned via cloud-init (`user-data.yaml`) with:

- **User:** `ubuntu` with passwordless sudo and your SSH key
- **Packages:** `curl`, `jq`, `net-tools`, `vim`, `apt-transport-https`, `ca-certificates`
- **Shared filesystem:** The repository root is mounted at `/flex-node` inside the guest via virtio-9p
- **Provisioning marker:** `/etc/flexnode/provisioned` is written on first boot

## Runtime Artifacts

All artifacts are stored in `<repo-root>/.vm/` (git-ignored):

| File | Description |
|---|---|
| `ubuntu-cloud.img` | Downloaded base cloud image |
| `<name>.qcow2` | VM disk |
| `<name>-seed.iso` | Cloud-init seed ISO |
| `<name>.pid` | QEMU process ID |
| `<name>.log` | Serial console log |
| `user-data.yaml` | Rendered user-data |
| `meta-data` | Generated cloud-init meta-data |
