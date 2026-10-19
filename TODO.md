# TODO: ACL support with the agent in a container

The remaining work to run the AKS Flex Node agent as a privileged container on
Azure Container Linux (ACL), with the prototype in `hack/acl-container`. A
living document: add items as they come up, and delete them once they're done;
git history keeps the record.

Approach: build on `main`, and make small, targeted changes so the agent in a
container behaves the same as `main`'s agent on a host. Don't build on PR #323;
borrow from it only where that's simpler than redoing the work.

## Remove the SELinux workaround

Blocked on ACL. The workaround is the commit "Add an opt-in SELinux workaround
for the agent container" (c1a4528 when written), and nothing else depends on it.
See `hack/acl-container/SELINUX.md` for the finding.

- [ ] ACL ships `allow spc_t kernel_t:dbus send_msg;`. Record the ACL build and
      gallery image version here, and link their tracking item.
- [ ] Confirm the fix on that build: `hack/acl-container/selinux-repro.sh` with
      `MACHINE=` set reports 0 failures and no denials for the current policy.
- [ ] `hack/acl-container/harness.py up` without `--selinux-workaround`, then
      `check`: the container runs as `spc_t` on the default snapshotter, with
      every check passing.
- [ ] Revert the workaround commit.
- [ ] Reprovision any development hosts set up with the workaround: Ignition
      runs once, so they keep it until reprovisioned.
- [ ] If ACL labels `/usr` instead of, or as well as, adding the rule, rerun
      `hack/acl-container/selinux-verify.sh`, since host processes then leave
      `kernel_t`.

Separately, not needed by Flex: ACL is considering
`allow kernel_t unlabeled_t:service reload;`, so host root can run
`systemctl reset-failed`. Note their decision here.

## Parity with `main`

The agent in a container must do what `main`'s agent does on an Ubuntu host,
with the same config and commands. Bootstrap, `start`, the daemon's
reconciliation of the Machine goal state, repave, the `NodeReboot` and
`AgentReset` MachineOperations, the deletion signal, and `reset` work:
`harness.py up` bootstraps the VM into a kind cluster through `main`'s in-cluster
controller, and `check` runs a pod on it, repaves it from kube1 to kube2 as
`main`'s e2e does, restarts its machine, reboots it, and resets it in each of
the three ways, bootstrapping it again after each.

- [ ] `AgentUpgrade`, which becomes a new image (see below)

## Run the real agent in the container

Nothing of the agent runs on the host itself. The host's provisioning,
`agent-container.bu`, installs units that run containers of the agent's image,
and a settings file that names the image. `aks-flex-node-bootstrap.service`
pulls the image and runs `main`'s `bootstrap.sh` from it, whose `start` installs
`aks-flex-node-agent.service`, which runs the daemon in a container, as `start`
installs it on a host. Reset removes that unit, after which nothing of the agent
runs, and `aks-flex-node-reset.service` runs `aks-flex-node reset` in a container
of its own. The units give each container only the host's root: its entrypoint
mounts the host paths the agent uses at their own paths, and the host's tools
run through `host-exec`.

- [ ] nspawn lifecycle helper, the one binary of the agent left on the host.
      Each machine's units run `/usr/local/bin/unbounded-agent-nspawn-lifecycle`
      at every start of the machine: `pre-start` rewrites its `.nspawn` file and
      `post-start` rewires NVIDIA devices. Unbounded copies the agent's binary
      there when it provisions a machine, which is why `/usr/local`, read-only
      on ACL, has a writable overlay; its newer releases only move the copy to
      `/opt/unbounded/agent/bin`. Change unbounded so its caller can supply the
      hooks' command and skip the copy, bump it in a commit of its own, and run
      both hooks in short-lived containers of the agent's image through
      `ctr run`, so machines still start without the daemon. Then remove the
      overlay.
- [ ] unbounded version: `main` pins v0.8.0, which has no ACL detection but
      needs none so far. Stay on it until the lifecycle helper, or another
      specific fix, needs a newer one, and bump it in a commit of its own.
- [ ] Upgrades: turn `AgentUpgrade`'s blue/green binary switching into an
      image update, with rollback. On a host the daemon also rewrites its own
      unit at every start; in a container it doesn't yet, so a newer image
      can't change `aks-flex-node-agent.service` without a new bootstrap.
- [ ] Recovery: replace `aks-flex-node-recovery.sh`. Recovery triggered by the
      container's unit failing can't run in that container. Run it as a
      separate container, which works in `spc_t`. If it's a host script
      instead, it runs as `kernel_t` and can't `reset-failed`, so it needs to
      wait out the unit's start limit and retry.
- [ ] Bootstrap: the bootstrap unit runs `bootstrap.sh` with the inputs in
      `/etc/aks-flex-node/bootstrap.env`, which the harness writes through
      Ignition, with a base config holding a bootstrap token. Decide how
      operators deliver those inputs in their Butane, and try
      `--fetch-bootstrap-data`, which fetches bootstrap data from the AKS RP
      with the VM's identity, from the container.
- [ ] Uninstall: on ACL the install is the Ignition config, which runs only
      at first boot, so uninstalling means reprovisioning without the agent's
      Butane config. Say so in the docs. In a container, reset empties the
      agent's directories rather than removing them: they're mount points
      there.
- [ ] `main`: when the daemon stops its own unit, for `AgentReset` or the
      deletion signal, `systemctl stop` ends it partway through
      `UninstallService`. The unit file is removed, but disabling it and
      reloading systemd fail, which leaves a dangling `.wants` link, on a host
      as in a container. Raise it with `main`: disable, remove, and reload
      before stopping.
- [ ] Preflight: in a container, `host-packages` checks the image's package
      database, and `containerd-active` warns that bootstrap will disable
      containerd, which Flex never does (on `main` either). Drop or reword
      both in a container.
- [ ] End-to-end on AKS: `main`'s e2e joins Ubuntu VMs to AKS in Azure;
      decide whether to add ACL VMs to it. Locally the harness joins a kind
      cluster, so far only with the SELinux workaround, where machines run as
      `spc_t`. Without it, machines created by the container run as
      `kernel_t`; pods in them are untested until ACL ships its fix.

## Container launch and image

- [ ] Cgroup placement: `ctr` puts the container at `/aks-flex/aks-flex-node`,
      outside its unit's cgroup, so systemd neither accounts for nor limits
      it. Consider a small launcher using the containerd client with the
      systemd cgroup driver, or `ctr run --cgroup` with a slice.
- [ ] Decide whether `ctr`, a debugging tool and the only container tool on
      ACL, stays the launcher.
- [ ] Pin the image by digest in the Ignition config, and sign and verify it.
- [ ] Private registries: pull credentials or a `hosts.toml`. MCR needs none.
- [ ] Offline and first boot: deliver the image with the OS, as a system
      extension or through `ctr image import` from Ignition, so the first
      start doesn't need the registry.

## Validation and housekeeping

- [ ] Run on an Azure VM from the gallery image, with Ignition as custom data.
      So far ACL has only run under QEMU.
- [ ] Close PR #323: this branch builds on `main` instead.
- [ ] `hack/acl-container/README.md` and `DESIGN.md` describe the prototype's
      single container unit, which mounted the host's paths itself and idled.
      Now bootstrap and reset units run the image, `start` installs the
      agent's own unit, the entrypoint mounts the host's paths, and `up`
      always bootstraps the VM into a kind cluster. They also still describe
      the workaround inline in the agent's unit, and still refer to PR #323,
      including `DESIGN.md` pointing to its host-root work as a starting
      point. `DESIGN.md` also says `host-exec` always uses `chroot /host`;
      `mount`, `umount`, and `mountpoint` run in the host's mount namespace
      instead. `README.md` puts the image reference in
      `/etc/aks-flex-node/agent-container.env`, now
      `/etc/sysconfig/aks-flex-node-container`, out of reset's reach. Update
      them when the docs are next revised.
- [ ] Consider CI for `hack/acl-container`: ShellCheck, Python compile, and
      Butane `--strict` rendering.
