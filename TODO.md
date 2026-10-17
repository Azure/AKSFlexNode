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
with the same config and commands:

- [ ] `start`: preflight, Machine registration, the nspawn worker, and the
      daemon
- [ ] Daemon reconciliation of the Machine goal state
- [ ] Repave, alternating kube1 and kube2
- [ ] `NodeReboot` and `AgentReset` MachineOperations
- [ ] `reset`
- [ ] `AgentUpgrade`, which becomes a new image (see below)

## Run the real agent in the container

The image carries `aks-flex-node` from `main`, but the entrypoint only checks
its access to the host and idles.

- [ ] Host OS detection: in the container, `/etc/os-release` is the image's
      (Azure Linux 3), not the host's. The agent has to read the host's,
      through `/host`, or be told what it is.
- [ ] unbounded version: `main` pins v0.8.0, which has no ACL detection. Stay
      on it unless a specific fix needs a newer one, and bump it in a commit
      of its own.
- [ ] Host paths: the agent writes `/etc/systemd/system`,
      `/etc/systemd/nspawn`, `/var/lib/machines`, `/etc/aks-flex-node`,
      `/var/log/aks-flex-node`, and its install directory as absolute paths.
      Prefer bind-mounting each into the container at the same path, which
      needs no change to the agent.
- [ ] Host commands: the image needs the `ip`, `nft`, `sysctl` and `modprobe`
      the agent runs, at versions that suit the host kernel. `systemctl`,
      `machinectl`, `systemd-run` and `journalctl` already run the host's
      copies through `host-exec`.
- [ ] Self-installation: add a small switch that stops the agent copying
      itself onto the host, writing `aks-flex-node-agent.service` and its
      recovery unit, and doing blue/green binary upgrades. The image and
      `aks-flex-node-container.service` replace them, and an upgrade is a new
      image.
- [ ] Recovery: replace `aks-flex-node-recovery.sh`. Recovery triggered by the
      container's unit failing can't run in that container. Run it as a
      separate container, which works in `spc_t`. If it's a host script
      instead, it runs as `kernel_t` and can't `reset-failed`, so it needs to
      wait out the unit's start limit and retry.
- [ ] nspawn lifecycle hooks: host systemd runs the agent's lifecycle helper
      around every machine start, from the host's filesystem. Either the
      image installs it on the host or the hooks move into the agent.
- [ ] Bootstrap: provide what `main`'s `scripts/bootstrap.sh` does,
      `/etc/aks-flex-node/config.json` and `aks-flex-node start`, from
      Ignition and the container instead of a script run on the host.
- [ ] Reset and uninstall: remove the container's unit and image instead of
      the agent's units and binaries.
- [ ] End-to-end: join a cluster from the container and run a pod. `main`'s
      e2e joins Ubuntu VMs to AKS in Azure; decide whether to add ACL VMs to
      it, or a local cluster to the harness. Under SELinux, machines created
      by the container run as `kernel_t` rather than `spc_t`; pods in them
      are untested.

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
- [ ] `hack/acl-container/README.md` and `DESIGN.md` still describe the
      workaround inline in the agent's unit, and still refer to PR #323,
      including `DESIGN.md` pointing to its host-root work as a starting
      point. Update them when the docs are next revised.
- [ ] Consider CI for `hack/acl-container`: ShellCheck, Python compile, and
      Butane `--strict` rendering.
