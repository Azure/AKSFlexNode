#!/usr/bin/env python3
"""Boot Azure Container Linux from the documented Butane config and join it to a kind cluster.

    hack/acl/acl.py up [--image ACL.qcow2] build, create the cluster and network, boot the VM
    hack/acl/acl.py test [--reset MODE]    check the node, reboot it, reset it, check cleanup
    hack/acl/acl.py down                   remove the VM, the network, and the cluster

The VM is provisioned only by the Ignition config that scripts/aks-flex-node-bootstrap.bu renders,
as getting-started documents, plus harness access (SSH user, hostname, static address). Butane
runs from its container image. bootstrap.sh then installs the agent under
/opt/unbounded/agent, the host root, and joins the node with a bootstrap token. The agent's machine
client talks to an in-cluster AKS Flex controller through the API server's service proxy, so the
node uses no Azure resources.

Without --image, up boots the Azure build of the image, which it first exports from a shared
compute gallery through a temporary managed disk; see export_gallery_image. That needs az and a
resource group to make the disk in, and az names the gallery's latest version on every run unless
ACL_IMAGE_VERSION pins one.

The bridge networking, VM launch, UKI patching, and gallery export are adapted from the unbounded
repository's hack/agent/e2e-kind harness. ukiboot.py is copied from it unchanged: it adds the
harness's first-boot kernel arguments to the image's first-boot addon, which ignition-quench
deletes after the first boot.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import datetime
import hashlib
import http.client
import http.server
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import tarfile
import textwrap
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable
from pathlib import Path
from xml.etree import ElementTree

sys.path.insert(0, str(Path(__file__).resolve().parent))
import ukiboot  # noqa: E402

REPO = Path(__file__).resolve().parents[2]
STATE = Path(os.environ.get("ACL_STATE_DIR", str(REPO / ".vm" / "acl")))

CLUSTER = os.environ.get("ACL_KIND_CLUSTER", "aks-flex-acl")
KIND_NODE = f"{CLUSTER}-control-plane"
KUBE_CONTEXT = f"kind-{CLUSTER}"

# The subnet and interface names differ from the unbounded harness, so both can run on one host.
SUBNET = os.environ.get("ACL_SUBNET", "192.168.111")
GATEWAY = f"{SUBNET}.1"
KIND_BRIDGE_IP = f"{SUBNET}.2"
VM_IP = f"{SUBNET}.10"
BRIDGE = "virbr-acl"
TAP = "tap-acl"
VETH_HOST = "veth-kind-acl"
VETH_KIND = "eth-acl"
SERVE_PORT = int(os.environ.get("ACL_SERVE_PORT", "8299"))
VM_MEMORY = os.environ.get("ACL_VM_MEMORY", "4096")
VM_CPUS = os.environ.get("ACL_VM_CPUS", "2")
VM_MIN_DISK = 20 * 1024**3

NODE_NAME = "acl-harness"
# Must match hostroot.Path in the agent library. Its parent belongs to the host.
HOST_ROOT_PARENT = "/opt/unbounded"
HOST_ROOT = f"{HOST_ROOT_PARENT}/agent"
SSH_USER = "core"
CONTROLLER_IMAGE = "localhost/aks-flex-controller:acl-harness"
PROXY_PATH = "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy"
BOOTSTRAP_GROUP = "system:bootstrappers:aks-flex-node"
BOOTSTRAP_UNIT = "aks-flex-node-bootstrap.service"
AGENT_UNIT = "aks-flex-node-agent.service"
FAKE_CLUSTER_ID = (
    "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/acl-harness"
    "/providers/Microsoft.ContainerService/managedClusters/acl-harness"
)
# Passed to bootstrap.sh through bootstrap.env, quoted as getting-started says. It arrives in the
# installed config unchanged only if that quoting keeps $, %, quotes, and backslashes as written.
QUOTING_PROBE = """$HOME ${X} %h %% "q" \\ it's"""
BUTANE_TEMPLATE = REPO / "scripts" / "aks-flex-node-bootstrap.bu"
BUTANE_IMAGE = os.environ.get("ACL_BUTANE_IMAGE", "quay.io/coreos/butane:v0.29.0")

AGENT_ARCHIVE = "aks-flex-node-linux-amd64.tar.gz"
IGNITION_NAME = "config.ign"

# Without --image, `up` boots the Azure build of the image, published to a shared compute gallery,
# and exports it here first; see export_gallery_image. It is outside STATE, so `down` keeps it.
# The settings are the unbounded harness's.
IMAGE_DIR = Path(os.environ.get("ACL_IMAGE_DIR", str(REPO / ".vm" / "acl-images")))
GALLERY_IMAGE = os.environ.get(
    "ACL_IMAGE_GALLERY_IMAGE",
    "/SharedGalleries/b3e01d89-bd55-414f-bbb4-cdfeb2628caa-ACL/Images/acl-1es-eval",
)
# The image is replicated to one region, which is where the export's disk has to be made.
GALLERY_LOCATION = os.environ.get("ACL_IMAGE_GALLERY_LOCATION", "westus2")
# The gallery's latest version unless this pins one.
GALLERY_VERSION = os.environ.get("ACL_IMAGE_VERSION", "")
# Where the export's temporary disk is made, and the subscription to use if not az's default.
EXPORT_RESOURCE_GROUP = os.environ.get("ACL_IMAGE_RESOURCE_GROUP", "")
EXPORT_SUBSCRIPTION = os.environ.get("ACL_IMAGE_SUBSCRIPTION", "")
GALLERY_IMAGE_PATTERN = re.compile(r"/SharedGalleries/([^/]+)/Images/([^/]+)")
GALLERY_VERSION_PATTERN = re.compile(r"[A-Za-z0-9._-]+")
# The export's temporary disks carry this tag, and only disks with it are ever swept. It is not
# the unbounded harness's, so neither harness sweeps the other's disks in a shared group.
EXPORT_DISK_TAG = "aks-flex-node-acl-harness"
# A disk older than this was left by a run that is gone: an interrupted run may not delete it. It
# is far longer than any export, so a running one is never touched.
EXPORT_STALE_AFTER = 6 * 3600
# The download takes under a minute on a fast link, and the SAS is revoked as soon as it ends;
# this only bounds a SAS that outlives a dead run. It is longer than the unbounded harness's ten
# minutes, which assume a CI runner's link: requests made after it expires are refused, and the
# 850 MiB have to arrive within it.
EXPORT_SAS_SECONDS = 1800
EXPORT_CHUNK = 8 << 20
EXPORT_WORKERS = 16
VHD_FOOTER_SIZE = 512

SSH_KEY = STATE / "ssh" / "id_ed25519"
SSH_OPTS = [
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "LogLevel=ERROR",
    "-o", "ConnectTimeout=10",
    "-i", str(SSH_KEY),
]

OVMF_CANDIDATES = [
    ("/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"),
    ("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"),
    ("/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"),
    ("/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin"),
]


# ---------------------------------------------------------------------------------------------
# Process helpers


def log(message: str) -> None:
    print(f"[acl] {message}", flush=True)


def warn(message: str) -> None:
    """Report a problem that does not fail the run. The message may hold another program's
    output. In GitHub Actions every line of the log is read for workflow commands, so it is
    printed once, escaped, as a warning, where a line break in it cannot start a command."""
    if os.environ.get("GITHUB_ACTIONS") == "true":
        escaped = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::warning::{escaped}", flush=True)
    else:
        print(f"[acl] warning: {message}", flush=True)


def die(message: str) -> None:
    print(f"[acl] error: {message}", file=sys.stderr, flush=True)
    sys.exit(1)


def run(cmd: list[str], *, check: bool = True, capture: bool = False, input: str | None = None,
        env: dict[str, str] | None = None, cwd: Path | None = None) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        cmd, check=False, text=True, input=input, cwd=cwd,
        env={**os.environ, **env} if env else None,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
    )
    if check and result.returncode != 0:
        detail = f": {result.stderr.strip()}" if capture and result.stderr else ""
        die(f"{' '.join(cmd)} exited {result.returncode}{detail}")
    return result


def capture(cmd: list[str], **kwargs) -> str:
    return run(cmd, capture=True, **kwargs).stdout.strip()


def sudo(*cmd: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return run(["sudo", *cmd], check=check, capture=not check)


def kubectl(*args: str, check: bool = True, input: str | None = None,
            quiet: bool = False) -> subprocess.CompletedProcess[str]:
    return run(["kubectl", "--context", KUBE_CONTEXT, *args], check=check, input=input,
               capture=quiet)


def kubectl_out(*args: str) -> str:
    return capture(["kubectl", "--context", KUBE_CONTEXT, *args])


def wait_until(description: str, timeout: float, probe, interval: float = 5) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if probe():
            return
        time.sleep(interval)
    die(f"timed out after {int(timeout)}s waiting for {description}")


# ---------------------------------------------------------------------------------------------
# Build


def build_binaries() -> tuple[Path, Path]:
    """Build a static linux/amd64 agent and controller. The agent runs on the ACL host, whose glibc
    may be older than the build host's, so cgo is off."""
    STATE.mkdir(parents=True, exist_ok=True)
    env = {"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
    agent = STATE / "aks-flex-node-linux-amd64"
    controller = STATE / "controller" / "aks-flex-controller"
    controller.parent.mkdir(exist_ok=True)
    log("building aks-flex-node and aks-flex-controller")
    run(["go", "build", "-o", str(agent), "./cmd/aks-flex-node"], env=env, cwd=REPO)
    run(["go", "build", "-o", str(controller), "./cmd/aks-flex-controller"], env=env, cwd=REPO)

    with tarfile.open(STATE / AGENT_ARCHIVE, "w:gz") as archive:
        archive.add(agent, arcname=agent.name)
    return agent, controller


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


# ---------------------------------------------------------------------------------------------
# Image
#
# Exporting the gallery image is ported from the unbounded harness's e2e.py, Azure/unbounded#885.


def gallery_image() -> Path:
    """Return the gallery image as a qcow2 in IMAGE_DIR, exporting it unless an earlier run did.

    The file is named for the version, so a newer one is exported without a change here and never
    masked by an earlier export. It is made under another name and renamed only once complete, so
    an interrupted export never sits under the name that is reused. A reused file is checked in
    full, since a host that crashed after an export could have lost its tail.
    """
    version = gallery_version()
    image = IMAGE_DIR / f"acl-gallery-{version}.qcow2"
    if image.exists():
        if run(["qemu-img", "check", "-q", "-f", "qcow2", str(image)], check=False).returncode != 0:
            die(f"{image}, exported earlier, fails qemu-img check; remove it to export it again")
        log(f"using {image}, exported earlier")
        return image

    IMAGE_DIR.mkdir(parents=True, exist_ok=True)
    # What an export killed outright left behind, whatever its version: the sparse VHD alone can
    # take most of a gigabyte.
    for leftover in IMAGE_DIR.glob("acl-gallery-*.qcow2.part*"):
        leftover.unlink(missing_ok=True)
    partial = image.with_name(image.name + ".part")
    export_gallery_image(f"{GALLERY_IMAGE}/Versions/{version}", partial)
    partial.replace(image)
    fsync_dir(IMAGE_DIR)
    return image


def fsync_dir(path: Path) -> None:
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def gallery_image_parts() -> tuple[str, str]:
    """Return the gallery's unique name and the image definition."""
    match = GALLERY_IMAGE_PATTERN.fullmatch(GALLERY_IMAGE)
    if not match:
        die(f"ACL_IMAGE_GALLERY_IMAGE={GALLERY_IMAGE!r} is not /SharedGalleries/<gallery>/Images/<image>")
        raise AssertionError("unreachable")
    return match.group(1), match.group(2)


def gallery_version() -> str:
    """Return the gallery image version to boot: ACL_IMAGE_VERSION, or the one "latest" names
    now. It names the image file, so it has to be a plain name, and not "latest", which would
    keep naming the first version exported under it."""
    if GALLERY_VERSION == "latest":
        die("ACL_IMAGE_VERSION=latest would reuse whichever version was exported first; "
            "leave it unset for the gallery's latest version")
    version = GALLERY_VERSION
    if not version:
        gallery, image = gallery_image_parts()
        version = acl_az([
            "sig", "image-version", "show-shared",
            "--gallery-unique-name", gallery,
            "--gallery-image-definition", image,
            "--gallery-image-version", "latest",
            "--location", GALLERY_LOCATION,
            "--query", "name", "-o", "tsv",
        ])
        log(f"Azure Container Linux gallery version {version} is the latest of {GALLERY_IMAGE}")
    if not GALLERY_VERSION_PATTERN.fullmatch(version):
        source = "ACL_IMAGE_VERSION" if GALLERY_VERSION else "the gallery's latest version"
        die(f"{source} {version!r} is not a version")
    return version


def az_command(args: list[str]) -> list[str]:
    if shutil.which("az") is None:
        die("az is needed to export the gallery image; install it, or pass --image")
    subscription = ["--subscription", EXPORT_SUBSCRIPTION] if EXPORT_SUBSCRIPTION else []
    return ["az", *args, *subscription, "--only-show-errors"]


def acl_az(args: list[str]) -> str:
    """Run az for the gallery image and return its output. A failure reports az's own error,
    which never holds a SAS: only grant-access prints one, to stdout."""
    result = run(az_command(args), check=False, capture=True)
    if result.returncode != 0:
        die(f"az {' '.join(args[:3])} failed (exit {result.returncode}): {result.stderr.strip()[-2000:]}")
    return result.stdout.strip()


def acl_az_best_effort(args: list[str], what: str) -> str | None:
    """Run az for cleanup, which must not fail the run: whatever it leaves, the next export's
    sweep removes."""
    result = run(az_command(args), check=False, capture=True)
    if result.returncode != 0:
        warn(f"could not {what}: {result.stderr.strip()[-500:]}")
        return None
    return result.stdout.strip()


def export_gallery_image(version_id: str, destination: Path) -> None:
    """Write a shared gallery image version to destination as qcow2.

    A gallery image cannot be downloaded as such. It is copied to a temporary managed disk in
    ACL_IMAGE_RESOURCE_GROUP and read through a read-only SAS, and the disk is deleted whether or
    not that worked. Only the pages the disk has written are fetched, about 850 MiB of a 32.5 GiB
    disk, into a sparse fixed VHD, which is the raw disk followed by a footer.

    The footer is cut off and the raw disk converted, rather than the VHD. qemu-img before 10.0
    sizes a VHD Azure wrote from its CHS geometry, not its footer's size, and loses 640 KiB off
    the end of this one, and with it the GPT's backup header; the guest then finds no partitions.
    The qcow2 has to come out at the disk's exact size, whatever qemu-img the host has.

    The gallery publishes no digest to check the result against. The transfer is TLS from Azure,
    the VHD footer and the qcow2 are checked, and the qcow2's sha256 is logged.
    """
    if not EXPORT_RESOURCE_GROUP:
        die("ACL_IMAGE_RESOURCE_GROUP must name the resource group, in "
            f"{GALLERY_LOCATION}, where the gallery image is exported; or pass --image")

    sweep_stale_export_disks()

    run_label = os.environ.get("GITHUB_RUN_ID", "local")
    disk = f"{EXPORT_DISK_TAG}-{run_label}-{secrets.token_hex(3)}"
    vhd = destination.with_name(destination.name + ".vhd")
    started = time.monotonic()
    log(f"exporting {version_id} through disk {disk} in {EXPORT_RESOURCE_GROUP}")

    try:
        try:
            acl_az(["disk", "create", "-g", EXPORT_RESOURCE_GROUP, "-n", disk,
                    "--location", GALLERY_LOCATION, "--sku", "Standard_LRS",
                    "--gallery-image-reference", version_id,
                    "--tags", f"purpose={EXPORT_DISK_TAG}", f"run={run_label}",
                    "--query", "id", "-o", "tsv"])
            try:
                download_page_blob(grant_export_sas(disk), vhd)
            finally:
                # Azure does not delete a disk while a SAS on it is active.
                acl_az_best_effort(["disk", "revoke-access", "-g", EXPORT_RESOURCE_GROUP, "-n", disk],
                                   f"revoke access to disk {disk}")
        finally:
            acl_az_best_effort(["disk", "delete", "-g", EXPORT_RESOURCE_GROUP, "-n", disk, "--yes"],
                               f"delete disk {disk}")

        log("converting the exported disk to qcow2")
        disk_bytes = vhd.stat().st_size - VHD_FOOTER_SIZE
        os.truncate(vhd, disk_bytes)
        run(["qemu-img", "convert", "-f", "raw", "-O", "qcow2", str(vhd), str(destination)])
        run(["qemu-img", "check", "-q", "-f", "qcow2", str(destination)])
        info = json.loads(capture(["qemu-img", "info", "-f", "qcow2", "--output=json", str(destination)]))
        if info.get("virtual-size") != disk_bytes:
            raise RuntimeError(f"the qcow2 is {info.get('virtual-size')} bytes, the disk {disk_bytes}")
        # qemu-img does not flush what it wrote, and the caller renames it to a name that is
        # reused.
        with destination.open("rb") as converted:
            os.fsync(converted.fileno())
    except RuntimeError as exc:
        destination.unlink(missing_ok=True)
        die(f"exporting {version_id} failed: {exc}")
    except BaseException:
        # die() raises SystemExit, and an interrupt raises KeyboardInterrupt.
        destination.unlink(missing_ok=True)
        raise
    finally:
        vhd.unlink(missing_ok=True)

    log(f"exported {version_id} in {time.monotonic() - started:.0f}s: {destination.stat().st_size} bytes, "
        f"sha256 {sha256(destination)}")


def grant_export_sas(disk: str) -> str:
    """Return a read SAS on the disk. It goes nowhere but this process's own requests, and is
    masked in GitHub's log in case anything prints it."""
    granted = json.loads(acl_az([
        "disk", "grant-access", "-g", EXPORT_RESOURCE_GROUP, "-n", disk,
        "--access-level", "Read", "--duration-in-seconds", str(EXPORT_SAS_SECONDS), "-o", "json",
    ]))
    sas = granted.get("accessSas") or granted.get("accessSAS") or ""
    if not sas.startswith("https://"):
        die(f"grant-access on disk {disk} returned no SAS")
    if os.environ.get("GITHUB_ACTIONS") == "true":
        for secret in sas_secrets(sas):
            print(f"::add-mask::{secret}", flush=True)
    return sas


def sas_secrets(sas: str) -> list[str]:
    """Return what to mask for a SAS: the whole URL, and its signature on its own, both as the
    URL carries it, percent-encoded, and decoded. Either form of the signature is all an error
    that prints part of the URL, or the URL decoded, would need to leak; GitHub masks only exact
    matches."""
    query = urllib.parse.urlsplit(sas).query
    encoded = [value for key, _, value in (part.partition("=") for part in query.split("&")) if key == "sig"]
    decoded = urllib.parse.parse_qs(query).get("sig", [])
    return list(dict.fromkeys(secret for secret in [sas, *encoded, *decoded] if secret))


def sweep_stale_export_disks(now: float | None = None) -> None:
    """Delete export disks that runs which are gone left behind."""
    listed = acl_az_best_effort([
        "disk", "list", "-g", EXPORT_RESOURCE_GROUP,
        "--query", f"[?tags.purpose=='{EXPORT_DISK_TAG}'].{{name:name,created:timeCreated,state:diskState}}",
        "-o", "json",
    ], "list earlier export disks")
    if listed is None:
        return

    cutoff = (time.time() if now is None else now) - EXPORT_STALE_AFTER
    for disk in json.loads(listed or "[]"):
        # A disk whose age is unknown is left alone: it could be a running export's. The sweep is
        # cleanup, and must not fail the export.
        try:
            created = azure_timestamp(disk["created"])
        except (KeyError, TypeError, ValueError):
            warn(f"not sweeping export disk {disk.get('name')!r}: its creation time "
                 f"{disk.get('created')!r} cannot be read")
            continue
        if created > cutoff:
            continue
        log(f"deleting export disk {disk['name']}, created {disk['created']} by an earlier run")
        if disk.get("state") == "ActiveSAS":
            acl_az_best_effort(["disk", "revoke-access", "-g", EXPORT_RESOURCE_GROUP, "-n", disk["name"]],
                               f"revoke access to disk {disk['name']}")
        acl_az_best_effort(["disk", "delete", "-g", EXPORT_RESOURCE_GROUP, "-n", disk["name"], "--yes"],
                           f"delete disk {disk['name']}")


def azure_timestamp(text: str) -> float:
    """Parse an ARM timestamp, which can carry seven fractional digits."""
    text = re.sub(r"(\.\d{6})\d+", r"\1", text).replace("Z", "+00:00")
    return datetime.datetime.fromisoformat(text).timestamp()


BlobFetch = Callable[[str, dict[str, str]], tuple[bytes, dict[str, str]]]


def download_page_blob(sas: str, destination: Path, fetch: BlobFetch | None = None) -> None:
    """Download the written pages of a page blob, such as a managed disk read through its SAS,
    into a sparse file of the blob's size.

    The SAS never leaves this process: it is on no command line, and no error raised here names
    it.
    """
    fetch = fetch or blob_request
    ranges: list[tuple[int, int]] = []
    marker = ""
    size = 0
    while True:
        query = "&comp=pagelist" + (f"&marker={urllib.parse.quote(marker)}" if marker else "")
        body, headers = fetch(sas + query, {})
        size = int(headers.get("x-ms-blob-content-length", "0"))
        root = ElementTree.fromstring(body)
        ranges += [(int(r.findtext("Start") or 0), int(r.findtext("End") or 0)) for r in root.findall("PageRange")]
        marker = root.findtext("NextMarker") or ""
        if not marker:
            break

    if size < VHD_FOOTER_SIZE:
        raise RuntimeError(f"the exported disk reports a size of {size} bytes")
    chunks = page_chunks(ranges)
    total = sum(stop - start + 1 for start, stop in chunks)
    log(f"downloading {total >> 20} MiB written in a {size >> 20} MiB disk")

    started = time.monotonic()
    lock = threading.Lock()
    fetched = [0, 0]
    fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.ftruncate(fd, size)

        def fetch_chunk(chunk: tuple[int, int]) -> None:
            start, stop = chunk
            data, _headers = fetch(sas, {"Range": f"bytes={start}-{stop}"})
            if len(data) != stop - start + 1:
                raise RuntimeError(f"read {len(data)} bytes at offset {start}, expected {stop - start + 1}")
            # The file is sparse, so a zero chunk is already there.
            if data.count(0) != len(data):
                os.pwrite(fd, data, start)
            with lock:
                fetched[0] += len(data)
                step = total // 10 or 1
                if fetched[0] // step != fetched[1]:
                    fetched[1] = fetched[0] // step
                    log(f"  {fetched[0] >> 20} of {total >> 20} MiB")

        with concurrent.futures.ThreadPoolExecutor(EXPORT_WORKERS) as pool:
            futures = [pool.submit(fetch_chunk, chunk) for chunk in chunks]
            try:
                for future in concurrent.futures.as_completed(futures):
                    future.result()
            except BaseException:
                for future in futures:
                    future.cancel()
                raise
        os.fsync(fd)
    finally:
        os.close(fd)

    elapsed = time.monotonic() - started
    log(f"downloaded {total >> 20} MiB in {elapsed:.0f}s ({total / max(elapsed, 0.001) / (1 << 20):.0f} MiB/s)")
    check_fixed_vhd(destination, size)


def page_chunks(ranges: list[tuple[int, int]], chunk: int = EXPORT_CHUNK) -> list[tuple[int, int]]:
    """Split inclusive page ranges into inclusive ranges of at most chunk bytes."""
    out = []
    for start, end in ranges:
        while start <= end:
            stop = min(end, start + chunk - 1)
            out.append((start, stop))
            start = stop + 1
    return out


def check_fixed_vhd(path: Path, size: int) -> None:
    """Fail unless path ends in the footer of a fixed VHD of its size, which is what a managed
    disk is and what qemu-img's vpc driver reads."""
    with path.open("rb") as handle:
        handle.seek(size - VHD_FOOTER_SIZE)
        footer = handle.read(VHD_FOOTER_SIZE)
    cookie, current, kind = footer[:8], int.from_bytes(footer[48:56], "big"), int.from_bytes(footer[60:64], "big")
    if cookie != b"conectix" or kind != 2 or current != size - VHD_FOOTER_SIZE:
        raise RuntimeError(f"the exported disk is not a fixed VHD of {size} bytes "
                           f"(cookie {cookie!r}, type {kind}, size {current})")


def blob_request(url: str, headers: dict[str, str]) -> tuple[bytes, dict[str, str]]:
    """GET from blob storage, retrying what may pass. Errors name the status or the failure,
    never the URL, which carries the SAS."""
    request = urllib.request.Request(url, headers={"x-ms-version": "2021-12-02", **headers})
    attempts = 5
    for attempt in range(1, attempts + 1):
        try:
            with urllib.request.urlopen(request, timeout=120) as response:
                return response.read(), {k.lower(): v for k, v in response.headers.items()}
        except urllib.error.HTTPError as exc:
            if (exc.code < 500 and exc.code != 429) or attempt == attempts:
                raise RuntimeError(f"blob storage answered HTTP {exc.code} {exc.reason}") from None
        except (urllib.error.URLError, http.client.HTTPException, OSError) as exc:
            if attempt == attempts:
                raise RuntimeError(f"blob storage request failed: {type(exc).__name__}") from None
        time.sleep(2 * attempt)
    raise AssertionError("unreachable")


# ---------------------------------------------------------------------------------------------
# Host networking


def nm_unmanage(iface: str) -> None:
    """Keep NetworkManager from detaching the interface from the bridge."""
    if shutil.which("nmcli"):
        sudo("nmcli", "device", "set", iface, "managed", "no", check=False)


def docker_uses_nft() -> bool:
    return bool(shutil.which("nft")) and sudo("nft", "list", "chain", "ip", "filter", "DOCKER-USER",
                                              check=False).returncode == 0


def create_network() -> None:
    log(f"creating bridge {BRIDGE} ({GATEWAY}/24) and {TAP}")
    delete_network()
    sudo("ip", "link", "add", BRIDGE, "type", "bridge")
    sudo("ip", "addr", "add", f"{GATEWAY}/24", "dev", BRIDGE)
    sudo("ip", "link", "set", BRIDGE, "up")
    sudo("ip", "tuntap", "add", "dev", TAP, "mode", "tap")
    sudo("ip", "link", "set", TAP, "master", BRIDGE)
    sudo("ip", "link", "set", TAP, "up")
    nm_unmanage(BRIDGE)
    nm_unmanage(TAP)

    # Docker's forwarding policy drops traffic it did not create.
    if docker_uses_nft():
        sudo("nft", "insert", "rule", "ip", "filter", "DOCKER-USER", "iifname", BRIDGE, "accept")
        sudo("nft", "insert", "rule", "ip", "filter", "DOCKER-USER", "oifname", BRIDGE, "accept")
    else:
        sudo("iptables", "-I", "FORWARD", "-i", BRIDGE, "-j", "ACCEPT")
        sudo("iptables", "-I", "FORWARD", "-o", BRIDGE, "-j", "ACCEPT")
    # Docker may drop non-Docker traffic to container addresses in raw PREROUTING.
    sudo("iptables", "-t", "raw", "-I", "PREROUTING", "-i", BRIDGE, "-j", "ACCEPT", check=False)
    # The VM reaches the internet for the rootfs image and Kubernetes binaries.
    sudo("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", f"{SUBNET}.0/24",
         "!", "-d", f"{SUBNET}.0/24", "-j", "MASQUERADE")


def delete_network() -> None:
    for iface in (TAP, VETH_HOST, BRIDGE):
        sudo("ip", "link", "delete", iface, check=False)
    for chain_opt in ("-i", "-o"):
        while sudo("iptables", "-D", "FORWARD", chain_opt, BRIDGE, "-j", "ACCEPT",
                   check=False).returncode == 0:
            pass
    while sudo("iptables", "-t", "raw", "-D", "PREROUTING", "-i", BRIDGE, "-j", "ACCEPT",
               check=False).returncode == 0:
        pass
    while sudo("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", f"{SUBNET}.0/24",
               "!", "-d", f"{SUBNET}.0/24", "-j", "MASQUERADE", check=False).returncode == 0:
        pass
    if docker_uses_nft():
        listing = sudo("nft", "-a", "list", "chain", "ip", "filter", "DOCKER-USER", check=False).stdout
        for line in listing.splitlines():
            if f'"{BRIDGE}"' in line and "# handle" in line:
                handle = line.rsplit("# handle", 1)[1].strip()
                sudo("nft", "delete", "rule", "ip", "filter", "DOCKER-USER", "handle", handle,
                     check=False)


def kind_pid() -> str:
    return capture(["docker", "inspect", KIND_NODE, "--format", "{{.State.Pid}}"])


def kind_docker_ip() -> str:
    ip = capture(["docker", "inspect", KIND_NODE, "--format",
                  "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"])
    if not ip:
        die(f"no address for {KIND_NODE}")
    return ip


def attach_kind_to_bridge() -> None:
    """Give the kind node an address on the bridge, so it can reach the VM's kubelet."""
    log(f"attaching {KIND_NODE} to {BRIDGE} as {KIND_BRIDGE_IP}")
    pid = kind_pid()
    sudo("ip", "link", "delete", VETH_HOST, check=False)
    sudo("ip", "link", "add", VETH_HOST, "type", "veth", "peer", "name", VETH_KIND)
    sudo("ip", "link", "set", VETH_HOST, "master", BRIDGE)
    sudo("ip", "link", "set", VETH_HOST, "up")
    sudo("ip", "link", "set", VETH_KIND, "netns", pid)
    sudo("nsenter", "-t", pid, "-n", "ip", "addr", "add", f"{KIND_BRIDGE_IP}/24", "dev", VETH_KIND)
    sudo("nsenter", "-t", pid, "-n", "ip", "link", "set", VETH_KIND, "up")
    nm_unmanage(VETH_HOST)


# ---------------------------------------------------------------------------------------------
# Cluster


def cluster_exists() -> bool:
    return CLUSTER in capture(["kind", "get", "clusters"]).split()


def create_cluster(node_image: str) -> None:
    if cluster_exists():
        log(f"kind cluster {CLUSTER} exists")
    else:
        cmd = ["kind", "create", "cluster", "--name", CLUSTER, "--wait", "120s"]
        if node_image:
            cmd += ["--image", node_image]
        run(cmd)

    attach_kind_to_bridge()
    docker_ip = kind_docker_ip()
    advertise_bridge_address()

    # kindnet and kube-proxy on the VM's node would otherwise use the control plane's container
    # hostname, which the VM cannot resolve.
    endpoint = f"{docker_ip}:6443"
    patch = {"spec": {"template": {"spec": {"containers": [
        {"name": "kindnet-cni", "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": endpoint}]}]}}}}
    kubectl("-n", "kube-system", "patch", "daemonset", "kindnet", "--type=strategic",
            "-p", json.dumps(patch))
    kubeconfig = kubectl_out("-n", "kube-system", "get", "configmap", "kube-proxy",
                             "-o", "jsonpath={.data.kubeconfig\\.conf}")
    lines = [f"    server: https://{endpoint}" if line.strip().startswith("server:") else line
             for line in kubeconfig.splitlines()]
    kubectl("-n", "kube-system", "patch", "configmap", "kube-proxy", "--type=merge",
            "-p", json.dumps({"data": {"kubeconfig.conf": "\n".join(lines) + "\n"}}))
    kubectl("-n", "kube-system", "rollout", "restart", "daemonset/kube-proxy")
    # Restarting kindnet after the address change avoids a race that leaves it crash looping.
    kubectl("-n", "kube-system", "delete", "pod", "-l", "app=kindnet", "--wait=false")
    kubectl("-n", "kube-system", "rollout", "status", "daemonset/kindnet", "--timeout=120s")
    kubectl("-n", "kube-system", "rollout", "status", "daemonset/kube-proxy", "--timeout=120s")


def advertise_bridge_address() -> None:
    """Make the control plane advertise its bridge address.

    kindnet on the VM's node routes the control plane's pod CIDR through the control plane's node
    address. The container's Docker address is not on the VM's link, so the route fails and
    kindnet never becomes ready.
    """
    script = textwrap.dedent(f"""\
        set -eu
        . /var/lib/kubelet/kubeadm-flags.env
        set -- $KUBELET_KUBEADM_ARGS
        new_args=""
        for arg do
          case "$arg" in
            --node-ip=*) ;;
            *) new_args="$new_args $arg" ;;
          esac
        done
        printf 'KUBELET_KUBEADM_ARGS="%s --node-ip={KIND_BRIDGE_IP}"\\n' "${{new_args# }}" \\
          >/var/lib/kubelet/kubeadm-flags.env
        systemctl restart kubelet
    """)
    run(["docker", "exec", KIND_NODE, "sh", "-c", script])

    def advertised() -> bool:
        result = kubectl("get", "node", KIND_NODE, "-o",
                         "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}",
                         check=False, quiet=True)
        return result.returncode == 0 and result.stdout.split() == [KIND_BRIDGE_IP]

    wait_until(f"{KIND_NODE} to advertise {KIND_BRIDGE_IP}", 120, advertised, 3)


def install_controller(controller: Path) -> None:
    """Deploy the controller from the repository manifests, with a locally built image."""
    log("deploying aks-flex-controller")
    context = controller.parent
    (context / "Dockerfile").write_text(textwrap.dedent("""\
        FROM scratch
        COPY aks-flex-controller /usr/local/bin/aks-flex-controller
        USER 65532:65532
        ENTRYPOINT ["/usr/local/bin/aks-flex-controller"]
    """))
    run(["docker", "build", "-q", "-t", CONTROLLER_IMAGE, str(context)])
    run(["kind", "load", "docker-image", CONTROLLER_IMAGE, "--name", CLUSTER])

    # The daemon discovers MachineOperations at startup, so the CRD comes first.
    unbounded = capture(["go", "list", "-m", "-f", "{{.Dir}}", "github.com/Azure/unbounded"], cwd=REPO)
    kubectl("apply", "-f", f"{unbounded}/deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml")
    kubectl("apply", "-f", "-", input=RBAC_MANIFEST)
    kubectl("apply", "-k", str(REPO / "hack" / "controller-deployment"))
    patch = [
        {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": CONTROLLER_IMAGE},
        {"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"},
        {"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": [
            "--listen-address=:8080",
            "--machine-configmap-namespace=kube-system",
            "--machine-configmap-name=aks-flex-machines",
            # Without the approver the daemon's client certificate is never issued.
            "--enable-csr-approver=true",
        ]},
    ]
    kubectl("-n", "kube-system", "patch", "deployment", "aks-flex-controller", "--type=json",
            "-p", json.dumps(patch))
    kubectl("-n", "kube-system", "rollout", "status", "deployment/aks-flex-controller",
            "--timeout=180s")
    kubectl("get", "--raw", f"{PROXY_PATH}/healthz")


# The bindings scripts/aks-flex-config setup-node-rbac applies for bootstrap-token nodes.
RBAC_MANIFEST = f"""
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-bootstrapper
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node-bootstrapper
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: {BOOTSTRAP_GROUP}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-auto-approve-csr
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: {BOOTSTRAP_GROUP}
"""


def kubernetes_version() -> str:
    version = json.loads(kubectl_out("version", "-o", "json"))["serverVersion"]["gitVersion"]
    return version.removeprefix("v").split("+", 1)[0]


def create_bootstrap_token() -> str:
    token_id, token_secret = secrets.token_hex(3), secrets.token_hex(8)
    expiration = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 24 * 3600))
    secret = {
        "apiVersion": "v1",
        "kind": "Secret",
        "type": "bootstrap.kubernetes.io/token",
        "metadata": {"name": f"bootstrap-token-{token_id}", "namespace": "kube-system"},
        "stringData": {
            "token-id": token_id,
            "token-secret": token_secret,
            "expiration": expiration,
            "usage-bootstrap-authentication": "true",
            "usage-bootstrap-signing": "true",
            "auth-extra-groups": BOOTSTRAP_GROUP,
        },
    }
    kubectl("apply", "-f", "-", input=json.dumps(secret), quiet=True)
    return f"{token_id}.{token_secret}"


def register_machine(version: str) -> None:
    """Add the node to the controller's machine store. The version has to match
    components.kubernetes exactly, since the store is read only."""
    machine = {
        "id": f"{FAKE_CLUSTER_ID}/agentPools/aksflexnodes/machines/{NODE_NAME}",
        "name": NODE_NAME,
        "type": "Microsoft.ContainerService/managedClusters/agentPools/machines",
        "properties": {
            "eTag": "1",
            "provisioningState": "Succeeded",
            "kubernetes": {"orchestratorVersion": version, "nodeLabels": {}, "nodeTaints": []},
        },
    }
    patch = {"data": {f"{NODE_NAME}.json": json.dumps(machine)}}
    kubectl("-n", "kube-system", "patch", "configmap", "aks-flex-machines", "--type=merge",
            "-p", json.dumps(patch))


# ---------------------------------------------------------------------------------------------
# Ignition


def base_config(token: str, version: str) -> dict:
    ca = kubectl_out("config", "view", "--minify", "--raw", "-o",
                     "jsonpath={.clusters[0].cluster.certificate-authority-data}")
    dns_ip = kubectl_out("-n", "kube-system", "get", "service", "kube-dns", "-o",
                         "jsonpath={.spec.clusterIP}")
    return {
        "azure": {
            "bootstrapToken": {"token": token},
            "targetCluster": {"resourceId": FAKE_CLUSTER_ID, "location": "local"},
        },
        "agent": {
            "nodeName": NODE_NAME,
            "machineClient": {"mode": "in-cluster", "endpointUrl": PROXY_PATH},
            "requireMachineRegistration": True,
        },
        "components": {"kubernetes": version},
        # Required so that reset has LocalDNS state to clean up.
        "networking": {"dnsServiceIP": dns_ip, "localDNS": {"mode": "Required"}},
        "node": {"kubelet": {"clusterFQDN": f"{kind_docker_ip()}:6443", "caCertData": ca}},
    }


def env_file_value(value: str) -> str:
    """Quote a value for a systemd environment file, as getting-started says to: single quotes keep
    it as written, and a value with a single quote in it goes in double quotes, with " and \\
    escaped."""
    if "'" not in value:
        return f"'{value}'"
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def render_ignition(config: dict, agent_url: str, agent_sha256: str) -> dict:
    """Render the documented Butane config from the three files it names, as an operator does."""
    files = STATE / "first-boot"
    shutil.rmtree(files, ignore_errors=True)
    files.mkdir(mode=0o700)
    shutil.copyfile(REPO / "scripts" / "bootstrap.sh", files / "bootstrap.sh")
    for name, content in (
        ("base-config.json", json.dumps(config, indent=2) + "\n"),
        ("bootstrap.env", "".join(f"{key}={env_file_value(value)}\n" for key, value in {
            "AKS_FLEX_NODE_AGENT_URL": agent_url,
            "AKS_FLEX_NODE_AGENT_SHA256": agent_sha256,
            "AKS_FLEX_NODE_CONFIG_OVERRIDES": json.dumps({"aclHarness": {"quoting": QUOTING_PROBE}}),
        }.items())),
    ):
        path = files / name
        path.write_text(content)
        path.chmod(0o600)
    rendered = capture(["docker", "run", "--rm", "-i", "-v", f"{files}:/files:ro,z", BUTANE_IMAGE,
                        "--strict", "--files-dir", "/files"], input=BUTANE_TEMPLATE.read_text())
    return json.loads(rendered)


def data_url(content: str) -> str:
    return "data:;base64," + base64.b64encode(content.encode()).decode()


def mac_address() -> str:
    octets = [int(part) for part in VM_IP.split(".")]
    return f"52:54:00:{octets[1]:02x}:{octets[2]:02x}:{octets[3]:02x}"


def add_harness_access(doc: dict, ssh_public_key: str) -> dict:
    """Add what the harness needs to reach the VM. Nothing here affects the bootstrap.

    A user Ignition config displaces the image's own systemd section, so the waagent mask that
    config carries is restated. The core user is given in full, because a bare name leaves it on
    /sbin/nologin. The static network unit matches on MAC, since interface names depend on the
    machine type; the ip= argument only configures the initramfs, and only on the first boot.
    The hostname is written because the image leaves it as "localhost": the metadata hostname
    service is masked, by the generic build or by first_boot_kargs.
    """
    doc.setdefault("passwd", {}).setdefault("users", []).append({
        "name": SSH_USER,
        "shell": "/bin/bash",
        "groups": ["sudo", "systemd-journal"],
        "sshAuthorizedKeys": [ssh_public_key],
    })
    doc.setdefault("systemd", {}).setdefault("units", []).append(
        {"name": "waagent.service", "enabled": False, "mask": True})
    network_unit = textwrap.dedent(f"""\
        [Match]
        MACAddress={mac_address()}

        [Network]
        Address={VM_IP}/24
        Gateway={GATEWAY}
        DNS=8.8.8.8
        DNS=8.8.4.4
    """)
    files = doc.setdefault("storage", {}).setdefault("files", [])
    files.append({"path": "/etc/hostname", "mode": 0o644, "overwrite": True,
                  "contents": {"source": data_url(f"{NODE_NAME}\n")}})
    files.append({"path": "/etc/systemd/network/10-acl-harness.network", "mode": 0o644,
                  "overwrite": True, "contents": {"source": data_url(network_unit)}})
    return doc


def initramfs_ip_karg() -> str:
    """Static address for Ignition's fetch in the initramfs. The interface is named because an
    empty device field matches lo first, and every fetch is refused."""
    return f"ip={VM_IP}::{GATEWAY}:24:{NODE_NAME}:eth0:none:8.8.8.8:8.8.4.4"


# On the Azure platform the initramfs fetches the hostname from Azure's metadata endpoint on the
# first boot, and retries it with no limit; under QEMU nothing answers. The generic build of the
# image masks it already, the Azure build does not. The harness writes /etc/hostname through
# Ignition instead; see add_harness_access.
METADATA_HOSTNAME_MASK = "rd.systemd.mask=flatcar-metadata-hostname.service"


def first_boot_kargs(config_url: str) -> str:
    """Return what the harness adds to the first boot's command line: where Ignition fetches its
    config, the initramfs address it fetches it over, and the metadata hostname mask. None of it
    is needed on a later boot, which runs on the image's own command line and the network unit
    and hostname Ignition wrote."""
    return f"ignition.config.url={config_url} {initramfs_ip_karg()} {METADATA_HOSTNAME_MASK}"


class FileServer:
    """Serve an explicit set of files on the bridge. Only these are exposed, since the state
    directory also holds the SSH key and the base config."""

    def __init__(self, files: dict[str, Path]):
        self.files = files
        served = files

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - required name
                path = served.get(self.path.lstrip("/").split("?", 1)[0])
                if path is None:
                    self.send_error(404)
                    return
                body = path.read_bytes()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args) -> None:
                pass

        self.httpd = http.server.ThreadingHTTPServer((GATEWAY, SERVE_PORT), Handler)
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self) -> "FileServer":
        self.thread.start()
        return self

    def __exit__(self, *exc) -> None:
        self.httpd.shutdown()
        self.httpd.server_close()


# ---------------------------------------------------------------------------------------------
# VM


def ovmf_firmware() -> tuple[Path, Path]:
    for code, variables in OVMF_CANDIDATES:
        if Path(code).is_file() and Path(variables).is_file():
            return Path(code), Path(variables)
    die("OVMF firmware not found; install the ovmf or edk2-ovmf package")
    raise AssertionError


def ensure_ssh_key() -> str:
    SSH_KEY.parent.mkdir(parents=True, exist_ok=True)
    if not SSH_KEY.exists():
        run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", str(SSH_KEY)])
    return SSH_KEY.with_suffix(".pub").read_text().strip()


def launch_vm(image: Path, config_url: str) -> None:
    disk = STATE / "acl.qcow2"
    disk.unlink(missing_ok=True)
    info = json.loads(capture(["qemu-img", "info", "--output=json", str(image)]))
    # The root partition runs to the end of the disk, so a smaller overlay stops boot.
    size = max(int(info["virtual-size"]), VM_MIN_DISK)
    run(["qemu-img", "create", "-q", "-f", "qcow2", "-b", str(image.resolve()), "-F",
         info["format"], str(disk), str(size)])

    # systemd-boot only marks a first boot while the image's first-boot addon exists, and
    # ignition-quench deletes it afterwards. Adding the harness's arguments to that addon, rather
    # than booting the kernel directly, keeps later boots from running Ignition again, and they
    # go with it. The overlay is new, so the addon is still there.
    patched = ukiboot.patch_firstboot_cmdline(disk, first_boot_kargs(config_url))
    log(f"patched {patched.addon} ({patched.used}/{patched.capacity} bytes)")

    code, variables_template = ovmf_firmware()
    variables = STATE / "OVMF_VARS.fd"
    shutil.copyfile(variables_template, variables)

    pid_file, serial_log = STATE / "qemu.pid", STATE / "serial.log"
    serial_log.unlink(missing_ok=True)
    run(["setsid", "qemu-system-x86_64",
         "-machine", "q35", "-cpu", "host", "-accel", "kvm",
         "-m", VM_MEMORY, "-smp", VM_CPUS,
         "-drive", f"if=pflash,format=raw,readonly=on,file={code}",
         "-drive", f"if=pflash,format=raw,file={variables}",
         "-drive", f"file={disk},format=qcow2,if=virtio",
         "-netdev", f"tap,id=net0,ifname={TAP},script=no,downscript=no",
         "-device", f"virtio-net-pci,netdev=net0,mac={mac_address()}",
         "-daemonize", "-pidfile", str(pid_file),
         "-serial", f"file:{serial_log}", "-display", "none"])
    log(f"VM started (pid {pid_file.read_text().strip()}, serial log {serial_log})")


def vm_running() -> bool:
    pid_file = STATE / "qemu.pid"
    if not pid_file.exists():
        return False
    try:
        os.kill(int(pid_file.read_text().strip()), 0)
    except (OSError, ValueError):
        return False
    return True


def stop_vm() -> None:
    pid_file = STATE / "qemu.pid"
    if not vm_running():
        pid_file.unlink(missing_ok=True)
        return
    pid = int(pid_file.read_text().strip())
    log(f"stopping VM (pid {pid})")
    os.kill(pid, 15)
    for _ in range(10):
        time.sleep(1)
        if not vm_running():
            break
    else:
        os.kill(pid, 9)
    pid_file.unlink(missing_ok=True)


def ssh(command: str, *, check: bool = True, timeout: float = 120) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run(["ssh", *SSH_OPTS, f"{SSH_USER}@{VM_IP}", command],
                                capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        result = subprocess.CompletedProcess(["ssh"], 255, "", f"timed out: {command}")
    if check and result.returncode != 0:
        die(f"on the VM, {command!r} exited {result.returncode}: {result.stderr.strip()}")
    return result


def ssh_out(command: str) -> str:
    return ssh(command).stdout.strip()


def wait_for_ssh(timeout: float = 600) -> None:
    def reachable() -> bool:
        if not vm_running():
            die(f"the VM exited; see {STATE / 'serial.log'}")
        return ssh("true", check=False, timeout=15).returncode == 0

    wait_until(f"SSH on {VM_IP}", timeout, reachable, 5)


def unit_property(unit: str, prop: str) -> str:
    return ssh_out(f"systemctl show {unit} -p {prop} --value")


def wait_for_bootstrap(timeout: float = 1800) -> None:
    """Wait for the first-boot unit to finish. It retries on failure, so a failed attempt is
    reported but not final."""
    log(f"waiting for {BOOTSTRAP_UNIT}")
    seen_restarts = 0

    def finished() -> bool:
        nonlocal seen_restarts
        # One property per call: systemctl prints several in its own order, not the one asked for.
        active = ssh(f"systemctl show {BOOTSTRAP_UNIT} -p ActiveState --value", check=False).stdout.strip()
        restarts_out = ssh(f"systemctl show {BOOTSTRAP_UNIT} -p NRestarts --value", check=False).stdout.strip()
        if not active or not restarts_out.isdigit():
            return False
        restarts = int(restarts_out)
        if restarts > seen_restarts:
            seen_restarts = restarts
            log(f"{BOOTSTRAP_UNIT} failed and is retrying (restart {restarts}):")
            print(ssh(f"sudo journalctl -u {BOOTSTRAP_UNIT} -b --no-pager -n 30", check=False).stdout)
        return active == "active"

    wait_until(f"{BOOTSTRAP_UNIT} to complete", timeout, finished, 15)
    log(f"{BOOTSTRAP_UNIT} completed")


def wait_for_node_ready(timeout: float = 900) -> None:
    log(f"waiting for node {NODE_NAME} to be Ready")

    def ready() -> bool:
        result = kubectl("get", "node", NODE_NAME, "-o",
                         "jsonpath={.status.conditions[?(@.type=='Ready')].status}",
                         check=False, quiet=True)
        return result.returncode == 0 and result.stdout.strip() == "True"

    wait_until(f"node {NODE_NAME} to be Ready", timeout, ready, 10)


# ---------------------------------------------------------------------------------------------
# Checks


class Checks:
    def __init__(self) -> None:
        self.failures: list[str] = []

    def expect(self, ok: bool, description: str, detail: str = "") -> None:
        if ok:
            log(f"ok: {description}")
        else:
            self.failures.append(description)
            log(f"FAIL: {description}{': ' + detail if detail else ''}")

    def paths_absent(self, description: str, paths: list[str]) -> None:
        present = [p for p in paths if path_exists(p)]
        self.expect(not present, description, ", ".join(present))

    def paths_present(self, description: str, paths: list[str]) -> None:
        missing = [p for p in paths if not path_exists(p)]
        self.expect(not missing, description, "missing " + ", ".join(missing))

    def note(self, message: str) -> None:
        log(f"not exercised: {message}")

    def done(self) -> None:
        if self.failures:
            die(f"{len(self.failures)} check(s) failed: " + "; ".join(self.failures))
        log("all checks passed")


def path_exists(path: str) -> bool:
    return ssh(f"sudo test -e {path} -o -L {path}", check=False).returncode == 0


# What reset has to remove, by what it is. Each is checked to exist before reset, so that its
# absence afterwards means something.
RESET_REMOVES = {
    "the agent, recovery, and first-boot units": [
        f"/etc/systemd/system/{AGENT_UNIT}",
        "/etc/systemd/system/aks-flex-node-agent-recovery.service",
        f"/etc/systemd/system/{BOOTSTRAP_UNIT}",
    ],
    "the config, credentials, and logs": ["/etc/aks-flex-node", "/var/log/aks-flex-node"],
    "the host helpers under the host root": [
        f"{HOST_ROOT}/bin/unbounded-agent-nspawn-lifecycle",
        f"{HOST_ROOT}/lib/aks-flex-node/aks-flex-node-recovery.sh",
    ],
    "the LocalDNS files": [
        "/etc/systemd/system/unbounded-localdns-network.service",
        f"{HOST_ROOT}/libexec/unbounded-localdns-network",
        "/sys/class/net/localdns",
    ],
}


def check_first_boot(checks: Checks) -> None:
    result = unit_property(BOOTSTRAP_UNIT, "Result")
    active = unit_property(BOOTSTRAP_UNIT, "ActiveState")
    checks.expect(active == "active" and result == "success",
                  f"{BOOTSTRAP_UNIT} succeeded on first boot", f"ActiveState={active} Result={result}")
    checks.expect(unit_property(BOOTSTRAP_UNIT, "InvocationID") != "",
                  f"{BOOTSTRAP_UNIT} ran in this boot")
    checks.paths_absent("the first-boot files, which carry the token, were removed after bootstrap",
                        ["/etc/aks-flex-node/first-boot"])

    installed = ssh(f"sudo test -x {HOST_ROOT}/bin/aks-flex-node", check=False).returncode == 0
    checks.expect(installed, f"the agent is installed under {HOST_ROOT}")
    root_is_dir = ssh(f"sudo test -d {HOST_ROOT} -a ! -L {HOST_ROOT}", check=False).returncode == 0
    checks.expect(root_is_dir, f"{HOST_ROOT} is a directory, not a link to the read-only /usr/local")
    parent = ssh_out(f"sudo stat -c '%a %U' {HOST_ROOT_PARENT}")
    checks.expect(parent == "755 root", f"{HOST_ROOT_PARENT} was created 0755 root", f"got {parent!r}")
    checks.paths_absent("nothing was installed under the read-only /usr/local",
                        ["/usr/local/bin/aks-flex-node", "/usr/local/lib/aks-flex-node"])
    probe = ssh_out("sudo jq -r .aclHarness.quoting /etc/aks-flex-node/config.json")
    checks.expect(probe == QUOTING_PROBE, "a setting with $, %, quotes, and backslashes reached "
                  "bootstrap.sh unchanged through bootstrap.env", f"got {probe!r}")
    checks.expect(unit_property(AGENT_UNIT, "ActiveState") == "active", f"{AGENT_UNIT} is active")


def check_workload(checks: Checks) -> None:
    pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "acl-harness-probe", "namespace": "default"},
        "spec": {
            "nodeName": NODE_NAME,
            "restartPolicy": "Never",
            "tolerations": [{"operator": "Exists"}],
            "containers": [{"name": "probe", "image": "docker.io/library/busybox:1.36",
                            "command": ["sh", "-c", "echo acl-harness-ok && sleep 3600"]}],
        },
    }
    kubectl("delete", "pod", "acl-harness-probe", "--ignore-not-found", "--wait=true", quiet=True)
    kubectl("apply", "-f", "-", input=json.dumps(pod), quiet=True)
    ready = kubectl("wait", "pod/acl-harness-probe", "--for=condition=Ready", "--timeout=300s",
                    check=False, quiet=True).returncode == 0
    checks.expect(ready, "a pod runs on the node")
    logs = kubectl("logs", "acl-harness-probe", check=False, quiet=True)
    checks.expect("acl-harness-ok" in logs.stdout, "kubectl logs reaches the kubelet",
                  logs.stderr.strip())
    kubectl("delete", "pod", "acl-harness-probe", "--ignore-not-found", "--wait=false", quiet=True)


def check_reboot(checks: Checks) -> None:
    """The first-boot unit must not run again once the agent is installed, and Ignition must not
    run again at all: its config server is down now, so a second run would stop the boot."""
    log("rebooting the VM")
    ssh("sudo systemctl reboot", check=False, timeout=15)
    wait_until("the VM to go down", 120, lambda: ssh("true", check=False, timeout=5).returncode != 0, 3)
    wait_for_ssh()
    checks.expect(unit_property(BOOTSTRAP_UNIT, "ConditionResult") == "no",
                  f"{BOOTSTRAP_UNIT} is skipped after a reboot")
    checks.expect(unit_property(BOOTSTRAP_UNIT, "InvocationID") == "",
                  f"{BOOTSTRAP_UNIT} did not run after a reboot")
    wait_for_node_ready()
    checks.expect(unit_property(AGENT_UNIT, "ActiveState") == "active",
                  f"{AGENT_UNIT} is active after a reboot")


def reset_node(mode: str) -> None:
    if mode == "cli":
        log("resetting with aks-flex-node reset")
        ssh(f"sudo {HOST_ROOT}/bin/aks-flex-node reset", timeout=600)
    else:
        log("resetting with an AgentReset MachineOperation")
        operation = {
            "apiVersion": "unbounded-cloud.io/v1alpha3",
            "kind": "MachineOperation",
            "metadata": {"name": "acl-harness-reset"},
            "spec": {"machineRef": NODE_NAME, "operationKind": "AgentReset",
                     "ttlSecondsAfterFinished": 3600},
        }
        kubectl("delete", "machineoperation", "acl-harness-reset", "--ignore-not-found", quiet=True)
        kubectl("apply", "-f", "-", input=json.dumps(operation))
        kubectl("wait", "machineoperation/acl-harness-reset", "--for=jsonpath={.status.phase}=Complete",
                "--timeout=600s")

        def uninstalled() -> bool:
            return ssh(f"test -e /etc/systemd/system/{AGENT_UNIT}", check=False).returncode != 0

        wait_until(f"{AGENT_UNIT} to be removed", 300, uninstalled, 5)
    kubectl("delete", "node", NODE_NAME, "--ignore-not-found", "--wait=false", quiet=True)


def localdns_table_exists() -> bool:
    return ssh("sudo nft list table ip unbounded_localdns", check=False).returncode == 0


def machine_exists(machine: str) -> bool:
    return ssh(f"sudo machinectl show {machine}", check=False).returncode == 0


def check_before_reset(checks: Checks) -> bool:
    """Check that what reset removes exists. Returns whether the first-boot unit is active."""
    for what, paths in RESET_REMOVES.items():
        checks.paths_present(f"{what} exist before reset", paths)
    checks.expect(localdns_table_exists(), "the LocalDNS nft table exists before reset")
    checks.expect(machine_exists("kube1"), "the kube1 machine exists before reset")
    return unit_property(BOOTSTRAP_UNIT, "ActiveState") == "active"


def check_reset(checks: Checks, bootstrap_was_active: bool) -> None:
    for what, paths in RESET_REMOVES.items():
        checks.paths_absent(f"reset removed {what}", paths)
    checks.expect(not localdns_table_exists(), "reset removed the LocalDNS nft table")
    parent_kept = ssh(f"sudo test -d {HOST_ROOT_PARENT}", check=False).returncode == 0
    checks.expect(parent_kept, f"reset left {HOST_ROOT_PARENT}, which belongs to the host")
    for machine in ("kube1", "kube2"):
        checks.expect(not machine_exists(machine), f"reset removed the {machine} machine")

    # A first-boot unit that stays active after its file is gone cannot be started again.
    if bootstrap_was_active:
        checks.expect(unit_property(BOOTSTRAP_UNIT, "ActiveState") != "active",
                      f"reset stopped {BOOTSTRAP_UNIT}, which was active")
    else:
        checks.note(f"{BOOTSTRAP_UNIT} was not active before reset, since the reboot skipped it; "
                    "run `test --no-reboot` to cover stopping it")


# ---------------------------------------------------------------------------------------------
# Commands


def check_prerequisites() -> None:
    missing = [tool for tool in ("docker", "kind", "kubectl", "go", "qemu-system-x86_64", "qemu-img",
                                 "qemu-nbd", "ssh", "ssh-keygen", "ip", "iptables", "nsenter", "setsid")
               if shutil.which(tool) is None]
    if missing:
        die("missing tools: " + ", ".join(missing))
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        die("/dev/kvm is not accessible")
    ovmf_firmware()
    if run(["sudo", "-n", "true"], check=False, capture=True).returncode != 0:
        die("sudo needs a password; run `sudo -v` in this terminal first")


def cmd_up(args: argparse.Namespace) -> None:
    local = Path(args.image).expanduser() if args.image else None
    if local and not local.is_file():
        die(f"image not found: {local}")
    check_prerequisites()
    # Before anything else is set up, so that an export that cannot run fails first.
    image = local or gallery_image()
    stop_vm()
    _, controller = build_binaries()
    public_key = ensure_ssh_key()

    create_network()
    create_cluster(args.node_image)
    install_controller(controller)
    version = kubernetes_version()
    register_machine(version)
    token = create_bootstrap_token()
    kubectl("delete", "node", NODE_NAME, "--ignore-not-found", quiet=True)

    serve_base = f"http://{GATEWAY}:{SERVE_PORT}"
    doc = render_ignition(base_config(token, version), f"{serve_base}/{AGENT_ARCHIVE}",
                          sha256(STATE / AGENT_ARCHIVE))
    ignition = STATE / IGNITION_NAME
    ignition.write_text(json.dumps(add_harness_access(doc, public_key), indent=2) + "\n")
    ignition.chmod(0o600)

    # The config server only runs while the VM provisions. Later boots must not need it.
    with FileServer({IGNITION_NAME: ignition, AGENT_ARCHIVE: STATE / AGENT_ARCHIVE}):
        launch_vm(image, f"{serve_base}/{IGNITION_NAME}")
        wait_for_ssh()
        wait_for_bootstrap()
    wait_for_node_ready()
    log(f"node {NODE_NAME} is Ready; run `{Path(sys.argv[0]).name} test` next")


def cmd_test(args: argparse.Namespace) -> None:
    if not vm_running():
        die("the VM is not running; run `up` first")
    checks = Checks()
    check_first_boot(checks)
    check_workload(checks)
    if args.reboot:
        check_reboot(checks)
    if args.reset != "none":
        bootstrap_was_active = check_before_reset(checks)
        reset_node(args.reset)
        check_reset(checks, bootstrap_was_active)
    checks.done()


def cmd_down(_: argparse.Namespace) -> None:
    stop_vm()
    delete_network()
    if shutil.which("kind") and cluster_exists():
        run(["kind", "delete", "cluster", "--name", CLUSTER])
    shutil.rmtree(STATE, ignore_errors=True)
    log("removed the VM, network, cluster, and state")
    if IMAGE_DIR.is_dir() and any(IMAGE_DIR.glob("acl-gallery-*.qcow2")):
        log(f"kept the exported images in {IMAGE_DIR}; remove it to reclaim the space")


def cmd_ssh(args: argparse.Namespace) -> None:
    os.execvp("ssh", ["ssh", *SSH_OPTS, f"{SSH_USER}@{VM_IP}", *args.command])


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    commands = parser.add_subparsers(dest="command", required=True)

    up = commands.add_parser("up", help="build, create the cluster and network, and boot the VM")
    up.add_argument("--image", default=os.environ.get("ACL_IMAGE", ""),
                    help="Azure Container Linux qcow2 image (or ACL_IMAGE); without one, the "
                         "gallery image is exported to ACL_IMAGE_DIR and booted")
    up.add_argument("--node-image", default=os.environ.get("ACL_KIND_NODE_IMAGE", ""),
                    help="kind node image, for another Kubernetes version")
    up.set_defaults(func=cmd_up)

    test = commands.add_parser("test", help="check the node, reboot it, reset it, and check cleanup")
    test.add_argument("--reset", choices=["agent-reset", "cli", "none"], default="agent-reset",
                      help="how to reset the node at the end (default: an AgentReset MachineOperation)")
    test.add_argument("--no-reboot", dest="reboot", action="store_false",
                      help="skip the reboot, so that reset runs while the first-boot unit is active")
    test.set_defaults(func=cmd_test)

    down = commands.add_parser("down", help="remove the VM, network, cluster, and state")
    down.set_defaults(func=cmd_down)

    ssh_cmd = commands.add_parser("ssh", help="open a shell on the VM, or run a command there")
    ssh_cmd.add_argument("command", nargs=argparse.REMAINDER)
    ssh_cmd.set_defaults(func=cmd_ssh)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
