# Azure Container Linux: privileged containers can't use the host's D-Bus under SELinux

## Summary

On Azure Container Linux (ACL), SELinux is enforcing, but the files in `/usr`
have no SELinux labels, so every host process runs as `kernel_t`: systemd,
dbus-daemon, systemd-machined, systemd-logind, systemd-resolved, and containerd
alike.

A privileged container that containerd starts runs as `spc_t`, as
container-selinux intends. The policy lets `spc_t` send D-Bus messages to the
domains those daemons would run in if `/usr` were labeled (`init_t`,
`system_dbusd_t`, `systemd_machined_t`, `systemd_logind_t`), but not to
`kernel_t`. So dbus-daemon refuses a privileged container's first message, and
`machinectl`, `busctl`, `loginctl`, `hostnamectl`, `resolvectl`, and any other
D-Bus client in the container fail with `Access denied`.

## The rule needed

```text
allow spc_t kernel_t:dbus send_msg;
```

In CIL: `(allow spc_t kernel_t (dbus (send_msg)))`.

With this one rule added to ACL's policy, and nothing else, every host
operation the AKS Flex Node agent performs works from a privileged container in
`spc_t`, with no SELinux denials: all 59 checks below. We verified it on ACL
3.0.20261005 with [`selinux-verify.sh`](selinux-verify.sh):

```text
PROBE                                    CONTEXT   NEW-MACHINE FAILED    DENIALS
current policy                           spc_t     -           16/49     dbus:send_msg=23
policy candidate-dbus.policy             spc_t     kernel_t    0/59      none
workaround (native snapshotter, usr_t)   kernel_t  spc_t       1/59      service:reload=1
```

Under the current policy every failure is a D-Bus call; everything else
already works from `spc_t`. (The current policy runs fewer checks, 49, because
the machine the container creates never starts.) The workaround row is our
current workaround, described [below](#our-current-workaround); its one failure
is [a separate gap](#a-separate-gap-systemctl-reset-failed-as-host-root).

No other rule is needed. We also tested
`allow spc_t unlabeled_t:service { enable disable };`, which we had expected to
need for enabling units; it made no difference, as enabling and disabling units
already works from `spc_t`.

### What was checked, and why

We audited every way the agent touches the host, in the AKS Flex Node code
(`main` and the ACL branch) and in the agent library it builds on,
[Azure/unbounded](https://github.com/Azure/unbounded) (v0.8.0 and v0.13.0).
Neither uses a D-Bus library: all host interaction is through the host's
command-line tools. The checks are those commands, with the arguments the agent
uses, run from a privileged container through `chroot /host`:

| What the agent does | Checks |
|---|---|
| Machines, through machined (D-Bus) | `machinectl list`, `show`, `start`, `poweroff`, `terminate`, `enable`, `disable`, `shell`, `remove` |
| DNS, through systemd-resolved (D-Bus) | `resolvectl domain` |
| Other host services (D-Bus) | `busctl list`, `loginctl list-sessions`, `hostnamectl` |
| Units it installs, through systemd's private socket | `systemctl daemon-reload`, `enable`, `is-enabled`, `start`, `restart`, `stop`, `reset-failed`, `disable`, `enable --now`, `disable --now`, on a unit the container writes into `/etc/systemd/system` |
| Units it disables, such as swap | `systemctl mask --now` and `unmask` of a unit in `/usr` |
| Self-upgrade restart | `systemd-run --collect --unit=… --on-active=1s …`, a transient timer on the host |
| Its own images, as for an upgrade, through containerd's socket, `/run/containerd/containerd.sock` | `ctr version`, `namespaces list`, `image pull`, `run --rm` of a short-lived container, `image rm` |
| Commands in a machine | `systemd-run --machine=NAME --pipe --wait …` |
| Kernel and files | `modprobe`; `ip link` add and delete; `nft` add and delete table; `sysctl -w`; `mount` and `umount` of bpffs, and `mountpoint`; `udevadm info`; `swapoff -a`; `restorecon -R` |
| Machines it creates | A machine whose root filesystem the container copies in without labels, as the agent unpacks one, then started, entered, enabled, disabled, powered off, terminated, and removed |

The agent also runs `ethtool`, `lspci`, `dmidecode`, `ldconfig`, `lldpctl`, and
`nvidia-smi` for inventory and GPU support, and package managers on other
distributions. They use neither D-Bus nor systemd, and weren't run.

Labeling `/usr`, the more thorough fix, we could not test; see
[Possible fixes](#possible-fixes).

## A separate gap: `systemctl reset-failed` as host root

Root on an ACL host can't run `systemctl reset-failed`. A root shell is
`kernel_t`, every unit file is `unlabeled_t`, and `kernel_t` has only
`{ start status stop }` on `unlabeled_t:service`. systemd checks
`reset-failed`, and `systemctl reload` too, as `reload`:

```text
$ sudo systemctl reset-failed systemd-pstore.service
Failed to reset failed state of unit systemd-pstore.service: Access denied

avc:  denied  { reload } for auid=n/a uid=0 gid=0 path="/usr/lib/systemd/system/systemd-pstore.service"
  cmdline="" function="bus_unit_method_reset_failed" scontext=system_u:system_r:kernel_t:s0
  tcontext=system_u:object_r:unlabeled_t:s0 tclass=service permissive=0
```

`allow kernel_t unlabeled_t:service reload;` fixes it: we loaded a policy with
only that added, and the same command succeeded. Labeling `/usr` would fix it
too.

It doesn't block us: `spc_t` already has `reload`, so a privileged container
can run `reset-failed`, and the agent treats a failed `reset-failed` as
best-effort anyway. It affects anything on the host that runs as root.

## Scenario

The AKS Flex Node agent manages systemd-nspawn machines on a host, which run
the Kubernetes node. On ACL, we want to ship the agent as an OCI image, and
have Ignition start it as a privileged containerd container instead of
installing it on the host. In the container, the agent has:

- the host's root bind-mounted at `/host`, recursively
- the host's network and PID namespaces
- the host's own `machinectl`, `systemctl`, and `systemd-run`, run through
  `chroot /host`, so their versions always match the host's systemd

`machinectl` talks to systemd-machined over the host's system D-Bus, and that
is what fails.

## Environment

| | |
|---|---|
| Image | Shared gallery image `acl-1es-eval`, version `3.20261007.1021` |
| OS | Microsoft Azure Container Linux `3.0.20261005`, `BUILD_ID=1217036` |
| Kernel | `6.6.157.1-1.azl3` |
| systemd | `255 (255-34.azl3)` |
| containerd, runc | `2.3.4`, `1.4.3`, both from ACL's `/usr` |
| SELinux | `targeted`, enforcing, MLS. Compiled policy `/etc/selinux/targeted/policy/policy.33` (sha256 `f94ebb51f413eb98…`). No policy store under `/var/lib/selinux`. |
| `/usr` | btrfs on dm-verity, read-only, mounted with `seclabel` |

We booted the image under QEMU/KVM, provisioned by Ignition. The problem comes
from the image's file labels and policy, so it should behave the same on an
Azure VM, but we have not run it on one.

## Reproducing it

### By hand

On an ACL host, as root:

```bash
ctr image pull mcr.microsoft.com/azurelinux/busybox:1.36
ctr run --rm --privileged --net-host --with-ns pid:/proc/1/ns/pid \
  --mount type=bind,src=/,dst=/host,options=rbind:rslave \
  mcr.microsoft.com/azurelinux/busybox:1.36 repro \
  sh -c 'cat /proc/self/attr/current; echo; chroot /host /usr/bin/machinectl list' </dev/null
```

```text
system_u:system_r:spc_t:s0
Could not get machines: Access denied
```

`machinectl list` run on the host works. dbus-daemon logs the denial
(`journalctl -b | grep 'avc:  denied'`):

```text
USER_AVC ... subj=system_u:system_r:kernel_t:s0 msg='avc:  denied  { send_msg } for
  msgtype=method_call interface=org.freedesktop.DBus member=Hello dest=org.freedesktop.DBus
  scontext=system_u:system_r:spc_t:s0 tcontext=system_u:system_r:kernel_t:s0 tclass=dbus
  permissive=0 exe="/usr/bin/dbus-daemon" ...'
```

The container is refused at its first message, the bus `Hello`.

### On a host: `selinux-repro.sh`

[`selinux-repro.sh`](selinux-repro.sh) is self-contained. Copy it to an ACL host
that can pull `mcr.microsoft.com/azurelinux/busybox:1.36` (or set `IMAGE` to
another image with `sh` and `chroot`), and run it as root:

```bash
sudo bash selinux-repro.sh                         # the environment, and the failure
sudo bash selinux-repro.sh --policy FILE           # also under a candidate binary policy;
                                                   # repeat --policy to compare several
sudo bash selinux-repro.sh --workaround            # also our current workaround
sudo MACHINE=NAME bash selinux-repro.sh ...        # also the machine checks
```

`MACHINE` names an existing, running systemd-nspawn machine. Each probe drives
it, and creates and drives a second machine from a copy of its root
filesystem.

It prints the OS and policy versions, each host binary's label in the policy's
`file_contexts` against its actual label, and the contexts of the host's
daemons. It then runs the checks above from a privileged container, once for
the current policy and once for each `--policy` and `--workaround`. Each probe
lists the SELinux denials logged while it ran, and a summary table ends the
output.

It puts back everything it changes:

- It works in containerd namespaces of its own, `acl-selinux-repro` and
  `acl-selinux-repro-ctr`, and removes them.
- After every probe, it removes what the probe created: the test unit, the
  transient timer, the dummy interface, the nft table, the bpffs mount, the
  test files, the image and container the probe ran through containerd, and the
  second machine. It unmasks the masked unit, unloads the `dummy` module if it
  loaded it, and restores `MACHINE`'s enablement and running state.
- `--policy` loads each file only for its probe, then puts back the original
  policy and reloads it.
- `--workaround` relabels the containerd native snapshotter's two top
  directories, and puts their labels back.

### From a workstation: `selinux-verify.sh`

[`selinux-verify.sh`](selinux-verify.sh) builds candidate policies from the
host's own policy and runs `selinux-repro.sh` against them. Run it on a machine
with podman and SSH access to an ACL host where the user can sudo:

```bash
ACL_SSH="ssh core@ACL_HOST" MACHINE=NAME ./selinux-verify.sh
```

`ACL_SSH` is a command that runs its last argument on the host, passing stdin
through; `MACHINE`, optional, is as above. The script:

1. copies the host's compiled policy
2. in a Fedora container with `checkpolicy`, `secilc`, and setools, converts it
   to CIL, adds each candidate's rules, compiles it again, and checks that it
   differs from the host's policy by exactly those rules
3. runs `selinux-repro.sh` on the host, with every candidate and the workaround

The candidates are a list at the top of the script; its default is the rule
above. The converted policy keeps the original's MLS, policy version, and
handling of unknown classes, and converting it back without changes gives a
policy identical to the original in types, rules, type transitions, booleans,
classes, and roles. This rebuild is only a way to test; a real fix belongs in
ACL's policy build.

## What is affected

From a privileged container (`spc_t`), with ACL's policy as shipped:

| Operation | Result | Why |
|---|---|---|
| Anything through dbus-daemon: every `machinectl` command, `busctl`, `loginctl`, `hostnamectl`, `resolvectl` | **Denied** | `spc_t` may not send to `kernel_t` on the bus |
| `systemctl`, including `daemon-reload`, `enable`, `disable`, `mask`, `reset-failed`, and `--now`; host `systemd-run` | Works | As root, these talk to systemd on its private socket, `/run/systemd/private`, not through dbus-daemon, and systemd's own SELinux checks pass |
| `systemd-run --machine=NAME` | Works | It finds the machine's leader in machined's state under `/run/systemd/machines`, and talks to the bus inside the machine |
| `ctr`, through containerd's socket: pulling, running, and removing images | Works | containerd runs as `kernel_t` and its socket is `container_runtime_t`, which `spc_t` may connect to, and containerd makes no SELinux checks of its own, unlike dbus-daemon |
| Kernel and file operations: modules, links, nft, sysctl, mounts, relabeling | Works | `spc_t` is unconfined at the kernel level |

So a privileged container can manage units and its own images, but not
machines, sessions, DNS, or anything else reached over the system bus.

## Root cause

### `/usr` is unlabeled, so no host process changes domain

The policy's `file_contexts` gives each binary and unit file its type, but the
files are `unlabeled_t`:

| File | Label in `file_contexts` (`matchpathcon`) | Actual label |
|---|---|---|
| `/usr/lib/systemd/systemd` | `init_exec_t` | `unlabeled_t` |
| `/usr/bin/dbus-daemon` | `dbusd_exec_t` | `unlabeled_t` |
| `/usr/lib/systemd/systemd-machined` | `systemd_machined_exec_t` | `unlabeled_t` |
| `/usr/lib/systemd/systemd-logind` | `systemd_logind_exec_t` | `unlabeled_t` |
| `/usr/bin/runc` | `container_engine_exec_t` | `unlabeled_t` |
| `/usr/bin/containerd`, `/usr/bin/machinectl` | `bin_t` | `unlabeled_t` |
| `/usr/lib/systemd/system/systemd-nspawn@.service`, `/etc/systemd/system` | `systemd_unit_t` | `unlabeled_t` |

Without an entry-point type nothing changes domain at exec, so systemd stays in
the kernel's initial domain, `kernel_t`, and so does everything it starts.

### A container's processes do change domain, to `spc_t`

The policy moves `kernel_t` to `spc_t` when it executes a file with a container
type:

```text
type_transition kernel_t container_file_t:process spc_t;
type_transition kernel_t container_ro_file_t:process spc_t;
type_transition kernel_t container_var_lib_t:process spc_t;
```

The files in containerd's snapshots are `container_var_lib_t`, because any
directory `kernel_t` creates in a `var_lib_t` one gets that type:

```text
type_transition kernel_t var_lib_t:dir container_var_lib_t;
```

### `spc_t` may only message the daemons' labeled domains

The D-Bus rules for `spc_t`:

```text
allow spc_t init_t:dbus send_msg;
allow spc_t system_dbusd_t:dbus send_msg;
allow privileged_container_domain systemd_machined_t:dbus send_msg;
allow privileged_container_domain systemd_logind_t:dbus send_msg;
allow spc_t spc_t:dbus { acquire_svc send_msg };
```

and the only types that may send to `kernel_t`:

```text
allow kernel_t kernel_t:dbus { acquire_svc send_msg };
allow init_t kernel_t:dbus send_msg;
allow systemd_logind_t kernel_t:dbus send_msg;
```

The other direction is already allowed: `kernel_t` has the `dbusd_unconfined`
attribute and `spc_t` the `dbusd_system_bus_client` one, and
`allow dbusd_unconfined dbusd_system_bus_client:dbus send_msg;` covers replies
and signals. No boolean in the policy changes any of this.

### A container can't get back to `kernel_t`

From the container, `runcon system_u:system_r:kernel_t:s0` or `nsenter -Z` to
run the host tool as `kernel_t`:

```text
avc:  denied  { nnp_transition } ... scontext=spc_t tcontext=kernel_t tclass=process2
avc:  denied  { entrypoint } ... path="/usr/bin/machinectl" scontext=kernel_t tcontext=unlabeled_t tclass=file
```

`ctr` sets NoNewPrivileges by default. Without it, `kernel_t` still can't be
entered from an `unlabeled_t` file. `process.selinuxLabel` set to `kernel_t` in
the OCI spec runs into the same `entrypoint` check, against
`container_var_lib_t`. A local policy module can't be added either, as there is
no policy store for `semodule`.

## Possible fixes

In order of preference:

1. **Label `/usr` when the image is built.** Run `setfiles` with the image's own
   `file_contexts` before the dm-verity hash is computed. btrfs keeps the labels
   as xattrs, and `/usr` is already mounted with `seclabel`.
   - systemd would run as `init_t`, dbus-daemon as `system_dbusd_t`, machined as
     `systemd_machined_t`, and runc as `container_engine_t`. Privileged
     containers would still be `spc_t`, which the existing rules already let
     talk to all of them. So standard privileged containers, including
     Kubernetes privileged pods, and the default overlayfs snapshotter should
     work as they do on other SELinux distributions. It would also fix
     [`reset-failed` for host root](#a-separate-gap-systemctl-reset-failed-as-host-root).
   - SELinux would then confine the host's services. Today enforcing mode
     constrains little more than processes that move into container domains.
   - The risk is that services that work now only because everything is
     unconfined start meeting denials. That needs a test pass, perhaps with
     some domains permissive at first.
   - We couldn't test this: `/usr` is read-only and verity-protected.
2. **Add `allow spc_t kernel_t:dbus send_msg;` to the policy**, while `/usr`
   stays unlabeled. Verified above against every host operation the agent
   performs. It allows `spc_t` to message every service on the host bus, since
   all of them are `kernel_t`; as `spc_t` is meant to be unconfined, that
   matches its intent. It would want revisiting if `/usr` is labeled later.
   Adding `allow kernel_t unlabeled_t:service reload;` alongside it would fix
   `reset-failed` for host root.
3. **A dedicated domain for host-management system containers**, such as
   `aks_flex_agent_t`, that a container can request through its OCI
   `process.selinuxLabel`, with access to systemd, machined, and logind. More
   precise than widening `spc_t`, but more work, and still dependent on 1 or 2
   for the target types.
4. **A policy store on the host**, so a node's provisioning, such as an
   Ignition config, could install a module with `semodule -i`. That lets
   consumers carry their own rules, at the cost of hosts whose policy differs
   from the image's.

## Our current workaround

Until one of those lands, we keep our container in `kernel_t`, the domain every
other host process runs in. We do that by giving its files a type the policy
has no transition for. The container runs on containerd's native snapshotter,
which nothing else on ACL uses, with that snapshotter's directory relabeled
`usr_t`. `kernel_t` has no file-creation transition in `usr_t` directories, so
new snapshots inherit the type:

```ini
ExecStartPre=/usr/bin/mkdir -p /var/lib/containerd/io.containerd.snapshotter.v1.native/snapshots
ExecStartPre=/usr/bin/chcon -t usr_t /var/lib/containerd/io.containerd.snapshotter.v1.native /var/lib/containerd/io.containerd.snapshotter.v1.native/snapshots
ExecStartPre=/usr/bin/ctr --namespace aks-flex image pull --local --snapshotter native IMAGE
ExecStart=/usr/bin/ctr --namespace aks-flex run --rm --snapshotter native --privileged ... IMAGE aks-flex-node
```

`selinux-repro.sh --workaround` shows it: the container is `kernel_t`, and
every check passes except `reset-failed`, which `kernel_t` can't do. It has
costs we would like to drop:

- The native snapshotter copies rather than overlays. Every container start
  copies the image, about 20 seconds for a 200 MB image on our test VM, and it
  uses about twice the image's size on disk, more for an image with many layers.
- It depends on today's policy and on a `chcon` under `/var`, which a full
  relabel would undo.
- The container is as unconfined as the rest of the host, rather than in the
  domain meant for it.

## Other observations

- **Kubernetes privileged pods.** ACL's containerd config enables SELinux in
  its CRI plugin (`enable_selinux = true`). We expect privileged containers
  started through CRI to end up in `spc_t` the same way, and so to be affected
  the same way. We only tested with `ctr`.
- **A machine's domain depends on who creates its directory.** Under
  `/var/lib/machines`, which is `var_lib_t`, a directory created by a `kernel_t`
  process becomes `container_var_lib_t` by the rule above, so systemd-nspawn
  moves to `spc_t` when it starts that machine's init. One created by an `spc_t`
  process stays `var_lib_t`, and its machine runs as `kernel_t`. So machines an
  agent on the host creates run as `spc_t`, and machines an agent in a
  privileged container creates run as `kernel_t`. With the rule above, both
  work in full. Probably neither is by design.

## Questions

1. Is the unlabeled `/usr` intentional, and is labeling it at build time
   planned?
2. Can `allow spc_t kernel_t:dbus send_msg;` land in an upcoming ACL build, and
   which one?
3. Should `kernel_t` have `reload` on `unlabeled_t` units, so host root can run
   `systemctl reset-failed`?
4. Which domain should systemd-nspawn machines under `/var/lib/machines` run
   in?

## Appendix: checking the policy

ACL has no `sesearch`. We copied `policy.33` off the host and queried it with
setools; the equivalent `sesearch` commands:

```bash
sesearch -A -s spc_t -c dbus policy.33                    # spc_t's D-Bus rules
sesearch -A -t kernel_t -c dbus -p send_msg policy.33     # who may send to kernel_t
sesearch -T -c process -D spc_t policy.33                 # transitions into spc_t
sesearch -T -s kernel_t -t var_lib_t policy.33            # the container_var_lib_t rule
sesearch -A -s kernel_t -c file -p entrypoint policy.33   # how kernel_t can be entered
sesearch -A -s kernel_t -t unlabeled_t -c service policy.33   # host root's unit permissions
```

## Appendix: verification output

`ACL_SSH=... MACHINE=kube1 ./selinux-verify.sh` against our test host, where the
agent runs its node in a systemd-nspawn machine, `kube1`:

```text
== Copying the host's policy into /tmp/opencode/aclc/verify
policy.33, f94ebb51f413eb98

== Building and checking the candidates
candidate-dbus.policy grants, over the host policy:
  allow spc_t kernel_t:dbus send_msg;

== Running selinux-repro.sh on the host

== Environment
OS          Microsoft Azure Container Linux 3.0.20261005 (VERSION=3.0.20261005 BUILD_ID=1217036)
kernel      6.6.157.1-1.azl3
systemd     systemd 255 (255-34.azl3)
containerd  2.3.4
runc        runc version 1.4.3
SELinux     Enforcing, /etc/selinux/targeted/policy/policy.33 (f94ebb51f413eb98), policy store: none
/usr        btrfs ro,relatime,seclabel,space_cache=v2,subvolid=5,subvol=/

== Host binaries and unit files: label in file_contexts vs. actual label
FILE                                           EXPECTED                     ACTUAL
/usr/lib/systemd/systemd                       init_exec_t                  unlabeled_t
/usr/bin/dbus-daemon                           dbusd_exec_t                 unlabeled_t
/usr/lib/systemd/systemd-machined              systemd_machined_exec_t      unlabeled_t
/usr/lib/systemd/systemd-logind                systemd_logind_exec_t        unlabeled_t
/usr/bin/containerd                            bin_t                        unlabeled_t
/usr/bin/runc                                  container_engine_exec_t      unlabeled_t
/usr/bin/machinectl                            bin_t                        unlabeled_t
/usr/lib/systemd/system/systemd-nspawn@.service systemd_unit_t               unlabeled_t
/etc/systemd/system                            systemd_unit_t               unlabeled_t
/var/lib/machines                              var_lib_t                    var_lib_t
/run/containerd/containerd.sock                container_runtime_t          container_runtime_t

== Host processes
systemd            system_u:system_r:kernel_t:s0
dbus-daemon        system_u:system_r:kernel_t:s0
systemd-machine    system_u:system_r:kernel_t:s0
systemd-logind     system_u:system_r:kernel_t:s0
systemd-resolve    system_u:system_r:kernel_t:s0
containerd         system_u:system_r:kernel_t:s0
machine kube1      system_u:system_r:spc_t:s0

== Privileged container, default snapshotter (overlayfs), current policy
  container context: system_u:system_r:spc_t:s0
  -- through dbus-daemon
  machinectl list --no-pager --no-legend                         exit 1  Could not get machines: Access denied
  busctl --system --no-pager --no-legend list --acquired         exit 1  Failed to list names: Permission denied
  loginctl list-sessions --no-pager --no-legend                  exit 1  Failed to list sessions: Access denied
  hostnamectl --static                                           exit 1  Could not get property: Access denied
  resolvectl domain                                              exit 1  Failed to get global data: Access denied
  -- units
  systemctl is-system-running                                    exit 0  running
  acl-selinux-repro.service label: system_u:object_r:unlabeled_t:s0
  systemctl daemon-reload                                        exit 0  
  systemctl enable acl-selinux-repro.service                     exit 0  Created symlink /etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service → /etc/systemd/system/acl-selinux-repro.service.
  systemctl is-enabled acl-selinux-repro.service                 exit 0  enabled
  systemctl start acl-selinux-repro.service                      exit 0  
  systemctl restart acl-selinux-repro.service                    exit 0  
  systemctl stop acl-selinux-repro.service                       exit 0  
  systemctl reset-failed acl-selinux-repro.service               exit 0  
  systemctl disable acl-selinux-repro.service                    exit 0  Removed "/etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service".
  systemctl enable --now acl-selinux-repro.service               exit 0  Created symlink /etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service → /etc/systemd/system/acl-selinux-repro.service.
  systemctl disable --now acl-selinux-repro.service              exit 0  Removed "/etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service".
  systemctl mask --now systemd-pstore.service                    exit 0  Created symlink /etc/systemd/system/systemd-pstore.service → /dev/null.
  systemctl unmask systemd-pstore.service                        exit 0  Removed "/etc/systemd/system/systemd-pstore.service".
  systemd-run --quiet --collect --unit=acl-selinux-repro-timer --on-active=1s /usr/bin/true exit 0  
  -- containerd, through its socket
  ctr version                                                    exit 0  Client:
  ctr namespaces list                                            exit 0  NAME              LABELS 
  ctr --namespace acl-selinux-repro-ctr image pull mcr.microsoft.com/azurelinux/busybox:1.36 exit 0  mcr.microsoft.com/azurelinux/busybox:1.3	fetching image content	
  ctr --namespace acl-selinux-repro-ctr run --rm mcr.microsoft.com/azurelinux/busybox:1.36 acl-selinux-repro-run true exit 0  
  ctr --namespace acl-selinux-repro-ctr image rm mcr.microsoft.com/azurelinux/busybox:1.36 exit 0  mcr.microsoft.com/azurelinux/busybox:1.36
  -- kernel and files
  modprobe dummy                                                 exit 0  
  ip link add aclrepro0 type dummy                               exit 0  
  ip link del aclrepro0                                          exit 0  
  nft add table inet acl_selinux_repro                           exit 0  
  nft delete table inet acl_selinux_repro                        exit 0  
  sysctl -w net.ipv4.ip_forward=1                                exit 0  net.ipv4.ip_forward = 1
  mount -t bpf bpf /run/acl-selinux-repro-bpf                    exit 0  
  mountpoint /run/acl-selinux-repro-bpf                          exit 0  /run/acl-selinux-repro-bpf is a mountpoint
  umount /run/acl-selinux-repro-bpf                              exit 0  
  udevadm info --query=property --name=/dev/vda                  exit 0  DEVPATH=/devices/pci0000:00/0000:00:03.0/virtio1/block/vda
  swapoff -a                                                     exit 0  
  restorecon -R /opt/acl-selinux-repro                           exit 0  
  machinectl remove acl-selinux-repro-img                        exit 1  Could not remove image: Access denied
  -- the existing machine kube1
  machinectl show kube1 -p State --value                         exit 1  Could not get path to machine: Access denied
  machinectl --quiet shell kube1 /bin/sh -c echo shell-ok        exit 1  Failed to get shell PTY: Access denied
  systemd-run --machine=kube1 --pipe --wait --quiet cat /etc/os-release exit 0  NAME="Microsoft Azure Linux"
  machinectl enable kube1                                        exit 1  Failed to check whether image kube1 exists: Access denied
  machinectl disable kube1                                       exit 1  Failed to check whether image kube1 exists: Access denied
  machinectl poweroff kube1                                      exit 1  Could not kill machine: Access denied
  machinectl start kube1                                         exit 1  Failed to check whether image kube1 exists: Access denied
  machinectl terminate kube1                                     exit 1  Could not terminate machine: Access denied
  machinectl start kube1                                         exit 1  Failed to check whether image kube1 exists: Access denied
  -- a machine this container creates, acl-selinux-repro-m
  cp -dR --preserve=mode,ownership,timestamps,links /var/lib/machines/kube1 /var/lib/machines/acl-selinux-repro-m exit 0  
  its init label: system_u:object_r:var_lib_t:s0
  machinectl start acl-selinux-repro-m                           exit 1  Failed to check whether image acl-selinux-repro-m exists: Access denied
  machinectl remove acl-selinux-repro-m                          exit 1  Could not remove image: Access denied
  SELinux denials:       5 { send_msg } for msgtype=method_call interface=org.freedesktop.DBus member=AddMatch dest=org.freedesktop.DBus scontext=system_u:system_r:spc_t:s0 tcontext=system_u:system_r:kernel_t:s0 tclass=dbus permissive=0
     18 { send_msg } for msgtype=method_call interface=org.freedesktop.DBus member=Hello dest=org.freedesktop.DBus scontext=system_u:system_r:spc_t:s0 tcontext=system_u:system_r:kernel_t:s0 tclass=dbus permissive=0

== Privileged container, default snapshotter, policy /tmp/acl-selinux-verify/candidate-dbus.policy
  loaded /tmp/acl-selinux-verify/candidate-dbus.policy (69ca96bf5e28ec80)
  container context: system_u:system_r:spc_t:s0
  -- through dbus-daemon
  machinectl list --no-pager --no-legend                         exit 0  kube1 container systemd-nspawn azurelinux 3.0 -
  busctl --system --no-pager --no-legend list --acquired         exit 0  org.freedesktop.DBus        1 systemd         root            -    init.scope               - -
  loginctl list-sessions --no-pager --no-legend                  exit 0  144 500 core - - active no -
  hostnamectl --static                                           exit 0  acl-flex-proto
  resolvectl domain                                              exit 0  Global:
  -- units
  systemctl is-system-running                                    exit 0  running
  acl-selinux-repro.service label: system_u:object_r:unlabeled_t:s0
  systemctl daemon-reload                                        exit 0  
  systemctl enable acl-selinux-repro.service                     exit 0  Created symlink /etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service → /etc/systemd/system/acl-selinux-repro.service.
  systemctl is-enabled acl-selinux-repro.service                 exit 0  enabled
  systemctl start acl-selinux-repro.service                      exit 0  
  systemctl restart acl-selinux-repro.service                    exit 0  
  systemctl stop acl-selinux-repro.service                       exit 0  
  systemctl reset-failed acl-selinux-repro.service               exit 0  
  systemctl disable acl-selinux-repro.service                    exit 0  Removed "/etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service".
  systemctl enable --now acl-selinux-repro.service               exit 0  Created symlink /etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service → /etc/systemd/system/acl-selinux-repro.service.
  systemctl disable --now acl-selinux-repro.service              exit 0  Removed "/etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service".
  systemctl mask --now systemd-pstore.service                    exit 0  Created symlink /etc/systemd/system/systemd-pstore.service → /dev/null.
  systemctl unmask systemd-pstore.service                        exit 0  Removed "/etc/systemd/system/systemd-pstore.service".
  systemd-run --quiet --collect --unit=acl-selinux-repro-timer --on-active=1s /usr/bin/true exit 0  
  -- containerd, through its socket
  ctr version                                                    exit 0  Client:
  ctr namespaces list                                            exit 0  NAME                  LABELS 
  ctr --namespace acl-selinux-repro-ctr image pull mcr.microsoft.com/azurelinux/busybox:1.36 exit 0  mcr.microsoft.com/azurelinux/busybox:1.3	fetching image content	
  ctr --namespace acl-selinux-repro-ctr run --rm mcr.microsoft.com/azurelinux/busybox:1.36 acl-selinux-repro-run true exit 0  
  ctr --namespace acl-selinux-repro-ctr image rm mcr.microsoft.com/azurelinux/busybox:1.36 exit 0  mcr.microsoft.com/azurelinux/busybox:1.36
  -- kernel and files
  modprobe dummy                                                 exit 0  
  ip link add aclrepro0 type dummy                               exit 0  
  ip link del aclrepro0                                          exit 0  
  nft add table inet acl_selinux_repro                           exit 0  
  nft delete table inet acl_selinux_repro                        exit 0  
  sysctl -w net.ipv4.ip_forward=1                                exit 0  net.ipv4.ip_forward = 1
  mount -t bpf bpf /run/acl-selinux-repro-bpf                    exit 0  
  mountpoint /run/acl-selinux-repro-bpf                          exit 0  /run/acl-selinux-repro-bpf is a mountpoint
  umount /run/acl-selinux-repro-bpf                              exit 0  
  udevadm info --query=property --name=/dev/vda                  exit 0  DEVPATH=/devices/pci0000:00/0000:00:03.0/virtio1/block/vda
  swapoff -a                                                     exit 0  
  restorecon -R /opt/acl-selinux-repro                           exit 0  
  machinectl remove acl-selinux-repro-img                        exit 0  
  -- the existing machine kube1
  machinectl show kube1 -p State --value                         exit 0  running
  machinectl --quiet shell kube1 /bin/sh -c echo shell-ok        exit 0  shell-ok
  systemd-run --machine=kube1 --pipe --wait --quiet cat /etc/os-release exit 0  NAME="Microsoft Azure Linux"
  machinectl enable kube1                                        exit 0  
  machinectl disable kube1                                       exit 0  Removed "/etc/systemd/system/machines.target.wants/systemd-nspawn@kube1.service".
  machinectl poweroff kube1                                      exit 0  
  machinectl start kube1                                         exit 0  
  machinectl terminate kube1                                     exit 0  
  machinectl start kube1                                         exit 0  
  -- a machine this container creates, acl-selinux-repro-m
  cp -dR --preserve=mode,ownership,timestamps,links /var/lib/machines/kube1 /var/lib/machines/acl-selinux-repro-m exit 0  
  its init label: system_u:object_r:var_lib_t:s0
  machinectl start acl-selinux-repro-m                           exit 0  
  new machine context: system_u:system_r:kernel_t:s0
  machinectl show acl-selinux-repro-m -p State --value           exit 0  running
  machinectl --quiet shell acl-selinux-repro-m /bin/sh -c echo shell-ok exit 0  shell-ok
  systemd-run --machine=acl-selinux-repro-m --pipe --wait --quiet cat /etc/os-release exit 0  NAME="Microsoft Azure Linux"
  machinectl enable acl-selinux-repro-m                          exit 0  Created symlink /etc/systemd/system/machines.target.wants/systemd-nspawn@acl-selinux-repro-m.service → /usr/lib/systemd/system/systemd-nspawn@.service.
  machinectl disable acl-selinux-repro-m                         exit 0  Removed "/etc/systemd/system/machines.target.wants/systemd-nspawn@acl-selinux-repro-m.service".
  machinectl poweroff acl-selinux-repro-m                        exit 0  
  machinectl start acl-selinux-repro-m                           exit 0  
  machinectl terminate acl-selinux-repro-m                       exit 0  
  machinectl start acl-selinux-repro-m                           exit 0  
  machinectl terminate acl-selinux-repro-m                       exit 0  
  machinectl remove acl-selinux-repro-m                          exit 0  
  SELinux denials: none
  restored the original policy (f94ebb51f413eb98)

== Workaround: native snapshotter, its directory relabeled usr_t
  new snapshot /var/lib/containerd/io.containerd.snapshotter.v1.native/snapshots/14: system_u:object_r:usr_t:s0
  container context: system_u:system_r:kernel_t:s0
  -- through dbus-daemon
  machinectl list --no-pager --no-legend                         exit 0  kube1 container systemd-nspawn azurelinux 3.0 -
  busctl --system --no-pager --no-legend list --acquired         exit 0  org.freedesktop.DBus        1 systemd         root            -    init.scope               - -
  loginctl list-sessions --no-pager --no-legend                  exit 0  144 500 core - - active no -
  hostnamectl --static                                           exit 0  acl-flex-proto
  resolvectl domain                                              exit 0  Global:
  -- units
  systemctl is-system-running                                    exit 0  running
  acl-selinux-repro.service label: system_u:object_r:unlabeled_t:s0
  systemctl daemon-reload                                        exit 0  
  systemctl enable acl-selinux-repro.service                     exit 0  Created symlink /etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service → /etc/systemd/system/acl-selinux-repro.service.
  systemctl is-enabled acl-selinux-repro.service                 exit 0  enabled
  systemctl start acl-selinux-repro.service                      exit 0  
  systemctl restart acl-selinux-repro.service                    exit 0  
  systemctl stop acl-selinux-repro.service                       exit 0  
  systemctl reset-failed acl-selinux-repro.service               exit 1  Failed to reset failed state of unit acl-selinux-repro.service: Access denied
  systemctl disable acl-selinux-repro.service                    exit 0  Removed "/etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service".
  systemctl enable --now acl-selinux-repro.service               exit 0  Created symlink /etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service → /etc/systemd/system/acl-selinux-repro.service.
  systemctl disable --now acl-selinux-repro.service              exit 0  Removed "/etc/systemd/system/multi-user.target.wants/acl-selinux-repro.service".
  systemctl mask --now systemd-pstore.service                    exit 0  Created symlink /etc/systemd/system/systemd-pstore.service → /dev/null.
  systemctl unmask systemd-pstore.service                        exit 0  Removed "/etc/systemd/system/systemd-pstore.service".
  systemd-run --quiet --collect --unit=acl-selinux-repro-timer --on-active=1s /usr/bin/true exit 0  
  -- containerd, through its socket
  ctr version                                                    exit 0  Client:
  ctr namespaces list                                            exit 0  NAME                  LABELS 
  ctr --namespace acl-selinux-repro-ctr image pull mcr.microsoft.com/azurelinux/busybox:1.36 exit 0  mcr.microsoft.com/azurelinux/busybox:1.3	fetching image content	
  ctr --namespace acl-selinux-repro-ctr run --rm mcr.microsoft.com/azurelinux/busybox:1.36 acl-selinux-repro-run true exit 0  
  ctr --namespace acl-selinux-repro-ctr image rm mcr.microsoft.com/azurelinux/busybox:1.36 exit 0  mcr.microsoft.com/azurelinux/busybox:1.36
  -- kernel and files
  modprobe dummy                                                 exit 0  
  ip link add aclrepro0 type dummy                               exit 0  
  ip link del aclrepro0                                          exit 0  
  nft add table inet acl_selinux_repro                           exit 0  
  nft delete table inet acl_selinux_repro                        exit 0  
  sysctl -w net.ipv4.ip_forward=1                                exit 0  net.ipv4.ip_forward = 1
  mount -t bpf bpf /run/acl-selinux-repro-bpf                    exit 0  
  mountpoint /run/acl-selinux-repro-bpf                          exit 0  /run/acl-selinux-repro-bpf is a mountpoint
  umount /run/acl-selinux-repro-bpf                              exit 0  
  udevadm info --query=property --name=/dev/vda                  exit 0  DEVPATH=/devices/pci0000:00/0000:00:03.0/virtio1/block/vda
  swapoff -a                                                     exit 0  
  restorecon -R /opt/acl-selinux-repro                           exit 0  
  machinectl remove acl-selinux-repro-img                        exit 0  
  -- the existing machine kube1
  machinectl show kube1 -p State --value                         exit 0  running
  machinectl --quiet shell kube1 /bin/sh -c echo shell-ok        exit 0  shell-ok
  systemd-run --machine=kube1 --pipe --wait --quiet cat /etc/os-release exit 0  NAME="Microsoft Azure Linux"
  machinectl enable kube1                                        exit 0  
  machinectl disable kube1                                       exit 0  Removed "/etc/systemd/system/machines.target.wants/systemd-nspawn@kube1.service".
  machinectl poweroff kube1                                      exit 0  
  machinectl start kube1                                         exit 0  
  machinectl terminate kube1                                     exit 0  
  machinectl start kube1                                         exit 0  
  -- a machine this container creates, acl-selinux-repro-m
  cp -dR --preserve=mode,ownership,timestamps,links /var/lib/machines/kube1 /var/lib/machines/acl-selinux-repro-m exit 0  
  its init label: system_u:object_r:container_var_lib_t:s0
  machinectl start acl-selinux-repro-m                           exit 0  
  new machine context: system_u:system_r:spc_t:s0
  machinectl show acl-selinux-repro-m -p State --value           exit 0  running
  machinectl --quiet shell acl-selinux-repro-m /bin/sh -c echo shell-ok exit 0  shell-ok
  systemd-run --machine=acl-selinux-repro-m --pipe --wait --quiet cat /etc/os-release exit 0  NAME="Microsoft Azure Linux"
  machinectl enable acl-selinux-repro-m                          exit 0  Created symlink /etc/systemd/system/machines.target.wants/systemd-nspawn@acl-selinux-repro-m.service → /usr/lib/systemd/system/systemd-nspawn@.service.
  machinectl disable acl-selinux-repro-m                         exit 0  Removed "/etc/systemd/system/machines.target.wants/systemd-nspawn@acl-selinux-repro-m.service".
  machinectl poweroff acl-selinux-repro-m                        exit 0  
  machinectl start acl-selinux-repro-m                           exit 0  
  machinectl terminate acl-selinux-repro-m                       exit 0  
  machinectl start acl-selinux-repro-m                           exit 0  
  machinectl terminate acl-selinux-repro-m                       exit 0  
  machinectl remove acl-selinux-repro-m                          exit 0  
  SELinux denials:       1 { reload } for auid=n/a gid=0 path="/etc/systemd/system/acl-selinux-repro.service" cmdline="" function="bus_unit_method_reset_failed" scontext=system_u:system_r:kernel_t:s0 tcontext=system_u:object_r:unlabeled_t:s0 tclass=service permissive=0

== Summary
PROBE                                    CONTEXT   NEW-MACHINE FAILED    DENIALS
current policy                           spc_t     -           16/49     dbus:send_msg=23
policy candidate-dbus.policy             spc_t     kernel_t    0/59      none
workaround (native snapshotter, usr_t)   kernel_t  spc_t       1/59      service:reload=1

candidates and results: /tmp/opencode/aclc/verify
```

The `AddMatch` denials are the same refusal, for the match rules some clients
send right after `Hello`, without waiting for its reply.
