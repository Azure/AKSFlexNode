"""The scenarios abupdate.py runs.

A scenario is a function of a Context, registered with @scenario. It drives the VM with the
Context's steps, which run their own checks: stage, finalize, commit, update (all three), reboot,
rollback, verify, and health_rollback. The step commands run the same steps, so a scenario and a
manual run of its steps check the same things. To add a scenario, add a function here; see README.md.

Scenarios run in the order they're defined here when `run all` or `test` runs them all.
"""

from __future__ import annotations

import dataclasses
import enum
from collections.abc import Callable
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from abupdate import Context


class Skip(Exception):
    """The image can't run a scenario or step: its Trident lacks a command, or rejects a setting
    the scenario needs. The scenario is reported as skipped rather than failed."""


class Needs(enum.Enum):
    """What a scenario needs of the VM before it starts. When the VM doesn't meet it, the scenario
    first runs the steps that do, as setup, unless run is given --no-setup."""

    NONE = "none"
    # The VM doesn't run the scenario's target image yet, so updating to it changes the image.
    OFF_TARGET = "off-target"
    # An A/B update was committed and not rolled back, and Trident is provisioned.
    COMMITTED_UPDATE = "committed-update"


@dataclasses.dataclass(frozen=True)
class Scenario:
    name: str
    description: str
    run: Callable[[Context], None]
    needs: Needs = Needs.NONE
    # The IDs of the checks it runs, as they appear in its output, for `scenarios`.
    checks: tuple[str, ...] = ()
    # The target --to replaces; OFF_TARGET compares the booted image with it.
    target: str = "update"
    # Depends on behavior the ACL image may not have; see the scenario's description.
    experimental: bool = False
    # Needs `up --cluster`; skipped without a cluster.
    cluster: bool = False


REGISTRY: dict[str, Scenario] = {}


ScenarioFunc = Callable[["Context"], None]


def scenario(name: str, description: str, *, needs: Needs = Needs.NONE, checks: tuple[str, ...] = (),
             target: str = "update", experimental: bool = False,
             cluster: bool = False) -> Callable[[ScenarioFunc], ScenarioFunc]:
    def register(func: ScenarioFunc) -> ScenarioFunc:
        if name in REGISTRY or name == "all":
            raise ValueError(f"scenario name {name!r} is taken")
        REGISTRY[name] = Scenario(name, description, func, needs, checks, target, experimental, cluster)
        return func

    return register


@scenario("forward", "Update to --to (default: update): stage, boot the new slot, and commit",
          needs=Needs.OFF_TARGET, checks=("A1", "A2", "A3", "A4"))
def forward(ctx: Context) -> None:
    ctx.update(ctx.target("update"))


@scenario("reboot-after-commit", "Reboot after a committed update; the VM must stay on the new "
          "slot, which shows commit set BootOrder rather than only the one-time BootNext",
          needs=Needs.COMMITTED_UPDATE, checks=("A5",))
def reboot_after_commit(ctx: Context) -> None:
    before, after = ctx.reboot()
    ctx.expect_same_slot("A5", before, after)


@scenario("manual-rollback", "Roll the last committed update back with `trident rollback --ab`; "
          "the VM must return to the previous slot and image",
          needs=Needs.COMMITTED_UPDATE, checks=("R1",))
def manual_rollback(ctx: Context) -> None:
    ctx.rollback()


@scenario("pingpong", "Update back and forth between the base and update images, --rounds times, "
          "without reinstalling", checks=("P<n>/A1", "P<n>/A2", "P<n>/A3", "P<n>/A4"), target="other")
def pingpong(ctx: Context) -> None:
    for round_number in range(1, ctx.rounds + 1):
        with ctx.scope(f"P{round_number}"):
            ctx.update(ctx.target("other", allow_override=False))


@scenario("health-rollback", "Update with a health check that always fails; commit must roll back "
          "to the previous slot. Skipped if this Trident rejects a health section in an image-only "
          "Host Configuration", checks=("H1",), target="other", experimental=True)
def health_rollback(ctx: Context) -> None:
    ctx.health_rollback(ctx.target("other"))


@scenario("annotation-update", "Update the way AKS will: by annotations on the Node, which "
          "trident-acl-agent acts on, getting the image from the harness's update server. Between stage "
          "and finalize, checks the contract's rules for a staged update",
          needs=Needs.OFF_TARGET, checks=("N0", "N1", "N5", "N4", "N2", "A2", "A3", "A4"), cluster=True,
          experimental=True)
def annotation_update(ctx: Context) -> None:
    ctx.annotation_update(ctx.target("update"))


@scenario("annotation-rollback", "Roll the last committed update back by annotation",
          needs=Needs.COMMITTED_UPDATE, checks=("N3", "R1"), cluster=True, experimental=True)
def annotation_rollback(ctx: Context) -> None:
    ctx.annotation_rollback()


@scenario("annotation-rejects", "Requests trident-acl-agent must refuse without touching Trident: "
          "unparseable ones, and ones that break the contract's rules",
          checks=("N0", "N4"), cluster=True, experimental=True)
def annotation_rejects(ctx: Context) -> None:
    ctx.annotation_rejects(ctx.target("update"))


@scenario("annotation-heartbeat", "An update by annotation whose download the update server slows, "
          "so the stage runs for a few of the agent's heartbeats, and its InProgress status refreshes",
          needs=Needs.OFF_TARGET, checks=("N0", "N1", "N6", "N2", "A2", "A3", "A4"), cluster=True, experimental=True)
def annotation_heartbeat(ctx: Context) -> None:
    ctx.annotation_update(ctx.target("update"), contract_checks=False, heartbeat=True)


@scenario("annotation-target-boot-failed", "An update by annotation whose reboot lands on the old slot: "
          "the VM is reset as the new slot's kernel starts. The commit must report TargetBootFailed, "
          "and a new update then succeed. Skipped if the reset comes too late",
          needs=Needs.OFF_TARGET, checks=("N0", "N1", "N7", "retry/N1", "retry/N2"), cluster=True, experimental=True)
def annotation_target_boot_failed(ctx: Context) -> None:
    ctx.annotation_target_boot_failed(ctx.target("update"))
