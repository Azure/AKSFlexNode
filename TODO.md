# TODO: ACL support with the agent in a container

The remaining work to run the AKS Flex Node agent as a privileged container on
Azure Container Linux (ACL), with the prototype in `hack/acl-container`. A
living document: add items as they come up, and delete them once they're done;
git history keeps the record.

Approach: build on `main`, and make small, targeted changes so the agent in a
container behaves the same as `main`'s agent on a host. Don't build on PR #323;
borrow from it only where that's simpler than redoing the work.

## ACL's SELinux fix

ACL 3.0.20261019 (build 1229918), gallery image
`ACLDEVEL/acl-1es-eval-kvm` version 3.20261019.3, ships
`allow spc_t kernel_t:dbus send_msg;`. On it, `selinux-repro.sh` with
`MACHINE=` set reports 0 of 59 failures and no denials, and `check` passes
without the workaround, whose commit is reverted. The harness boots that build
by default. See `hack/acl-container/SELINUX.md`.

- [ ] That build comes from a development gallery, and is excluded from its
      latest. Move the harness's default to the image AKS will use once the
      rule reaches it, and link ACL's tracking item.
- [ ] Raise with ACL: 3.0.20261019 builds `/var/lib/containerd` into the
      image, with an Arc agent image, and leaves it unlabeled, though its
      `file_contexts` labels it `container_var_lib_t`. So every container
      containerd starts stays `kernel_t`, and machines the agent creates run
      as `spc_t`; once labeled, the agent's containers run as `spc_t` and its
      machines as `kernel_t`. The agent passes `check` either way, but a
      relabel, or a build that labels it, changes the domains under running
      nodes. SELINUX.md's question 5.
- [ ] Reprovision any development hosts set up with the workaround: Ignition
      runs once, so they keep it until reprovisioned.
- [ ] If ACL labels `/usr` too, rerun `hack/acl-container/selinux-verify.sh`,
      since host processes then leave `kernel_t`.

Separately, not needed by Flex: ACL considered
`allow kernel_t unlabeled_t:service reload;`, so host root could run
`systemctl reset-failed`; 3.0.20261019 doesn't add it.

## Parity with `main`

The agent in a container must do what `main`'s agent does on an Ubuntu host,
with the same config and commands. Bootstrap, `start`, the daemon's
reconciliation of the Machine goal state, repave, the `NodeReboot`,
`AgentUpgrade`, and `AgentReset` MachineOperations, the deletion signal, and
`reset` work: `harness.py up` bootstraps the VM into a kind cluster through
`main`'s in-cluster controller, and `check` runs a pod on it, repaves it from
kube1 to kube2 as `main`'s e2e does, restarts its machine, upgrades its agent to
another image, to a broken one, which rolls back, and back, reboots it, and
resets it in each of the three ways, bootstrapping it again after each.

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

`AgentUpgrade` takes an `image` in a container, rather than a release archive:
the agent pulls it, checks that its agent runs, and selects it in
`/etc/aks-flex-node/agent-image.env`, which its unit reads, before restarting
on it. As on a host, the restarted daemon judges the upgrade and publishes the
result, and a recovery unit, here running the last good image, restores that
image if the new one fails. At every start, the daemon also rewrites its units
from its image, copies its binary into the active machine, and removes images
it won't run again. The bootstrap and reset units read the same selection, so
whatever provisions bootstrap's inputs can name the image to join with, as
`bootstrap.sh`'s inputs name the agent it downloads; reset removes it with the
rest of `/etc/aks-flex-node`, so without it, bootstrap runs the install's
image.

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
- [ ] `main`: a MachineOperation could run twice, as the daemon read it
      through the manager's cache, which could still have it in progress just
      after it finished: one `NodeReboot` rebooted the node twice. PR #374
      reads it from the API server instead, and this branch carries the same
      commit; drop it here once #374 merges and this branch is rebased.
- [ ] `AgentUpgrade`'s restart counts toward the agent unit's start limit, 5
      starts in 300 s, on `main`'s hosts too. An upgrade past it is refused by
      systemd, rolled back by recovery, and reported failed, though the
      candidate never ran. It takes several upgrades in a few minutes, which
      AKS doesn't send, but `check --only upgrade` run twice did. The daemon
      could reset its unit's failed state, which clears the count, before it
      schedules the restart: on a host it may, and in a container `spc_t` may,
      though `kernel_t`, as the agent's containers run where containerd's
      files are unlabeled, may not. Raise it with `main`.
- [ ] unbounded: preflight's `docker-active` and `kubelet-active` warn that
      the service's "state could not be determined" on any host where it
      isn't running, Ubuntu included. `systemctl is-active` exits non-zero
      for every state but active, 3 for inactive and 4 for no such unit,
      which the check takes for an error. Raise it with unbounded.
- [ ] `AgentUpgrade`'s `image` parameter is this branch's choice. AKS sends
      AgentUpgrades, so agree on it with them, including that it must be
      pinned by digest, as `main` requires `sha256` for an archive since #369.
- [ ] Bootstrap: the bootstrap unit runs `bootstrap.sh` with the inputs in
      `/etc/aks-flex-node/bootstrap.env`, and optionally an image in
      `/etc/aks-flex-node/agent-image.env`, which the harness writes through
      Ignition, with a base config holding a bootstrap token. Decide how
      operators deliver those inputs in their Butane, and try
      `--fetch-bootstrap-data`, which fetches bootstrap data from the AKS RP
      with the VM's identity, from the container.
- [ ] End-to-end on AKS: `main`'s e2e joins Ubuntu VMs to AKS in Azure;
      decide whether to add ACL VMs to it. Locally the harness joins a kind
      cluster, with pods in machines running as `spc_t` and as `kernel_t`.

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
- [ ] Consider CI for `hack/acl-container`: ShellCheck, Python compile, and
      Butane `--strict` rendering.

## Deferred, until everything else is done

- [ ] Direct activation: `main`'s `agent-upgrade` command lets a provisioning
      system activate a binary it staged, without a MachineOperation: to join
      a node again with a newer agent, or to upgrade a running one out of band.
      The first is covered: bootstrap's inputs can name the image. For the
      second, a one-shot unit like the reset unit could run `agent-upgrade`
      with an image, read from a file, in a container of its own: pull and
      check it, select it, restart the agent's unit, and roll back if the new
      daemon doesn't come up. Build it only once someone needs it; AKS's own
      flows use MachineOperations.
