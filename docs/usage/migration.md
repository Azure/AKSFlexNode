# Migrating to `/opt/unbounded/agent`

Earlier releases installed the agent's binaries and helpers under `/usr/local`. This release installs them under `/opt/unbounded/agent`; see [Where the agent is installed](operations.md#where-the-agent-is-installed). This page describes what changed, and how a host an earlier release installed moves.

## What changed

- The binaries and helpers moved from `/usr/local` to `/opt/unbounded/agent`. The config, state, logs, and systemd units didn't move.
- `aks-flex-node` is no longer on the default `PATH`, so scripts and runbooks that run it by name need updating; see [Where the agent is installed](operations.md#where-the-agent-is-installed).

## How an existing host moves

A host an earlier release installed reaches `/opt/unbounded/agent` in two stages:

1. **Linked.** The first command of a newer release that changes the host, such as the agent after an AgentUpgrade, links `/opt/unbounded/agent` to `/usr/local`, creating `/opt/unbounded` if needed. The files stay where they are and every path the units name is unchanged, so the host can still be returned to the earlier release, which is still the last-good binary.
2. **Moved.** Once neither the current nor the last-good binary is from an earlier release, the agent copies the files into a real `/opt/unbounded/agent`, runs the copied binary, points the units at them, and restarts itself from there. The restarted agent then removes the files from `/usr/local`, along with `/usr/local/lib/aks-flex-node` once nothing else is in it. That is at the first agent start after the AgentUpgrade that pushes the earlier release out of last-good. To move a host that is not due another upgrade, apply the release it already runs as an AgentUpgrade.

Nothing under `/usr/local` is removed until the agent runs from the copy, so a move that is interrupted, or a restart that fails, leaves both copies in place, and the next agent start finishes it. An agent that has queued its restart, or that finds the files moved under it by a move that then failed, takes no work; if it is not restarted within two minutes it exits, and systemd starts it again.

A host where the agent cannot run from `/opt/unbounded/agent`, because `/opt` is mounted `noexec` or for another reason, stays linked. The agent logs why, removes any copy it made, points the units back at `/usr/local` if an interrupted move had already pointed them at the copy, and tries again at its next start.

## Reset and uninstall

`aks-flex-node reset` removes the helpers under both locations and keeps the binaries. The uninstall script removes the binaries from both, then `/opt/unbounded/agent` itself, as the link to `/usr/local` or an emptied directory, and an interrupted move's `/opt/unbounded/agent.staging`, at any stage. It leaves `/opt/unbounded`, and a link you made at `/opt/unbounded/agent` to somewhere else, as the agent does.

## Earlier releases

On a host that no longer runs anything from `/usr/local`, an AgentUpgrade or a direct activation first removes the copy the install scripts left in `/usr/local/bin`, and refuses to switch the binary while `/usr/local/bin/aks-flex-node` still leads to an executable, such as through a link you made there. An earlier release would take that executable as its own binary and install itself under `/usr/local` beside the agent under `/opt/unbounded/agent`, which the agent refuses to run on. With nothing there, an AgentUpgrade to an earlier release fails as any upgrade whose daemon cannot start does: that release looks for its files under `/usr/local`, and the agent restores the last-good binary.

To go back to an earlier release on a host installed under a real `/opt/unbounded/agent`, uninstall the agent first, then install the earlier release with that release's own install script. The install scripts of this release choose `/opt/unbounded/agent` on such a host, where an earlier release doesn't look for itself.
