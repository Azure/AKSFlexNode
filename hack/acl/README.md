# Azure Container Linux harness

`acl.py` boots an Azure Container Linux VM from the Ignition config that
`aks-flex-node ignition` renders, and joins it to a local kind cluster. It
exercises the path an Ignition-provisioned host takes: the first-boot unit runs
`bootstrap.sh`, the agent is installed under `/opt/unbounded`, which is writable
while `/usr` is not, and reset removes what bootstrap installed.

No Azure resources are used. The node joins with a bootstrap token, and the
agent's machine client talks to an AKS Flex controller deployed in the kind
cluster, reached through the API server's service proxy.

## Requirements

- Linux with KVM (`/dev/kvm` readable and writable)
- `docker`, `kind`, `kubectl`, `go`
- `qemu-system-x86_64`, `qemu-img`, `qemu-nbd`, and OVMF firmware (the `ovmf`
  or `edk2-ovmf` package)
- `ip`, `iptables`, `nsenter`, `ssh`, `ssh-keygen`
- `sudo` without a password prompt, for the bridge, TAP device, and firewall
  rules. Run `sudo -v` first if your sudo caches credentials.
- An Azure Container Linux qcow2 image

## Usage

```bash
hack/acl/acl.py up --image /path/to/acl.qcow2
hack/acl/acl.py test
hack/acl/acl.py down
```

`up` builds the agent and controller, creates the kind cluster and a bridge on
`192.168.111.0/24`, deploys the controller, renders the Ignition config, and
boots the VM. It returns once the first-boot unit has finished and the node is
Ready. The Ignition config and agent archive are served only while `up` runs, so
later boots cannot depend on them.

`test` checks that:

- the first-boot unit succeeded, removed the script that carries the bootstrap
  token, and installed the agent under `/opt/unbounded` and nothing under
  `/usr/local`
- an argument containing `$`, `%`, quotes, and backslashes reached `bootstrap.sh`
  unchanged
- a pod runs on the node and `kubectl logs` reaches its kubelet
- after a reboot, the first-boot unit is skipped and the node is Ready again
- reset removes the units, config, host helpers, LocalDNS state, and machines,
  each of which is first checked to exist

`--reset agent-reset` (the default) resets through an AgentReset
MachineOperation, `--reset cli` runs `aks-flex-node reset` on the VM, and
`--reset none` skips it. With the reboot, the first-boot unit is already
inactive when reset runs, so `test --no-reboot` is needed to check that reset
stops it.

`acl.py ssh [COMMAND]` opens a shell on the VM or runs a command there.

`down` stops the VM and removes the network, cluster, and `.vm/acl`. Run it
after an interrupted `up` as well, since it also removes leftover interfaces
and firewall rules.

## Settings

| Variable | Default | |
|---|---|---|
| `ACL_IMAGE` | | Image, instead of `--image` |
| `ACL_KIND_NODE_IMAGE` | kind's default | kind node image, for another Kubernetes version |
| `ACL_KIND_CLUSTER` | `aks-flex-acl` | kind cluster name |
| `ACL_SUBNET` | `192.168.111` | First three octets of the bridge subnet |
| `ACL_SERVE_PORT` | `8299` | Port for the Ignition config and agent archive |
| `ACL_VM_MEMORY`, `ACL_VM_CPUS` | `4096`, `2` | VM size |
| `ACL_STATE_DIR` | `.vm/acl` | Build output, disk overlay, logs, and SSH key |

## Troubleshooting

The VM's serial console is logged to `.vm/acl/serial.log`. On the VM,
`journalctl -u aks-flex-node-bootstrap.service` shows each bootstrap attempt.

The bridge networking, VM launch, and UKI patching are adapted from the
unbounded repository's `hack/agent/e2e-kind` harness. `ukiboot.py` is copied
from it unchanged; it adds the Ignition URL to a UKI command line addon, so that
Ignition runs only on the first boot.
