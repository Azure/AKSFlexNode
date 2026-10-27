# TODO: ACL support with the agent in a container

The remaining work to run the AKS Flex Node agent as a privileged container on
Azure Container Linux (ACL), with the prototype in `hack/acl-container`. A
living document: add items as they come up, and delete them once they're done;
git history keeps the record.

Approach: build on `main`, and make small, targeted changes so the agent in a
container behaves the same as `main`'s agent on a host. `main` has #323, which
runs the agent on an ACL host, under `/opt/unbounded/agent`; in the container
the agent skips its host-side steps. Host mode is interim, until this branch
lands.

## Before merging

- [ ] ACL's SELinux rule, and its labels on containerd's files, reach the
      image AKS will use. ACL 3.0.20261026 has both, but from a development
      gallery, and excluded from its latest. Then move the harness's default
      to that image, and link ACL's tracking item.
- [ ] Document the container mode in `docs/usage/azure-container-linux.md`,
      which describes host mode, as the way to run on ACL, and host mode as
      interim.

## ACL's SELinux fix

ACL 3.0.20261019 (build 1229918) ships `allow spc_t kernel_t:dbus send_msg;`,
and `check` passes without the workaround, whose commit is reverted. ACL
3.0.20261026 (build 1236866), gallery image `ACLDEVEL/acl-1es-eval-kvm` version
3.20261027.1, also labels `/var/lib/containerd`, so the agent's containers run
as `spc_t` as it ships, and `check` passes on it with no denials. ACL tests its
builds for the labels now. The harness boots that build by default, and expects
`spc_t`.

- [ ] Early adopters need access to that build's gallery image, ACLDEVEL's
      `acl-1es-eval-kvm`, which is shared with particular tenants.
- [ ] Reprovision any development hosts set up with the workaround, or from
      a build before 3.0.20261026: Ignition runs once, so they keep the
      workaround, and earlier builds leave containerd's files unlabeled, where
      the agent's containers may not reset their unit's start limit.

Separately: ACL considered `allow kernel_t unlabeled_t:service reload;`, so
host root could run `systemctl reset-failed`; neither 3.0.20261019 nor
3.0.20261026 adds it. ACL will allow it in a future image, which #385, host
mode's start limit, waits for. When it ships, check it in `hack/acl` and close
#385. The agent's containers don't need it: as `spc_t`, they may already.

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
`scripts/aks-flex-node-agent-container.bu`, installs units that run containers of the agent's image,
and a settings file that names the image. `aks-flex-node-bootstrap.service`
pulls the image and runs `main`'s `bootstrap.sh` from it, whose `start` installs
`aks-flex-node-agent.service`, which runs the daemon in a container, as `start`
installs it on a host. Reset removes that unit, after which nothing of the agent
runs, and `aks-flex-node-reset.service` runs `aks-flex-node reset` in a container
of its own. The machines' hooks start the agent's nspawn lifecycle units, as on a
host, which run its phases in short-lived containers of its image. The units
give each container only the host's root: its entrypoint mounts the host paths
the agent uses at their own paths, and the host's tools run through
`host-exec`.

`AgentUpgrade` names an image in a container, rather than a release archive,
as an `oci://` `downloadURL` with its digest as `sha256`: the agent pulls it,
checks that its agent runs, and selects it in
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

- [ ] GPU hosts in the container: the agent's containers see the host's
      `/dev`, `/sys`, and `/etc/os-release`, but the image's `/usr/bin` and
      `ldconfig`. Unbounded's NVIDIA resolution looks for `nvidia-smi` in
      `/usr/bin` and the driver's libraries through `ldconfig -p`, so on a GPU
      host it would find the image's, none, both when provisioning a machine
      and in its hooks. Untested. Running `ldconfig` through `host-exec` would
      likely cover the libraries; the binaries need the host's paths.
- [ ] Swap in the container: `start`'s `DisableSwap`, unbounded's, runs
      `swapoff -a` and masks swap units through the host's tools, but
      comments out swap entries in `/etc/fstab` from Go, which in the
      container edits the image's `/etc/fstab`, not the host's. Swap in the
      host's `fstab` would come back after a reboot, and kubelet, with
      unbounded's default `failSwapOn`, wouldn't start. A mount can't fix it:
      the edit replaces the file. ACL has no swap: no `/etc/fstab` at
      runtime, no swap or zram units, and the Azure agent's
      `ResourceDisk.EnableSwap=n`, in 3.20261019.3 and 3.20261021.1. So it
      matters only if an operator's Ignition config adds swap to `fstab`.
      Fix it with a task that edits `/host/etc/fstab` in the container, or an
      `fstab` path option upstream. Unbounded doesn't mask `dev-disk-by*`
      swap units, or turn off the Azure agent's swap, in either mode.
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
      cluster, with pods in its machines.

## Container launch and image

- [ ] Offline and first boot: the bootstrap unit pulls the image at every
      bootstrap, and when the pull fails runs the image if it's there, as the
      install's is after a reset. A first boot has none, so a node can't join
      without the registry. Ask ACL to pre-seed the agent's image in its
      images, as 3.0.20261019 does the Arc agent's, and labeled; it would then
      join offline.

## Validation and housekeeping

- [ ] Run on an Azure VM from the gallery image, with Ignition as custom data.
      So far ACL has only run under QEMU. Join there with
      `--fetch-bootstrap-data`, with a managed identity and with a service
      principal, as `main`'s docs do. Those keep the fetched bootstrap token,
      which takes precedence, so the agent's own client doesn't run its
      credential plugin. Only a config with an identity and no token does,
      which unit tests cover.
- [ ] Once this branch lands, retire host mode on ACL, which #323 adds: its
      ACL docs, its Butane config, and `hack/acl`. A host-mode node moves to
      the container by reprovisioning.

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
