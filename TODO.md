# TODO: ACL support with the agent in a container

The remaining work to run the AKS Flex Node agent as a privileged container on
Azure Container Linux (ACL), with the prototype in `hack/acl-container`. A
living document: add items as they come up, and delete them once they're done;
git history keeps the record.

Approach: build on `main`, and make small, targeted changes so the agent in a
container behaves the same as `main`'s agent on a host. Don't build on PR #323;
borrow from it only where that's simpler than redoing the work.

## Before merging

- [ ] The preview image workflow, `.github/workflows/acl-preview-image.yml`,
      is temporary. It builds the agent's image on every push to this branch
      and publishes it to `ghcr.io/azure/aks-flex-node-agent-acl-preview` for
      early adopters. Before this branch merges, do one of these:
      - Formalize it: build the agent's image from `main` as part of the
        release, with the release's version tags, like the agent's other
        artifacts (its archives, and the controller image, which `release.yml`
        builds and publishes behind the `release` environment's approval).
      - Remove it: revert its commit.

      Either way, retire the preview package, and tell early adopters where
      the image moved, or that it's gone. A formalized image may also want to
      be reproducible, which the preview isn't, though its base images are
      pinned: `tdnf` installs whatever the repositories hold at build time, so
      every rebuild of a commit has a new digest. That would take pinning the
      packages, and a fixed time for the build, its files, and its labels.
- [ ] ACL's SELinux rule reaches the image AKS will use. ACL 3.0.20261019 has
      it, but from a development gallery, and excluded from its latest. Then
      move the harness's default to that image, and link ACL's tracking item.
- [ ] Agree on `AgentUpgrade`'s `image` parameter with AKS, which sends
      AgentUpgrades, including that it must be pinned by digest, as `main`
      requires `sha256` for an archive since #369.

## ACL's SELinux fix

ACL 3.0.20261019 (build 1229918), gallery image
`ACLDEVEL/acl-1es-eval-kvm` version 3.20261019.3, ships
`allow spc_t kernel_t:dbus send_msg;`. On it, `selinux-repro.sh` with
`MACHINE=` set reports 0 of 59 failures and no denials, and `check` passes
without the workaround, whose commit is reverted. The harness boots that build
by default. See `hack/acl-container/SELINUX.md`.

- [ ] ACL's next nightly labels `/var/lib/containerd`, which they said
      after our writeup of 2026-10-20 (SELINUX.md's question 5): 3.0.20261019
      builds it into the image, with an Arc agent image, and leaves it
      unlabeled, so every container containerd starts stays `kernel_t`, and
      the machines the agent creates run as `spc_t`. Labeled, the agent's
      containers run as `spc_t` and its machines as `kernel_t`. When the
      nightly is out:
      - Boot it in the harness as it ships, and run `check`: the agent's
        containers should run as `spc_t` without `up --label-containerd`.
      - Make it the harness's default, pinned in `aclimage.py`.
      - Keep 3.0.20261019 bootable, with `ACL_IMAGE_VERSION=3.20261019.3`,
        to run `check` where the agent's containers are `kernel_t` and may
        not `reset-failed` their unit. Upgrade recovery's fallback for that,
        starting the unit once its start limit has passed, stays, for any
        host whose policy denies it.
      - Then decide whether `--label-containerd` still earns its place.
- [ ] Early adopters need access to that build's gallery image, ACLDEVEL's
      `acl-1es-eval-kvm`, which is shared with particular tenants.
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
- [ ] `AgentUpgrade`'s restart counts toward the agent unit's start limit, 5
      starts in 300 s, on `main`'s hosts too. An upgrade past it is refused by
      systemd, rolled back by recovery, and reported failed, though the
      candidate never ran. It takes several upgrades in a few minutes, which
      AKS doesn't send, but `check --only upgrade` run twice did. Raised with
      `main` as #386: the daemon resets its unit's start count before it
      schedules the restart. Once it merges, rebase onto it, and do the same
      in the container's executor, best effort: `spc_t` may reset it, and
      `kernel_t`, as the agent's containers run where containerd's files are
      unlabeled, may not.
- [ ] An upgraded daemon rewrites its recovery unit from its own image as it
      starts, before it judges the upgrade, as `main`'s rewrites its recovery
      unit and script from its own binary on a host. So a candidate with a
      broken recovery unit breaks the recovery that would roll it back. Raised
      in review of PR #370. Raised with `main` as #387: the daemon keeps the
      last-good version's assets while an upgrade's signal exists, and writes
      its own once the upgrade is judged. Once it merges, rebase onto it, and
      have the container's convergence hold its units the same way.
- [ ] unbounded: preflight's `docker-active` and `kubelet-active` warn that
      the service's "state could not be determined" on any host where it
      isn't running, Ubuntu included. `systemctl is-active` exits non-zero
      for every state but active, 3 for inactive and 4 for no such unit,
      which the check takes for an error. Raised with unbounded as
      Azure/unbounded#962, which reads `systemctl show` instead; it comes with
      the next bump of unbounded.
- [ ] Bootstrap: the bootstrap unit runs `bootstrap.sh` with the inputs in
      `/etc/aks-flex-node/bootstrap.env`, and optionally an image in
      `/etc/aks-flex-node/agent-image.env`, which the harness writes through
      Ignition, with a base config holding a bootstrap token; README.md's "Try
      a preview" documents that. Decide how operators deliver those inputs.
      `--fetch-bootstrap-data`, which fetches bootstrap data from the AKS RP
      with the machine's identity, is untested from the container: with a
      managed identity it reaches Azure's metadata endpoint over the host's
      network, and a service principal's credential files work under
      `/etc/aks-flex-node` or `/run`, where `main`'s docs put them and the
      containers see them at their own paths. Arc is deferred, below.
- [ ] End-to-end on AKS: `main`'s e2e joins Ubuntu VMs to AKS in Azure;
      decide whether to add ACL VMs to it. Locally the harness joins a kind
      cluster, with pods in machines running as `spc_t` and as `kernel_t`.

## Container launch and image

- [ ] Offline and first boot: the bootstrap unit pulls the image at every
      bootstrap, and when the pull fails runs the image if it's there, as the
      install's is after a reset. A first boot has none, so a node can't join
      without the registry. Ask ACL to pre-seed the agent's image in its
      images, as 3.0.20261019 does the Arc agent's, and labeled; it would then
      join offline.

## Validation and housekeeping

- [ ] Run on an Azure VM from the gallery image, with Ignition as custom data.
      So far ACL has only run under QEMU.
- [ ] Close PR #323: this branch builds on `main` instead.
- [ ] Consider CI for `hack/acl-container`: ShellCheck, Python compile, and
      Butane `--strict` rendering.

## Deferred, until everything else is done

- [ ] Arc: `--fetch-bootstrap-data`, and the agent's Arc support, with Arc
      authentication, likely fail in the agent's containers. The Azure SDK
      recognizes an Arc machine by `IDENTITY_ENDPOINT` and `IMDS_ENDPOINT`, or
      by Arc's `/opt/azcmagent/bin/himds`, and the containers have neither.
      Arc's identity service answers with a key file under
      `/var/opt/azcmagent/tokens`, which they don't mount. The Arc preflight
      runs `azcmagent`, which they don't have. And ACL 3.0.20261019 ships the
      Arc agent as a preview container image, so Arc on ACL may differ from a
      host install. Start by learning how Arc is delivered on ACL; then pass
      the variables, mount the token directory, and give the containers
      `azcmagent`, or adapt the preflight.
- [ ] Private registries: `ctr` pulls anonymously, which MCR and the public
      preview package need. A private registry needs credentials, which `ctr`
      takes only as static ones (`--user`, or headers in a `hosts.toml`).

- [ ] Direct activation: `main`'s `agent-upgrade` command lets a provisioning
      system activate a binary it staged, without a MachineOperation: to join
      a node again with a newer agent, or to upgrade a running one out of band.
      The first is covered: bootstrap's inputs can name the image. For the
      second, a one-shot unit like the reset unit could run `agent-upgrade`
      with an image, read from a file, in a container of its own: pull and
      check it, select it, restart the agent's unit, and roll back if the new
      daemon doesn't come up. Build it only once someone needs it; AKS's own
      flows use MachineOperations.
