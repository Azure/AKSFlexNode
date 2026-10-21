# YurtHub agent integration

The agent runs OpenYurt's YurtHub ("Hub") in each Flex worker so kubelet can keep
running from a local cache while the node is disconnected from the API server.
AKS RP sends disconnect-tolerance (DT) intent; the agent enables Hub when it is
true and disables it when it is false or omitted. Default-on with customer
opt-out is later scope.

| Owner | Responsibility |
| --- | --- |
| Dalec / PMC | Native Hub packages and publication. |
| Flex agent release | Embeds the pinned Hub binaries. |
| Flex host agent | Worker service and config, kubelet routing, cache/PKI, Node metadata, lifecycle. |
| RP / ENO | DT intent, the yurt-manager chart and Hub's cluster prerequisites. |

One RP toggle drives both the manager and the agent's intent. The agent follows
explicit intent, not manager presence, so disable must work after the manager is gone.

## Intent

| Path | Field | Use |
| --- | --- | --- |
| Machine GET | `properties.flexNodesProfile.disconnectTolerance.enabled` | Drives enable/disable on every successful read. |
| `listBootstrapData` | `node.disconnectTolerance.enabled` | Merged into `config.json`; reserved for the first-Ready gate follow-up. |

- **Omitted means false**, including responses from an older RP. An RP rollback
  past the field disables DT on capable agents.
- **A failed read changes nothing**; local state is kept until a successful read.
- **Malformed values** (wrong shape or `null`) leave intent unset with a warning;
  the Machine read itself still succeeds.
- RP resolves the value per read without changing the Machine ETag, so it is
  checked on every read, independently of SettingsVersion.
- The current SDK model doesn't include `flexNodesProfile`, so the Machine clients
  decode it from the raw response (`pkg/aksmachine/flex_nodes_profile.go`).

**API version:** both fields are only returned from `2026-10-02-preview`, which ARM
doesn't accept yet. Until then Machine calls use the SDK default and bootstrap uses
`2026-05-02-preview`, so DT reads as false. Opt in with
`agent.machineClient.apiVersion` and `--bootstrap-data-api-version`.
Pending: make `2026-10-02-preview` the default once ARM accepts it, and read the
typed SDK field once it exists.

## Hub delivery

`pkg/yurthub/packages.json` pins each Dalec package on `packages.microsoft.com`
(URL, package SHA-256, binary SHA-256) per worker image: Ubuntu 24.04, Ubuntu 26.04
and Azure Linux 3, amd64. When DT is enabled, Prepare matches the **worker image's**
`/etc/os-release`, downloads that package, verifies both checksums, extracts
`/usr/bin/yurthub` and installs it at `/usr/local/bin/aks-flex-yurthub`, so it never
replaces a `yurthub` shipped by the image. It skips the download when the installed
binary already matches the pin, so Hub versions follow the agent's pins.

Joining never needs PMC; enabling Hub does. Hosts need outbound HTTPS to
`packages.microsoft.com`; behind a proxy, set `HTTPS_PROXY` in the agent's service
environment. The download is bounded to 3 minutes. If it fails or the checksums
don't match, Prepare fails and the adapter retries on later reads while kubelet stays
on the direct API. On an enabled Hub, a failed download for a new pin keeps the
running binary. Unsupported images or architectures, and exec-credential-only joins
(SP, MSI, Arc; Hub needs a kubelet client certificate) log a warning instead,
re-checked at most every 15 minutes.

**Alternative: embed Hub in the agent.** Release builds could carry the pinned
binaries (build tag plus a fetch step in the release job). That removes the PMC
egress requirement but grows the agent from about 57 MB to 119 MB, because the
Dalec builds are per-distro (dynamically linked).

Every worker gets a dormant, disabled `aks-flex-yurthub.service` and
`/var/lib/yurthub/{cache,pki}` (0700) during provisioning. Worker-rootfs writes use
root-confined file operations so image symlinks can't redirect them onto the host.

## Node condition

The agent reports DT state as a `DisconnectTolerance` Node condition (merged by
type, so kubelet's and NPD's conditions are kept). The agent compares its result
with the Node's current condition on each Machine read and writes when they differ,
so external edits are repaired and transition times survive agent restarts. An
unchanged condition's `lastHeartbeatTime` is refreshed once per Machine poll
interval (default 10 minutes), including while a failed enable waits to retry, so
an older heartbeat means the agent isn't updating it. Every node running this agent shows it, `Disabled` by default:

| Status | Reason | Meaning |
| --- | --- | --- |
| True | `Enabled` | Hub is serving kubelet. |
| True | `Degraded` | Kubelet is routed through Hub, but Hub isn't ready or couldn't be updated. |
| False | `Disabled` | DT is off. |
| False | `PackageDownloadFailed` | No valid Hub package could be downloaded; kubelet uses the API server directly. |
| False | `Unsupported` | No Hub package for this worker image or join type. |
| Unknown | `EnableFailed` / `DisableFailed` | Another error. |

Messages are short and fixed per reason; failure messages link to troubleshooting
steps (aka.ms link pending). Error details, such as the download URL and cause, are
only in the agent log (`journalctl -u aks-flex-node-agent`).

It is written with the daemon's node credential, which may patch its own status. A
failed write is logged and retried on the next read.

Known limits: the condition is only re-evaluated on Machine reads, so it can lag
by up to the poll interval or the Hub retry delay (up to 15 minutes); it isn't
updated while Machine reads fail; `Enabled` checks Hub readiness, not that kubelet
is still routed through Hub; and during a repave it keeps the old worker's value
until the new worker's first read.

## Transitions

`pkg/daemon/hub_adapter.go` maps each Machine read to a per-worker transition
engine (`hub_transition.go`). Its journal lives in the worker rootfs
(`/var/lib/aks-flex-node/hub-transition.json`), so repave and reset discard it with
the worker. Acceptance is journaled before any step and each step checkpoints the
next, so every step is idempotent and a crash resumes where it left off. An initial
false with no journal is a no-op, so upgrading the agent doesn't touch existing
nodes.

| Step | Effect |
| --- | --- |
| Prepare | Download and install the binary, add config links and `/etc/aks-flex-node/yurthub.env`; start Hub and wait up to 2 minutes for `readyz`. |
| Route | Check forwarding through Hub, then point kubelet at Hub through a Flex-owned drop-in and restart kubelet. |
| Verify | Confirm the live kubelet uses Hub, then initialize Node metadata once. |
| Converge | On later reads while enabled: repair the binary, arguments or unit and restart Hub only (never kubelet) when they changed. |
| Restore | Route kubelet back to the direct API and confirm it before stopping Hub. |
| Stop | Stop and disable Hub and delete its cache. PKI and binary stay, so re-enable needs no new serving CSR. |

Hub steps run on the daemon's existing serialized reconcile queue, never alongside
repave, reset or agent upgrade, and after the Machine status report, so they never
delay that report (they can delay later queued work; see Open items). Failures are logged rather than returned, so the normal Machine poll
continues; the adapter retries after 1 minute, doubling to 15.

| Event | Behavior |
| --- | --- |
| New join | Kubelet joins direct; the first Machine read enables Hub if intent is true. |
| Intent change | Applied on the next Machine read (10-minute poll plus jitter, or a Node event), without repave. |
| Offline | Nothing changes until the next successful read. |
| Worker/host restart | Hub and the kubelet drop-in are enabled units; cache and PKI persist. |
| Repave | New worker and journal; Hub is enabled again if intent is true. |

## Decisions

- **Loopback-only listeners** (`--node-ip=127.0.0.1 --enable-dummy-if=false`). Hub
  forwards unauthenticated requests using the node's kubelet certificate; with
  upstream defaults, pods on the node and other nodes could read the node's Node
  object with no credentials.
- **Filters off; Hub serves kubelet only** (`--enable-resource-filter=false`). With
  filters on, pods started under Hub keep calling Hub and lose API access when it is
  disabled. Cost: API-calling pods get no Hub cache while offline.
- **Node metadata:** NodePool name is the Flex agent pool name (no NodePool objects
  needed with filters off). Autonomy is set only as
  `node.openyurt.io/autonomy-duration: 0s`, and only when the Node has no autonomy
  annotation; the legacy annotation is never added because it overrides customer
  durations. `kubernetes.azure.com/flex-yurthub-initialized` marks completion, and
  patches never overwrite customer values.
- **Disable** deletes Hub's cache, which holds plaintext Secrets for pods on the node,
  and leaves Node metadata and manager-set Pod tolerations in place.

## Cluster prerequisites (yurt-manager chart)

- **Approval of Hub's kubelet-serving CSR.** The upstream `csrapprover` works for
  the internal POC; customer preview needs a scoped replacement. If approval is
  missing, Prepare times out and retries on later reads.
- **ConfigMap `get/list/watch` in `kube-system`** for the node, because Hub isn't
  ready until its `yurt-hub-cfg` watch starts.

NodePools, `yurt-hub-cfg` itself and the upstream Hub ClusterRole aren't needed.

## Open items

- **Readiness gate:** Hub is enabled after join, not before first Ready.
- **Local access:** host processes and hostNetwork pods can reach Hub's loopback
  listeners and act as the node; kubelet-to-Hub traffic is plaintext on loopback. A
  process that binds Hub's ports first could also stand in for Hub when the agent
  routes kubelet; checking the listener's owner before Route is a possible hardening.
- **Secrets at rest:** Hub's cache holds Secrets in plaintext while enabled.
- **Disable ordering:** RP should keep the manager until agents report Hub disabled;
  RP could read the Node condition for that.
- **Agent downgrade:** an agent without Hub support can't disable Hub; turn DT off
  first.
- **API-version rollback:** older API versions omit the field, which reads as false,
  so switching a node back from `2026-10-02-preview` disables Hub.
- **Queue time:** one Hub attempt can hold the daemon queue for several minutes
  (3-minute download, 2-minute readiness wait, bounded Node calls); there's no
  overall cap.
- **Heartbeat cost:** each heartbeat write is a Node update, which also triggers a
  Machine read, so every node does one extra status patch and ARM read per poll.
- **Verify failures:** if Verify keeps failing after Route, kubelet stays on Hub; there
  is no automatic fallback.
- **Hashing cost:** each steady read hashes the installed Hub binary a few times;
  cache by size and modification time if it shows up.
- **Autonomy condition:** Hub's node condition reads only the legacy annotation, so
  it reports autonomy disabled.

## Validation

Unit tests cover staging, journal recovery at every step, intent decoding, package
download and extraction, each operation, Converge drift and Node condition
reporting against a fake worker and Node API. The extractor was also checked
against the real pinned PMC packages, and a live download installed the Azure Linux
3 package. Run `make check`.

Live runs on Ubuntu 24.04 workers validated enable and disable through production
operations, Node metadata preservation across disable/re-enable, Pod tolerations
generated by the manager, and Converge upgrading a previously enabled Hub. A later
integrated run went off→on→off→on from real RP Machine reads. Those runs predate the
latest field rename, the switch from embedded to downloaded Hub binaries and the Node
condition, so a live rerun is due.

Not yet validated: automated initial join and cache sync, offline worker or host
reboot, CNI/DNS/application recovery, credential expiry, repave, and coordinated
product disable.
