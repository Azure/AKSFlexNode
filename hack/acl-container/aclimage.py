"""Export the Azure Container Linux gallery image to a local qcow2.

A shared gallery image cannot be downloaded as such: it is copied to a
temporary managed disk, the disk's written pages are read through a read-only
SAS into a sparse VHD, the disk is deleted whether or not that worked, and the
raw disk inside the VHD is converted to qcow2.

The image is saved as <ACL_IMAGE_DIR>/<gallery>/<image>/acl-gallery-<version>.qcow2
and reused while that version is current: a version is only unique within its
gallery image. See README.md for the settings and the Azure permissions the
export needs.
"""

from __future__ import annotations

import concurrent.futures
import datetime
import http.client
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable
from pathlib import Path
from xml.etree import ElementTree

GALLERY_IMAGE = os.environ.get(
    "ACL_IMAGE_GALLERY_IMAGE",
    "/SharedGalleries/035db282-f1c8-4ce7-b78f-2a7265d5398c-ACLDEVEL/Images/acl-1es-eval-kvm",
)
# Where the export's disk is made, which the image has to be replicated to.
GALLERY_LOCATION = os.environ.get("ACL_IMAGE_GALLERY_LOCATION", "westus2")
# A build with the SELinux rule the agent's containers need, that labels containerd's files, so
# they run as spc_t, where the rule applies. It's excluded from the gallery's latest, so it's
# named; an empty ACL_IMAGE_VERSION asks the gallery for its latest.
GALLERY_VERSION = os.environ.get("ACL_IMAGE_VERSION", "3.20261027.1")
EXPORT_RESOURCE_GROUP = os.environ.get("ACL_IMAGE_RESOURCE_GROUP", "")
EXPORT_SUBSCRIPTION = os.environ.get("ACL_IMAGE_SUBSCRIPTION", "")
GALLERY_IMAGE_PATTERN = re.compile(r"/SharedGalleries/([^/]+)/Images/([^/]+)")
GALLERY_VERSION_PATTERN = re.compile(r"[A-Za-z0-9._-]+")
# Gallery and image names, which name the cache's directories.
GALLERY_NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*")
# Export disks carry this tag, and the sweep only ever deletes stale disks that have it.
EXPORT_DISK_TAG = "aks-flex-node-acl-harness"
EXPORT_STALE_AFTER = 6 * 3600
EXPORT_SAS_SECONDS = 1800
EXPORT_CHUNK = 8 << 20
EXPORT_WORKERS = 16
VHD_FOOTER_SIZE = 512


def log(message: str) -> None:
    print(f"[acl-image] {message}", flush=True)


def warn(message: str) -> None:
    print(f"[acl-image] warning: {message}", flush=True)


def die(message: str) -> None:
    print(f"[acl-image] error: {message}", file=sys.stderr, flush=True)
    sys.exit(1)


def run(cmd: list[str], *, capture: bool = False) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(cmd, check=False, text=True,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if result.returncode != 0:
        detail = f": {result.stderr.strip()}" if capture and result.stderr else ""
        die(f"{' '.join(cmd)} exited {result.returncode}{detail}")
    return result


def gallery_image(image_dir: Path) -> Path:
    """Return the gallery image as a qcow2 in image_dir, exporting it unless an earlier run did.

    The file is under the gallery's and the image's names, named for the version, and renamed
    into place only once complete, so an interrupted export never sits under the name that is
    reused, and another gallery image's version never stands in for this one's.
    """
    gallery, image_name = gallery_identity()
    version = gallery_version()
    image_dir = image_dir / gallery / image_name
    image = image_dir / f"acl-gallery-{version}.qcow2"
    if image.exists():
        if subprocess.run(["qemu-img", "check", "-q", "-f", "qcow2", str(image)]).returncode != 0:
            die(f"{image}, exported earlier, fails qemu-img check; remove it to export it again")
        log(f"using {image}, exported earlier")
        return image

    image_dir.mkdir(parents=True, exist_ok=True)
    for leftover in image_dir.glob("acl-gallery-*.qcow2.part*"):
        leftover.unlink(missing_ok=True)
    partial = image.with_name(image.name + ".part")
    export_gallery_image(f"{GALLERY_IMAGE}/Versions/{version}", partial)
    partial.replace(image)
    fd = os.open(image_dir, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    return image


def gallery_identity() -> tuple[str, str]:
    """Return the gallery's and the image's names in ACL_IMAGE_GALLERY_IMAGE."""
    match = GALLERY_IMAGE_PATTERN.fullmatch(GALLERY_IMAGE)
    if not match or not all(GALLERY_NAME_PATTERN.fullmatch(name) for name in match.groups()):
        die(f"ACL_IMAGE_GALLERY_IMAGE={GALLERY_IMAGE!r} is not /SharedGalleries/<gallery>/Images/<image>")
        raise AssertionError("unreachable")
    return match.group(1), match.group(2)


def gallery_version() -> str:
    """Return ACL_IMAGE_VERSION, or the version "latest" names now."""
    if GALLERY_VERSION == "latest":
        die("ACL_IMAGE_VERSION=latest would reuse whichever version was exported first; "
            "set it empty, ACL_IMAGE_VERSION=, for the gallery's latest version")
    version = GALLERY_VERSION
    if not version:
        gallery, image = gallery_identity()
        version = az([
            "sig", "image-version", "show-shared",
            "--gallery-unique-name", gallery,
            "--gallery-image-definition", image,
            "--gallery-image-version", "latest",
            "--location", GALLERY_LOCATION,
            "--query", "name", "-o", "tsv",
        ])
        log(f"gallery version {version} is the latest of {GALLERY_IMAGE}")
    if not GALLERY_VERSION_PATTERN.fullmatch(version):
        die(f"{version!r} is not a gallery image version")
    return version


def az_command(args: list[str]) -> list[str]:
    if shutil.which("az") is None:
        die("az is needed to export the gallery image; install it, or pass --image")
    subscription = ["--subscription", EXPORT_SUBSCRIPTION] if EXPORT_SUBSCRIPTION else []
    return ["az", *args, *subscription, "--only-show-errors"]


def az(args: list[str]) -> str:
    """Run az and return its output. A failure reports az's own error, which never holds a SAS:
    only grant-access prints one, to stdout."""
    result = subprocess.run(az_command(args), check=False, text=True, capture_output=True)
    if result.returncode != 0:
        die(f"az {' '.join(args[:3])} failed (exit {result.returncode}): {result.stderr.strip()[-2000:]}")
    return result.stdout.strip()


def az_best_effort(args: list[str], what: str) -> str | None:
    result = subprocess.run(az_command(args), check=False, text=True, capture_output=True)
    if result.returncode != 0:
        warn(f"could not {what}: {result.stderr.strip()[-500:]}")
        return None
    return result.stdout.strip()


def export_gallery_image(version_id: str, destination: Path) -> None:
    """Write a shared gallery image version to destination as qcow2.

    The footer is cut off and the raw disk converted, rather than the VHD: qemu-img before 10.0
    sizes a VHD Azure wrote from its CHS geometry and loses the GPT's backup header.
    """
    if not EXPORT_RESOURCE_GROUP:
        die("ACL_IMAGE_RESOURCE_GROUP must name the resource group, in "
            f"{GALLERY_LOCATION}, where the gallery image is exported; or pass --image")

    sweep_stale_export_disks()

    disk = f"{EXPORT_DISK_TAG}-local-{secrets.token_hex(3)}"
    vhd = destination.with_name(destination.name + ".vhd")
    started = time.monotonic()
    log(f"exporting {version_id} through disk {disk} in {EXPORT_RESOURCE_GROUP}")

    try:
        try:
            az(["disk", "create", "-g", EXPORT_RESOURCE_GROUP, "-n", disk,
                "--location", GALLERY_LOCATION, "--sku", "Standard_LRS",
                "--gallery-image-reference", version_id,
                "--tags", f"purpose={EXPORT_DISK_TAG}", "run=local",
                "--query", "id", "-o", "tsv"])
            try:
                download_page_blob(grant_export_sas(disk), vhd)
            finally:
                # Azure does not delete a disk while a SAS on it is active.
                az_best_effort(["disk", "revoke-access", "-g", EXPORT_RESOURCE_GROUP, "-n", disk],
                               f"revoke access to disk {disk}")
        finally:
            az_best_effort(["disk", "delete", "-g", EXPORT_RESOURCE_GROUP, "-n", disk, "--yes"],
                           f"delete disk {disk}")

        log("converting the exported disk to qcow2")
        disk_bytes = vhd.stat().st_size - VHD_FOOTER_SIZE
        os.truncate(vhd, disk_bytes)
        run(["qemu-img", "convert", "-f", "raw", "-O", "qcow2", str(vhd), str(destination)])
        run(["qemu-img", "check", "-q", "-f", "qcow2", str(destination)])
        info = json.loads(run(["qemu-img", "info", "-f", "qcow2", "--output=json", str(destination)],
                              capture=True).stdout)
        if info.get("virtual-size") != disk_bytes:
            raise RuntimeError(f"the qcow2 is {info.get('virtual-size')} bytes, the disk {disk_bytes}")
        with destination.open("rb") as converted:
            os.fsync(converted.fileno())
    except RuntimeError as exc:
        destination.unlink(missing_ok=True)
        die(f"exporting {version_id} failed: {exc}")
    except BaseException:
        destination.unlink(missing_ok=True)
        raise
    finally:
        vhd.unlink(missing_ok=True)

    log(f"exported {version_id} in {time.monotonic() - started:.0f}s "
        f"({destination.stat().st_size} bytes)")


def grant_export_sas(disk: str) -> str:
    granted = json.loads(az([
        "disk", "grant-access", "-g", EXPORT_RESOURCE_GROUP, "-n", disk,
        "--access-level", "Read", "--duration-in-seconds", str(EXPORT_SAS_SECONDS), "-o", "json",
    ]))
    sas = granted.get("accessSas") or granted.get("accessSAS") or ""
    if not sas.startswith("https://"):
        die(f"grant-access on disk {disk} returned no SAS")
    return sas


def sweep_stale_export_disks() -> None:
    """Delete export disks that runs which are gone left behind."""
    listed = az_best_effort([
        "disk", "list", "-g", EXPORT_RESOURCE_GROUP,
        "--query", f"[?tags.purpose=='{EXPORT_DISK_TAG}'].{{name:name,created:timeCreated,state:diskState}}",
        "-o", "json",
    ], "list earlier export disks")
    if listed is None:
        return
    cutoff = time.time() - EXPORT_STALE_AFTER
    for disk in json.loads(listed or "[]"):
        try:
            text = re.sub(r"(\.\d{6})\d+", r"\1", disk["created"]).replace("Z", "+00:00")
            created = datetime.datetime.fromisoformat(text).timestamp()
        except (KeyError, TypeError, ValueError):
            warn(f"not sweeping export disk {disk.get('name')!r}: its creation time cannot be read")
            continue
        if created > cutoff:
            continue
        log(f"deleting export disk {disk['name']}, created {disk['created']} by an earlier run")
        if disk.get("state") == "ActiveSAS":
            az_best_effort(["disk", "revoke-access", "-g", EXPORT_RESOURCE_GROUP, "-n", disk["name"]],
                           f"revoke access to disk {disk['name']}")
        az_best_effort(["disk", "delete", "-g", EXPORT_RESOURCE_GROUP, "-n", disk["name"], "--yes"],
                       f"delete disk {disk['name']}")


BlobFetch = Callable[[str, dict[str, str]], tuple[bytes, dict[str, str]]]


def download_page_blob(sas: str, destination: Path, fetch: BlobFetch | None = None) -> None:
    """Download the written pages of a page blob into a sparse file of the blob's size. The SAS
    is on no command line, and no error raised here names it."""
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
    log(f"downloaded {total >> 20} MiB in {elapsed:.0f}s")
    check_fixed_vhd(destination, size)


def page_chunks(ranges: list[tuple[int, int]], chunk: int = EXPORT_CHUNK) -> list[tuple[int, int]]:
    out = []
    for start, end in ranges:
        while start <= end:
            stop = min(end, start + chunk - 1)
            out.append((start, stop))
            start = stop + 1
    return out


def check_fixed_vhd(path: Path, size: int) -> None:
    with path.open("rb") as handle:
        handle.seek(size - VHD_FOOTER_SIZE)
        footer = handle.read(VHD_FOOTER_SIZE)
    cookie, current, kind = footer[:8], int.from_bytes(footer[48:56], "big"), int.from_bytes(footer[60:64], "big")
    if cookie != b"conectix" or kind != 2 or current != size - VHD_FOOTER_SIZE:
        raise RuntimeError(f"the exported disk is not a fixed VHD of {size} bytes "
                           f"(cookie {cookie!r}, type {kind}, size {current})")


def blob_request(url: str, headers: dict[str, str]) -> tuple[bytes, dict[str, str]]:
    """GET from blob storage, retrying what may pass. Errors never name the URL, which carries
    the SAS."""
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
