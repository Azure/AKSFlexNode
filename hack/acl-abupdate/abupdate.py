#!/usr/bin/env python3
"""Drive A/B updates of Azure Container Linux with Trident in a local KVM VM, and check them.

    hack/acl-abupdate/abupdate.py image fetch [TAG...]   pull the installer ISO and COSIs from ACR
    hack/acl-abupdate/abupdate.py image build NAME ...   build a COSI with your files, to update to
    hack/acl-abupdate/abupdate.py up                     install the base image, save snapshot base
    hack/acl-abupdate/abupdate.py test                   run every scenario, each from snapshot base
    hack/acl-abupdate/abupdate.py run SCENARIO...        run scenarios; `scenarios` lists them
    hack/acl-abupdate/abupdate.py update --to TARGET     stage, finalize, and commit one A/B update
    hack/acl-abupdate/abupdate.py status                 the VM, its booted slot and image, Trident's state
    hack/acl-abupdate/abupdate.py down                   stop the VM and remove its state

The flow is the one ACL's KVM images document: the installer ISO streams the base image to the VM's
disk, an Ignition config on a volume labelled ACLSEED provisions SSH access on its first boot, and
`trident update`, given an image-only Host Configuration, writes a later COSI to the inactive A/B
slot. Only the download uses Azure. See README.md for the settings, commands, scenarios, and checks;
scenarios.py defines the scenarios.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import contextlib
import dataclasses
import datetime
import fcntl
import getpass
import hashlib
import io
import json
import os
import re
import secrets
import shlex
import shutil
import signal
import socket
import subprocess
import sys
import tarfile
import textwrap
import time
import urllib.parse
import urllib.request
import uuid
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any, NoReturn

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cosibuild  # noqa: E402
from errors import StepError  # noqa: E402
from scenarios import REGISTRY, Needs, Scenario, Skip  # noqa: E402

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
PROG = "hack/acl-abupdate/abupdate.py"


def _env_int(name: str, default: int) -> int:
    value = os.environ.get(name, "").strip()
    if not value:
        return default
    if not value.isdigit():
        print(f"[acl-ab] error: {name} must be a whole number, not {value!r}", file=sys.stderr)
        sys.exit(2)
    return int(value)


DEFAULT_STATE = REPO / ".vm" / "acl-abupdate"
STATE = Path(os.environ.get("ACL_AB_STATE_DIR", str(DEFAULT_STATE)))
# up makes the state directory, or takes an empty one, and marks it; down removes it only if it's
# the default or marked: ACL_AB_STATE_DIR could name any directory.
STATE_MARKER = STATE / ".acl-ab-state"
STATE_FILE = STATE / "state.json"
LOCK_FILE = STATE / "lock"
VM_DISK = STATE / "disk.qcow2"
# The UEFI variables are qcow2 rather than raw, so QEMU's snapshots include them: BootNext and
# BootOrder are part of what an A/B update changes.
VM_VARS = STATE / "vars.qcow2"
PID_FILE = STATE / "qemu.pid"
QMP_SOCKET = STATE / "qmp.sock"
SERIAL_LOG = STATE / "serial.log"
SNAPSHOT_DIR = STATE / "snapshots"
RUNS_DIR = STATE / "runs"
SSH_KEY = STATE / "ssh" / "id_ed25519"
KUBECONFIG = STATE / "kubeconfig"
# The cluster's spec, written before any of its resources exist, so down can remove them after an up
# that failed before state.json was written.
CLUSTER_FILE = STATE / "cluster.json"
FLEX_DIR = STATE / "flex"
OMAHA_DIR = STATE / "omaha"

# Downloads and installed base disks outlive down; remove the directory to reclaim the space.
CACHE = Path(os.environ.get("ACL_AB_CACHE_DIR", str(REPO / ".vm" / "acl-abupdate-cache")))
IMAGES = CACHE / "images"
INSTALLED = CACHE / "installed"
TOOLS = CACHE / "tools"
BUILDS = CACHE / "builds"
ORAS_AUTH = CACHE / "oras-auth.json"

# Where the images come from: an OCI registry and namespace, and the repositories of the KVM
# installer ISO and of the COSIs, each holding the file as an ORAS artifact's one layer, by build
# tag. There are no defaults: set them, or use local files. up keeps them with the VM's state, so
# later commands don't need them set; see use_images.
REGISTRY_PATH = os.environ.get("ACL_AB_REGISTRY", "")
ISO_REPO = os.environ.get("ACL_AB_ISO_REPO", "")
COSI_REPO = os.environ.get("ACL_AB_COSI_REPO", "")
BASE_TAG = os.environ.get("ACL_AB_BASE_TAG", "")
UPDATE_TAG = os.environ.get("ACL_AB_UPDATE_TAG", "")
# Local files instead of the registry's, for the base install and the base and update targets.
BASE_ISO = os.environ.get("ACL_AB_BASE_ISO", "")
BASE_COSI = os.environ.get("ACL_AB_BASE_COSI", "")
UPDATE_COSI = os.environ.get("ACL_AB_UPDATE_COSI", "")
# The subscription `az acr login` looks the registry up in; az's current one if unset.
SUBSCRIPTION = os.environ.get("ACL_AB_SUBSCRIPTION", "")
# ACR takes this placeholder username with an access token from `az acr login --expose-token`.
ACR_TOKEN_USER = "00000000-0000-0000-0000-000000000000"
# ACR access tokens last three hours; logging in again before then keeps a pull from failing midway.
ACR_TOKEN_SECONDS = 2 * 3600
ORAS_VERSION = "1.3.2"
ORAS_SHA256 = "9229ccc6d17bb282039ad4a69abb16dcb887a5bce567c075d731d9b3c7ad8eaf"
TAG_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
SNAPSHOT_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
# A build's name: lowercase, starting with a letter, so it's never a build tag, and usable in a URL
# path and in the semver version the update server offers it as.
BUILD_NAME_PATTERN = re.compile(r"[a-z][a-z0-9-]{0,39}")
# The Image Customizer container that `image build` runs; see cosibuild.py.
IMAGE_CUSTOMIZER = os.environ.get("ACL_AB_IMAGE_CUSTOMIZER", cosibuild.DEFAULT_IMAGE_CUSTOMIZER)

VM_MEMORY = _env_int("ACL_AB_VM_MEMORY", 6144)
VM_CPUS = _env_int("ACL_AB_VM_CPUS", 2)
DISK_SIZE = os.environ.get("ACL_AB_DISK_SIZE", "64G")
SSH_PORT = _env_int("ACL_AB_SSH_PORT", 2222)
# Where COSIs are copied in the VM. /tmp may be in memory, and a COSI is gigabytes.
GUEST_DIR = os.environ.get("ACL_AB_GUEST_DIR", "/var/tmp/acl-abupdate")
INSTALL_TIMEOUT = _env_int("ACL_AB_INSTALL_TIMEOUT", 2400)
BOOT_TIMEOUT = _env_int("ACL_AB_BOOT_TIMEOUT", 900)
STAGE_TIMEOUT = _env_int("ACL_AB_STAGE_TIMEOUT", 3600)
COMMAND_TIMEOUT = 600
SSH_USER = "core"

# Cluster mode, `up --cluster`: a kind cluster on root Podman, which the VM joins as a flex node
# with the agent in containers. See README.md.
KIND_CLUSTER = os.environ.get("ACL_AB_KIND_CLUSTER", "aclab")
KIND_NODE_IMAGE = os.environ.get("ACL_AB_KIND_NODE_IMAGE", "")
CLUSTER_SUBNET = os.environ.get("ACL_AB_CLUSTER_SUBNET", "192.168.113")
LOCAL_REGISTRY_PORT = _env_int("ACL_AB_LOCAL_REGISTRY_PORT", 5097)
OMAHA_PORT = _env_int("ACL_AB_OMAHA_PORT", 8297)
JOIN_TIMEOUT = _env_int("ACL_AB_JOIN_TIMEOUT", 1200)
READY_TIMEOUT = _env_int("ACL_AB_READY_TIMEOUT", 300)
LOCAL_REGISTRY = "aclab-registry"
AGENT_IMAGE_TAG = "localhost/aks-flex-node-agent:acl-abupdate"
AGENT_REPOSITORY = "aks-flex-node-agent"
# How the VM reaches the host's 127.0.0.1, through QEMU's user networking.
GUEST_HOST = "10.0.2.2"
# Annotation-driven updates: what the harness, as orchestrator, puts in its requests. Nebraska keys
# applications by UUID, and the docs' example pins a track per version, so the harness does too.
ANNOTATION_APP_ID = "5c1e9a3e-3d2b-4e71-9f0c-acab00da7e01"
# A slow download for annotation-heartbeat: the stage takes about this long, a few of the agent's
# 60s heartbeats.
ANNOTATION_HEARTBEAT_STAGE_SECONDS = 200
# How long to give the agent to act on a request that it shouldn't act on, before checking it didn't.
ANNOTATION_SETTLE_SECONDS = 20
# The contract's status annotations, under the prefix the drop-in pins.
STATUS_KEY = "update-status"
COMMIT_STATUS_KEY = "update-commit-status"
# The agent finalizes, reboots, and commits after the boot, within its own finalize timeout.
ANNOTATION_REBOOT_TIMEOUT = 20 * 60
# Where the VM trusts the update server's CA.
OMAHA_CA_ANCHOR = "/etc/pki/ca-trust/source/anchors/acl-abupdate-omaha-ca.pem"
OMAHA_CA_CERTS = "/etc/pki/tls/certs/acl-abupdate-omaha-ca.pem"
SEED_LABEL = "ACLSEED"
HEALTH_CHECK_NAME = "acl-abupdate-forced-failure"
# Written before each update, and checked after it, as the agent's state would have to survive:
# it is under /etc, /opt, and /var (docs/usage/azure-container-linux.md#os-updates).
MARKER_PATHS = (
    "/etc/acl-abupdate/marker",
    "/var/lib/acl-abupdate/marker",
    "/opt/acl-abupdate/marker",
    "/home/core/.acl-abupdate-marker",
)
# Trident's agent for annotation-driven A/B updates, which the seed enables with this drop-in.
TRIDENT_AGENT_UNIT = "trident-acl-agent.service"
TRIDENT_AGENT_DROPIN = HERE / "trident-acl-agent-override.conf"
TRIDENT_AGENT_DROPIN_PATH = f"/etc/systemd/system/{TRIDENT_AGENT_UNIT}.d/{TRIDENT_AGENT_DROPIN.name}"
TRIDENT_AGENT_KUBECONFIG_VARIABLE = "TRIDENT_ACL_AGENT_KUBERNETES_KUBECONFIG"
# The VM's hostname, which the seed sets: the image's is localhost. The flex agent names the node
# after it, and so does trident-acl-agent by default.
NODE_NAME = "acl-abupdate"
# The second NIC, on the cluster's bridge, which only `up --cluster` attaches. QEMU's user NIC keeps
# QEMU's default MAC, and stays first, as eth0.
CLUSTER_NIC_MAC = "52:54:00:ab:71:0a"
CLUSTER_NETWORK = "/etc/systemd/network/10-acl-abupdate-cluster.network"
# The image's catch-all network runs DHCP on every link, and systemd-networkd-wait-online is
# enabled, so a NIC this file doesn't claim on its first boot would hold boot up waiting for a lease
# that never comes. Its address is a drop-in the harness writes when the VM joins a cluster.
CLUSTER_NETWORK_CONTENTS = f"""\
[Match]
MACAddress={CLUSTER_NIC_MAC}

[Link]
RequiredForOnline=no

[Network]
DHCP=no
LinkLocalAddressing=no
IPv6AcceptRA=no
"""
# Trident's servicingState values after a finalize reboot, which commit takes to provisioned.
FINALIZED_STATES = ("ab-update-finalized", "manual-rollback-ab-finalized")
HEALTH_FAILED_STATE = "ab-update-health-check-failed"
EFI_GLOBAL_GUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

OVMF_CANDIDATES = [
    ("/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"),
    ("/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"),
    ("/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"),
    ("/usr/share/qemu/ovmf-x86_64-code.bin", "/usr/share/qemu/ovmf-x86_64-vars.bin"),
]

SSH_OPTS = [
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "LogLevel=ERROR",
    "-o", "ConnectTimeout=10",
    "-o", "BatchMode=yes",
    "-o", "IdentitiesOnly=yes",
    "-o", "ServerAliveInterval=10",
    "-o", "ServerAliveCountMax=6",
    "-i", str(SSH_KEY),
]

HEALTH_CHECK_CONFIG = f"""\
health:
  checks:
  - name: {HEALTH_CHECK_NAME}
    runOn:
    - ab-update
    content: |
      echo "{HEALTH_CHECK_NAME}: this check fails on purpose"
      exit 1
"""


# ---------------------------------------------------------------------------------------------
# Process helpers

_json_output = False
_ssh_port = SSH_PORT


def _stream():
    return sys.stderr if _json_output else sys.stdout


def log(message: str) -> None:
    print(f"[acl-ab] {message}", file=_stream(), flush=True)


def warn(message: str) -> None:
    """Report a problem that doesn't fail the run. The message may hold another program's output.
    In GitHub Actions every line of the log is read for workflow commands, so it is printed once,
    escaped, as a warning, where a line break in it can't start a command."""
    if os.environ.get("GITHUB_ACTIONS") == "true":
        escaped = message.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
        print(f"::warning::{escaped}", file=_stream(), flush=True)
    else:
        print(f"[acl-ab] warning: {message}", file=_stream(), flush=True)


def die(message: str, code: int = 2) -> NoReturn:
    print(f"[acl-ab] error: {message}", file=sys.stderr, flush=True)
    sys.exit(code)


def run(cmd: list[str], *, check: bool = True, capture: bool = False,
        input: str | None = None) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(cmd, check=False, text=True, input=input,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if check and result.returncode != 0:
        detail = f": {last_lines(result.stderr)}" if capture and result.stderr else ""
        raise StepError(f"{' '.join(cmd)} exited {result.returncode}{detail}")
    return result


def host_run(cmd: list[str], *, input: str | None = None, timeout: float | None = None,
             stream: bool = False) -> subprocess.CompletedProcess[str]:
    """The `run` the cluster modules take: it never raises, and leaves checking the status to them.
    With stream, the output goes where the log does, for long builds and pulls."""
    try:
        if stream:
            return subprocess.run(cmd, text=True, input=input, timeout=timeout, stdout=_stream(),
                                  stderr=subprocess.STDOUT, check=False)
        return subprocess.run(cmd, text=True, input=input, timeout=timeout, capture_output=True, check=False)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(cmd, 124, "", f"timed out after {duration(timeout or 0)}")
    except FileNotFoundError as error:
        return subprocess.CompletedProcess(cmd, 127, "", str(error))


def poll(timeout: float, probe: Callable[[], bool], interval: float = 5) -> bool:
    """Return whether probe returned true within the timeout."""
    deadline = time.monotonic() + timeout
    while True:
        if probe():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(interval)


def duration(seconds: float) -> str:
    minutes, seconds = divmod(round(seconds), 60)
    return f"{minutes}m{seconds:02d}s" if minutes else f"{seconds}s"


def human_size(size: int) -> str:
    value = float(size)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{size} B"
        value /= 1024
    return f"{size} B"


def last_lines(text: str, count: int = 12) -> str:
    return "\n".join((text or "").strip().splitlines()[-count:])


def first_line(text: str) -> str:
    lines = (text or "").strip().splitlines()
    return lines[0] if lines else ""


def now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def short(digest: str | None) -> str:
    return (digest or "")[:12] or "none"


# ---------------------------------------------------------------------------------------------
# Parsing

# The value parse_yaml gives a key with nested content.
BLOCK = "<block>"
_YAML_KEY = re.compile(r"""^("(?:[^"\\]|\\.)*"|'(?:[^']|'')*'|[^\s#'"-][^:]*?|-[^\s][^:]*?)\s*:(?:\s+(.*))?$""")
_BLOCK_SCALARS = {"|", "|-", "|+", ">", ">-", ">+"}


def yaml_scalar(value: str) -> str | None:
    value = value.strip()
    if value.startswith('"') and value.endswith('"') and len(value) >= 2:
        try:
            return str(json.loads(value))
        except json.JSONDecodeError:
            return value[1:-1]
    if value.startswith("'") and value.endswith("'") and len(value) >= 2:
        return value[1:-1].replace("''", "'")
    if " #" in value:
        value = value.split(" #", 1)[0].rstrip()
    return None if value in ("", "~", "null", "Null", "NULL") else value


def parse_yaml(text: str) -> dict[str, str | None]:
    """Flatten a YAML document's mappings to dotted keys, as far as reading Trident's Host Status
    needs: scalars, nested mappings, and block scalars. Sequences are skipped. A key with nested
    content or a sequence maps to BLOCK, and a key with neither to None. Python has no YAML parser
    of its own, and the harness uses the standard library only."""
    flat: dict[str, str | None] = {}
    parents: set[str] = set()
    stack: list[tuple[int, str]] = []
    sequence_indent: int | None = None
    block: tuple[str, int] | None = None
    block_lines: list[str] = []

    def path_of(entries: list[tuple[int, str]]) -> str:
        return ".".join(key for _, key in entries)

    for line in text.splitlines():
        stripped = line.strip()
        indent = len(line) - len(line.lstrip(" "))
        if block is not None:
            if not stripped or indent > block[1]:
                block_lines.append(line)
                continue
            flat[block[0]] = textwrap.dedent("\n".join(block_lines)).strip("\n")
            block = None
        if not stripped or stripped.startswith("#") or stripped in ("---", "..."):
            continue
        is_item = stripped == "-" or stripped.startswith("- ")
        if sequence_indent is not None:
            if indent > sequence_indent or (indent == sequence_indent and is_item):
                continue
            sequence_indent = None
        if is_item:
            # A sequence belongs to the key above it, whose indent may equal the items'.
            while stack and stack[-1][0] > indent:
                stack.pop()
            if stack:
                parents.add(path_of(stack))
            sequence_indent = indent
            continue
        while stack and stack[-1][0] >= indent:
            stack.pop()
        match = _YAML_KEY.match(stripped)
        if not match:
            continue
        key = yaml_scalar(match.group(1)) or ""
        if stack:
            parents.add(path_of(stack))
        path = ".".join([*(k for _, k in stack), key])
        value = (match.group(2) or "").strip()
        if value in _BLOCK_SCALARS:
            block, block_lines = (path, indent), []
        elif value == "":
            flat[path] = BLOCK
            stack.append((indent, key))
        else:
            flat[path] = yaml_scalar(value)
    if block is not None:
        flat[block[0]] = textwrap.dedent("\n".join(block_lines)).strip("\n")
    return {key: None if value == BLOCK and key not in parents else value for key, value in flat.items()}


def yaml_block(text: str, key: str) -> str:
    """The text of a top-level key's value, for showing a nested value such as lastError."""
    lines: list[str] = []
    inside = False
    for line in text.splitlines():
        if inside:
            if line and not line[0].isspace() and not line.startswith("- "):
                break
            lines.append(line)
        elif line.startswith(f"{key}:"):
            rest = line[len(key) + 1:].strip()
            if rest:
                return rest
            inside = True
    return textwrap.dedent("\n".join(lines)).strip()


def parse_sections(text: str) -> dict[str, str]:
    """Split the probe script's output into its `### name` sections."""
    sections: dict[str, list[str]] = {}
    current: list[str] | None = None
    for line in text.splitlines():
        if line.startswith("### "):
            current = sections.setdefault(line[4:].strip(), [])
        elif current is not None:
            current.append(line)
    return {name: "\n".join(lines).strip("\n") for name, lines in sections.items()}


def parse_verity(text: str) -> dict[str, str]:
    """`veritysetup status`'s fields, keyed by their lowercased names: data device, hash device,
    root hash, status."""
    fields: dict[str, str] = {}
    for line in text.splitlines():
        key, separator, value = line.strip().partition(":")
        if separator and key and value.strip():
            fields[key.strip().lower()] = value.strip()
    return fields


def parse_os_release(text: str) -> dict[str, str]:
    values: dict[str, str] = {}
    for line in text.splitlines():
        key, separator, value = line.strip().partition("=")
        if not separator or not key or key.startswith("#"):
            continue
        try:
            parts = shlex.split(value)
        except ValueError:
            parts = [value.strip("\"'")]
        values[key] = parts[0] if parts else ""
    return values


def parse_properties(text: str) -> dict[str, str]:
    """KEY=value lines, as `systemctl show` prints them."""
    return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)


def split_environment(assignments: str) -> dict[str, str]:
    """VAR=value assignments, separated by spaces and quoted as systemd quotes them."""
    try:
        words = shlex.split(assignments)
    except ValueError:
        words = assignments.split()
    return dict(word.split("=", 1) for word in words if "=" in word)


def unit_environment(text: str) -> dict[str, str]:
    """The variables a unit file or drop-in sets with Environment=."""
    values: dict[str, str] = {}
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("Environment="):
            values.update(split_environment(line[len("Environment="):]))
    return values


def efi_payload(hex_value: str) -> bytes:
    """A UEFI variable's data, from efivarfs's hex, which starts with 4 bytes of attributes."""
    try:
        return bytes.fromhex(hex_value)[4:]
    except ValueError:
        return b""


def efi_boot_number(hex_value: str) -> str:
    data = efi_payload(hex_value)
    return f"{int.from_bytes(data[:2], 'little'):04X}" if len(data) >= 2 else ""


def efi_boot_order(hex_value: str) -> list[str]:
    data = efi_payload(hex_value)
    return [f"{int.from_bytes(data[i:i + 2], 'little'):04X}" for i in range(0, len(data) - 1, 2)]


def efi_load_option_description(hex_value: str) -> str:
    """The description of a Boot#### variable's EFI_LOAD_OPTION: a null-terminated UCS-2 string
    after its 4-byte attributes and 2-byte file path list length."""
    data = efi_payload(hex_value)[6:]
    for end in range(0, len(data) - 1, 2):
        if data[end:end + 2] == b"\0\0":
            return data[:end].decode("utf-16-le", "replace")
    return ""


def iso_volume_id(path: Path) -> str | None:
    """The volume identifier of an ISO 9660 image's primary volume descriptor, or None if path
    isn't one."""
    with path.open("rb") as image:
        image.seek(0x8000)
        descriptor = image.read(72)
    if len(descriptor) < 72 or descriptor[1:6] != b"CD001":
        return None
    return descriptor[40:72].decode("ascii", "replace").strip()


def read_cosi(path: Path) -> dict[str, Any]:
    """What the harness needs of a COSI (docs/Reference/Composable-OS-Image.md in Trident's
    repository): the SHA-384 of its metadata.json, which a Host Configuration's image.sha384
    names, and the verity root hashes of its filesystems, which identify the image booted."""
    with path.open("rb") as archive:
        if archive.read(11) != b"cosi-marker":
            raise ValueError(f"{path} isn't a COSI: it doesn't start with the cosi-marker entry")
    raw = None
    with tarfile.open(path, "r:") as tar:
        # metadata.json is the second entry, so this reads no further.
        for member in tar:
            if member.name == "metadata.json" and member.isfile():
                extracted = tar.extractfile(member)
                raw = extracted.read() if extracted else None
                break
    if raw is None:
        raise ValueError(f"{path} has no metadata.json")
    metadata = json.loads(raw)
    roothashes = {}
    for image in metadata.get("images") or []:
        verity = image.get("verity") or {}
        if verity.get("roothash"):
            roothashes[str(image.get("mountPoint", ""))] = str(verity["roothash"])
    os_release = parse_os_release(str(metadata.get("osRelease", "")))
    return {
        "metadataSha384": hashlib.sha384(raw).hexdigest(),
        "cosiVersion": str(metadata.get("version", "")),
        "osArch": str(metadata.get("osArch", "")),
        "roothashes": roothashes,
        "roothash": roothashes.get("/usr") or roothashes.get("/", ""),
        "osRelease": {key: os_release[key] for key in ("NAME", "VERSION", "VERSION_ID", "BUILD_ID", "PRETTY_NAME")
                      if key in os_release},
    }


# ---------------------------------------------------------------------------------------------
# Images


@dataclasses.dataclass(frozen=True)
class Image:
    """A downloaded or local installer ISO or COSI."""

    name: str
    path: Path
    kind: str
    info: dict[str, Any] = dataclasses.field(default_factory=dict, compare=False, hash=False)

    @property
    def roothash(self) -> str:
        return str(self.info.get("roothash", ""))

    @property
    def sha384(self) -> str:
        return str(self.info.get("metadataSha384", ""))

    @property
    def version(self) -> str:
        release = self.info.get("osRelease") or {}
        return str(release.get("VERSION") or release.get("VERSION_ID") or "")

    @property
    def build(self) -> str:
        return str((self.info.get("annotations") or {}).get("com.microsoft.acl.buildNumber", ""))

    @property
    def built(self) -> dict[str, Any]:
        """What `image build` recorded of the build this image is, or nothing for any other image."""
        return self.info.get("built") or {}

    def describe(self) -> str:
        parts = [f"{self.kind}", human_size(int(self.info.get("size", 0)))]
        if self.build:
            parts.append(f"build {self.build}")
        if self.built:
            parts.append(f"built from {self.built.get('from', {}).get('name', '?')}, version {self.built.get('version', '?')}")
        if self.kind == "cosi":
            parts += [f"OS {self.version or '?'}", f"/usr root hash {short(self.roothash)}"]
        return f"{self.name}: {', '.join(parts)}"


def describe_file(path: Path, kind: str) -> dict[str, Any]:
    info: dict[str, Any] = {"file": path.name, "size": path.stat().st_size}
    if kind == "cosi":
        try:
            info.update(read_cosi(path))
        except (ValueError, OSError, tarfile.TarError, json.JSONDecodeError) as error:
            raise StepError(f"can't read {path} as a COSI: {error}") from error
        if info["osArch"].lower() not in ("x86_64", "amd64", "x64"):
            raise StepError(f"{path.name} is for {info['osArch']}; the VM is x86_64")
    elif iso_volume_id(path) is None:
        raise StepError(f"{path.name} isn't an ISO 9660 image")
    return info


_local_images: dict[Path, Image] = {}


def local_image(path: Path, kind: str, name: str = "") -> Image:
    path = path.expanduser().resolve()
    if path not in _local_images:
        if not path.is_file():
            raise StepError(f"{path} doesn't exist")
        _local_images[path] = Image(name or path.name, path, kind, describe_file(path, kind))
    return _local_images[path]


def cached_image(directory: Path, kind: str) -> Image | None:
    try:
        info = json.loads((directory / "info.json").read_text())
    except (OSError, json.JSONDecodeError):
        return None
    path = directory / str(info.get("file", ""))
    return Image(directory.name, path, kind, info) if path.is_file() else None


def oras_binary() -> str:
    """oras from PATH, or ORAS_VERSION downloaded to the cache and checked against its digest."""
    found = shutil.which("oras")
    if found:
        return found
    binary = TOOLS / f"oras-{ORAS_VERSION}" / "oras"
    if binary.is_file():
        return str(binary)
    url = (f"https://github.com/oras-project/oras/releases/download/v{ORAS_VERSION}/"
           f"oras_{ORAS_VERSION}_linux_amd64.tar.gz")
    log(f"oras isn't on PATH; downloading ORAS {ORAS_VERSION} to {binary.parent}")
    try:
        with urllib.request.urlopen(url, timeout=300) as response:
            archive = response.read()
    except OSError as error:
        raise StepError(f"couldn't download ORAS from {url}: {error}; install it "
                        "(https://oras.land/docs/installation)") from error
    digest = hashlib.sha256(archive).hexdigest()
    if digest != ORAS_SHA256:
        raise StepError(f"the ORAS download's sha256 is {digest}, not {ORAS_SHA256}")
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as tar:
        member = tar.extractfile("oras")
        if member is None:
            raise StepError("the ORAS archive has no oras binary")
        content = member.read()
    binary.parent.mkdir(parents=True, exist_ok=True)
    partial = binary.with_suffix(".partial")
    partial.write_bytes(content)
    partial.chmod(0o755)
    os.replace(partial, binary)
    return str(binary)


def oras(*args: str, capture: bool = True) -> subprocess.CompletedProcess[str]:
    # A registry config of the harness's own keeps the token out of ~/.docker/config.json.
    command = [oras_binary(), *args, "--registry-config", str(ORAS_AUTH)]
    if capture:
        return subprocess.run(command, text=True, capture_output=True, check=False)
    # The progress goes where the log does, so --json leaves stdout to the result.
    return subprocess.run(command, text=True, stdout=_stream(), check=False)


def registry_host() -> str:
    return REGISTRY_PATH.split("/", 1)[0]


def missing_settings(names: list[str]) -> str:
    """The error for image settings that aren't set, which have no defaults."""
    names = [name for name in names if name]
    return (f"set {', '.join(names)}: the images' registry, repositories, and tags have no defaults; see "
            "Settings in hack/acl-abupdate/README.md. Local files can stand in, with ACL_AB_BASE_ISO, "
            "ACL_AB_BASE_COSI, and ACL_AB_UPDATE_COSI")


def use_images(state: State) -> None:
    """Take the image settings the VM was brought up with, so later commands don't need them set.
    Those up didn't record, as a VM brought up before it did, come from the environment."""
    global REGISTRY_PATH, ISO_REPO, COSI_REPO
    REGISTRY_PATH = state.registry or REGISTRY_PATH
    ISO_REPO = state.iso_repo or ISO_REPO
    COSI_REPO = state.cosi_repo or COSI_REPO


def acr_login() -> None:
    """Log oras in to the registry with an ACR token from az, passed on stdin."""
    host = registry_host()
    if not shutil.which("az"):
        raise StepError(f"az isn't installed; it gets the token oras pulls from {host} with")
    command = ["az", "acr", "login", "--name", host.split(".", 1)[0], "--expose-token",
               "--query", "accessToken", "--output", "tsv"]
    if SUBSCRIPTION:
        command += ["--subscription", SUBSCRIPTION]
    log(f"getting an ACR token for {host} from az")
    result = run(command, check=False, capture=True)
    token = result.stdout.strip()
    if result.returncode != 0 or not token:
        raise StepError(f"az couldn't get a token for {host}: {last_lines(result.stderr, 4)}\n"
                        "Log in with `az login` to the tenant the registry is in, and set ACL_AB_SUBSCRIPTION "
                        "if it isn't in az's current subscription")
    CACHE.mkdir(parents=True, exist_ok=True)
    if not ORAS_AUTH.exists():
        with os.fdopen(os.open(ORAS_AUTH, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
            out.write("{}\n")
    login = subprocess.run([oras_binary(), "login", host, "--registry-config", str(ORAS_AUTH),
                            "--username", ACR_TOKEN_USER, "--password-stdin"],
                           input=token, text=True, capture_output=True, check=False)
    if login.returncode != 0:
        raise StepError(f"oras login {host} failed: {last_lines(login.stderr, 4)}")


_AUTH_ERROR = re.compile(r"unauthori[sz]ed|denied|\b401\b|\b403\b|credential|authenticat", re.IGNORECASE)


def oras_remote(*args: str) -> subprocess.CompletedProcess[str]:
    """Run an oras command against the registry, logging in first if the token may have expired,
    and again if the registry refuses it."""
    fresh = ORAS_AUTH.is_file() and time.time() - ORAS_AUTH.stat().st_mtime < ACR_TOKEN_SECONDS
    if not fresh:
        acr_login()
    result = oras(*args)
    if result.returncode != 0 and _AUTH_ERROR.search(result.stderr):
        acr_login()
        result = oras(*args)
    return result


def fetch_artifact(repo: str, tag: str, kind: str, *, refresh: bool = False) -> Image:
    """Pull a tag's ISO or COSI into the cache, unless it's there. The artifact holds the file
    itself as its one layer, so oras pull gives it as it was built."""
    if not TAG_PATTERN.fullmatch(tag):
        raise StepError(f"{tag!r} isn't an image tag")
    if not REGISTRY_PATH or not repo:
        raise StepError(missing_settings(["ACL_AB_REGISTRY" if not REGISTRY_PATH else "",
                                          ("ACL_AB_ISO_REPO" if kind == "iso" else "ACL_AB_COSI_REPO") if not repo else ""]))
    directory = IMAGES / repo / tag
    if not refresh:
        cached = cached_image(directory, kind)
        if cached:
            return cached
    reference = f"{REGISTRY_PATH}/{repo}:{tag}"
    manifest = oras_remote("manifest", "fetch", reference)
    if manifest.returncode != 0:
        detail = last_lines(manifest.stderr, 4)
        if "not found" in detail.lower():
            raise StepError(f"{reference} isn't published: {detail}\n"
                            f"List the tags with `oras repo tags {REGISTRY_PATH}/{repo}`")
        raise StepError(f"oras couldn't fetch the manifest of {reference}: {detail}")
    try:
        annotations = json.loads(manifest.stdout).get("annotations") or {}
    except (json.JSONDecodeError, AttributeError):
        annotations = {}
    partial = directory.with_name(f"{tag}.partial")
    shutil.rmtree(partial, ignore_errors=True)
    partial.mkdir(parents=True)
    (partial / "manifest.json").write_text(manifest.stdout)
    build = annotations.get("com.microsoft.acl.buildNumber", "")
    log(f"pulling {reference}" + (f" (build {build})" if build else ""))
    pulled = oras("pull", "--output", str(partial), reference, capture=False)
    if pulled.returncode != 0:
        raise StepError(f"oras pull {reference} exited {pulled.returncode}")
    files = sorted(path for path in partial.iterdir() if path.is_file() and path.suffix == f".{kind}")
    if len(files) != 1:
        raise StepError(f"{reference} should hold one .{kind} file; it holds "
                        f"{', '.join(path.name for path in partial.iterdir())}")
    info = describe_file(files[0], kind)
    info.update(reference=reference, annotations=annotations, pulled=now_iso())
    (partial / "info.json").write_text(json.dumps(info, indent=2) + "\n")
    shutil.rmtree(directory, ignore_errors=True)
    partial.rename(directory)
    image = cached_image(directory, kind)
    if image is None:
        raise StepError(f"the pull of {reference} left no image in {directory}")
    log(f"pulled {image.describe()}")
    return image


def base_iso(tag: str, *, refresh: bool = False) -> Image:
    if BASE_ISO:
        return local_image(Path(BASE_ISO), "iso", tag)
    return fetch_artifact(ISO_REPO, tag, "iso", refresh=refresh)


def cosi(tag: str, state: State | None = None, *, refresh: bool = False) -> Image:
    local = local_cosi(tag, state)
    return local if local else fetch_artifact(COSI_REPO, tag, "cosi", refresh=refresh)


def local_cosi(tag: str, state: State | None = None) -> Image | None:
    """The COSI ACL_AB_BASE_COSI or ACL_AB_UPDATE_COSI names for a tag, if either does."""
    base_tag, update_tag = (state.base_tag, state.update_tag) if state else (BASE_TAG, UPDATE_TAG)
    if BASE_COSI and tag == base_tag:
        return local_image(Path(BASE_COSI), "cosi", tag)
    if UPDATE_COSI and tag == update_tag:
        return local_image(Path(UPDATE_COSI), "cosi", tag)
    return None


def cosi_if_present(tag: str, state: State | None = None) -> Image | None:
    """A tag's COSI if it's local or cached, without pulling it."""
    local = local_cosi(tag, state)
    if local or not COSI_REPO:
        return local
    return cached_image(IMAGES / COSI_REPO / tag, "cosi")


def build_image(name: str) -> Image | None:
    """The build of that name, if `image build` made one."""
    return cached_image(BUILDS / name, "cosi") if BUILD_NAME_PATTERN.fullmatch(name) else None


def known_builds() -> list[Image]:
    builds = [build_image(directory.name) for directory in sorted(BUILDS.iterdir())] if BUILDS.is_dir() else []
    return [image for image in builds if image]


def offer_version(image: Image) -> str | None:
    """The version the update server offers an image as: a build's own, or None for its tag's."""
    return str(image.built.get("version") or "") or None


def package_for(target: Image) -> Any:
    """The update server's package for a target."""
    import omaha

    return omaha.Package.for_cosi(target.name, target.path, target.sha384, offer_version(target))


def build_version(name: str, source: Image, previous: Image | None) -> tuple[str, int]:
    """A build's semver version, and which build of its name it is: its source's version, as the
    update server offers that, with -NAME.N for the Nth build of NAME, so each is offered as its own."""
    import omaha

    count = int(previous.built.get("count", 0) if previous else 0) + 1
    core = offer_version(source)
    if not core:
        try:
            core = omaha.tag_to_semver(source.name)
        except StepError:
            core = source.version if omaha.is_semver(source.version) else "0.0.0"
    core = re.split(r"[-+]", core, maxsplit=1)[0]
    return f"{core}-{name}.{count}", count


def build_os_version(name: str, source: Image, count: int) -> str:
    """The VERSION_ID a build gets in os-release: its source's, with -NAME.N as in build_version, so
    an update to the build changes the OS version, as one to a new release of ACL does. Empty when
    the source has no semver VERSION_ID, and the build keeps its source's os-release."""
    import omaha

    release = source.info.get("osRelease") or {}
    core = re.split(r"[-+]", str(release.get("VERSION_ID", "")), maxsplit=1)[0]
    return f"{core}-{name}.{count}" if omaha.is_semver(core) else ""


def known_cosis() -> list[Image]:
    """The COSIs the harness has seen: the cache's, builds, and local ones, to name the booted image."""
    images: list[Image] = []
    repo = IMAGES / COSI_REPO
    if COSI_REPO and repo.is_dir():
        for directory in sorted(repo.iterdir()):
            if directory.is_dir() and not directory.name.endswith(".partial"):
                image = cached_image(directory, "cosi")
                if image:
                    images.append(image)
    images += known_builds()
    for value, tag in ((BASE_COSI, BASE_TAG), (UPDATE_COSI, UPDATE_TAG)):
        if value:
            with contextlib.suppress(StepError):
                local_image(Path(value), "cosi", tag)
    images += [image for image in _local_images.values() if image.kind == "cosi"]
    return images


def image_name(roothash: str) -> str:
    if not roothash:
        return "unknown"
    for image in known_cosis():
        if image.roothash == roothash:
            return image.name
    return f"unknown image ({short(roothash)})"


def pick_other(roothash: str, base: Image, update: Image) -> Image:
    """The one of base and update that the VM doesn't run."""
    if roothash == base.roothash:
        return update
    if roothash == update.roothash:
        return base
    raise StepError(f"the VM runs neither {base.name} nor {update.name} (/usr root hash "
                    f"{short(roothash)}), so `other` names neither; name a target")


def resolve_target(spec: str, state: State, current_roothash: Callable[[], str]) -> Image:
    """A target: base, update, other (whichever of those the VM doesn't run), a build, a tag, or a COSI."""
    if spec == "base":
        return cosi(state.base_tag, state)
    if spec == "update":
        return cosi(state.update_tag, state)
    if spec == "other":
        return pick_other(current_roothash(), cosi(state.base_tag, state), cosi(state.update_tag, state))
    built = build_image(spec)
    if built:
        return built
    path = Path(spec).expanduser()
    if spec.endswith(".cosi") or "/" in spec or path.is_file():
        return local_image(path, "cosi")
    if TAG_PATTERN.fullmatch(spec):
        return cosi(spec, state)
    raise StepError(f"{spec!r} isn't a target: name base, update, other, a build, a tag, or a .cosi file")


# ---------------------------------------------------------------------------------------------
# Ignition seed


def ignition_config(public_key: str) -> dict[str, Any]:
    """The first-boot config ACL's KVM images document: user core, with the harness's key, in sudo and
    systemd-journal, and passwordless sudo, which is fine for this disposable VM. It also sets the
    hostname, claims the cluster NIC, and enables trident-acl-agent.service with the harness's
    drop-in. Ignition writes the drop-in before systemd starts any unit, so the agent never runs
    without it."""
    return {
        "ignition": {"version": "3.4.0"},
        "passwd": {"users": [{
            "name": SSH_USER,
            "uid": 500,
            "homeDir": f"/home/{SSH_USER}",
            "shell": "/bin/bash",
            "groups": ["sudo", "systemd-journal"],
            "sshAuthorizedKeys": [public_key],
        }]},
        "storage": {"files": [
            {
                "path": f"/etc/sudoers.d/90-{SSH_USER}",
                "mode": 0o440,
                "contents": {"source": f"data:,{SSH_USER}%20ALL%3D%28ALL%29%20NOPASSWD%3A%20ALL%0A"},
            },
            {
                "path": "/etc/hostname",
                "mode": 0o644,
                "overwrite": True,
                "contents": {"source": "data:," + urllib.parse.quote(f"{NODE_NAME}\n")},
            },
            {
                "path": CLUSTER_NETWORK,
                "mode": 0o644,
                "contents": {"source": "data:," + urllib.parse.quote(CLUSTER_NETWORK_CONTENTS)},
            },
        ]},
        "systemd": {"units": [{
            "name": TRIDENT_AGENT_UNIT,
            "enabled": True,
            "dropins": [{"name": TRIDENT_AGENT_DROPIN.name, "contents": TRIDENT_AGENT_DROPIN.read_text()}],
        }]},
    }


def seed_digest() -> str:
    """A digest of what the seed provisions, leaving out the SSH key, which each install makes.
    The image reads the seed on its first boot only, so a cached install whose digest differs has
    to be installed again."""
    return hashlib.sha256(json.dumps(ignition_config(""), sort_keys=True).encode()).hexdigest()


def make_iso(source: Path, iso: Path, label: str) -> None:
    for tool in ("xorriso", "genisoimage", "mkisofs"):
        if shutil.which(tool):
            prefix = [tool, "-as", "mkisofs"] if tool == "xorriso" else [tool]
            run([*prefix, "-quiet", "-V", label, "-J", "-R", "-o", str(iso), str(source)], capture=True)
            return
    if shutil.which("bsdtar"):
        run(["bsdtar", "-c", "-f", str(iso), "--format", "iso9660", "--options",
             f"iso9660:volume-id={label},iso9660:rockridge,iso9660:joliet", "-C", str(source),
             *sorted(path.name for path in source.iterdir())], capture=True)
        return
    raise StepError("no tool to make the seed ISO with; install xorriso, genisoimage, or bsdtar (libarchive)")


def build_seed(directory: Path, public_key: str) -> Path:
    """The configuration media the installed image waits for on its first boot: a volume labelled
    ACLSEED holding /ignition/config.ign."""
    source = directory / "seed"
    (source / "ignition").mkdir(parents=True, exist_ok=True)
    (source / "ignition" / "config.ign").write_text(json.dumps(ignition_config(public_key), indent=2) + "\n")
    iso = directory / "seed.iso"
    iso.unlink(missing_ok=True)
    make_iso(source, iso, SEED_LABEL)
    label = iso_volume_id(iso)
    if label != SEED_LABEL:
        raise StepError(f"the seed ISO's volume label is {label!r}, not {SEED_LABEL}")
    return iso


# ---------------------------------------------------------------------------------------------
# VM


def ovmf_firmware() -> tuple[Path, Path]:
    for code, variables in OVMF_CANDIDATES:
        if Path(code).is_file() and Path(variables).is_file():
            return Path(code), Path(variables)
    die("OVMF firmware not found; install the ovmf or edk2-ovmf package")


def check_vm_prerequisites() -> None:
    missing = [tool for tool in ("qemu-system-x86_64", "qemu-img", "ssh", "ssh-keygen", "setsid")
               if not shutil.which(tool)]
    if missing:
        die(f"missing {', '.join(missing)}; install QEMU (qemu-system-x86 and qemu-utils, or "
            "qemu-kvm) and OpenSSH")
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        die("/dev/kvm isn't readable and writable; add yourself to the kvm group")
    ovmf_firmware()
    if len(str(QMP_SOCKET)) > 100:
        die(f"{QMP_SOCKET} is too long for a unix socket; set ACL_AB_STATE_DIR to a shorter path")


def port_free(port: int) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe_socket:
        probe_socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            probe_socket.bind(("127.0.0.1", port))
        except OSError:
            return False
    return True


def qemu_command(*, code: Path, disk: Path, variables: Path, memory: int, cpus: int, ssh_port: int,
                 installer: Path | None = None, seed: Path | None = None, loadvm: str = "",
                 cluster_tap: str = "", smbios_uuid: str = "") -> list[str]:
    """QEMU's command line. Each device's bootindex makes OVMF try the disk first, then the
    installer; -boot order isn't enough for OVMF. User networking forwards SSH from 127.0.0.1
    only. The cluster NIC, on a TAP device in the cluster's bridge, comes after it, so the user
    NIC stays eth0 and keeps carrying SSH, the default route, and DNS. An SMBIOS UUID gives the VM
    a /sys/class/dmi/id/product_uuid, as an Azure VM has, which kubelet reports as the Node's
    systemUUID."""
    command = [
        "setsid", "qemu-system-x86_64", "-name", "acl-abupdate",
        "-machine", "q35", "-cpu", "host", "-accel", "kvm", "-m", str(memory), "-smp", str(cpus),
    ]
    if smbios_uuid:
        command += ["-uuid", smbios_uuid]
    command += [
        "-drive", f"if=pflash,format=raw,unit=0,readonly=on,file={code}",
        "-drive", f"if=pflash,format=qcow2,unit=1,file={variables}",
        "-drive", f"if=none,id=osdisk,format=qcow2,file={disk}",
        "-device", "virtio-blk-pci,drive=osdisk,bootindex=1",
    ]
    if installer:
        command += ["-drive", f"if=none,id=installer,media=cdrom,readonly=on,format=raw,file={installer}",
                    "-device", "ide-cd,bus=ide.0,drive=installer,bootindex=2"]
    if seed:
        command += ["-drive", f"if=none,id=seed,media=cdrom,readonly=on,format=raw,file={seed}",
                    "-device", "ide-cd,bus=ide.1,drive=seed"]
    command += ["-nic", f"user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:{ssh_port}-:22"]
    if cluster_tap:
        command += ["-netdev", f"tap,id=cluster0,ifname={cluster_tap},script=no,downscript=no",
                    "-device", f"virtio-net-pci,netdev=cluster0,mac={CLUSTER_NIC_MAC}"]
    command += [
        "-chardev", f"file,id=serial0,path={SERIAL_LOG},append=on", "-serial", "chardev:serial0",
        "-qmp", f"unix:{QMP_SOCKET},server=on,wait=off",
        "-display", "none", "-daemonize", "-pidfile", str(PID_FILE),
    ]
    if loadvm:
        command += ["-loadvm", loadvm]
    return command


def launch_vm(*, disk: Path, variables: Path, memory: int, cpus: int, ssh_port: int,
              installer: Path | None = None, seed: Path | None = None, loadvm: str = "",
              cluster_tap: str = "", smbios_uuid: str = "") -> None:
    """Start QEMU in the background, as qemu_command describes. Only the cluster NIC needs root,
    for its TAP device, which the cluster's setup creates owned by the user."""
    if vm_pid() is not None:
        raise StepError("the VM is running already")
    for path in (disk, variables, installer, seed):
        if path is not None and "," in str(path):
            raise StepError(f"QEMU can't take a path with a comma: {path}")
    if not port_free(ssh_port):
        raise StepError(f"127.0.0.1:{ssh_port} is in use; pass up another --ssh-port, or set ACL_AB_SSH_PORT")
    if cluster_tap and not Path(f"/sys/class/net/{cluster_tap}").exists():
        raise StepError(f"the cluster's TAP device {cluster_tap} doesn't exist")
    code, _ = ovmf_firmware()
    QMP_SOCKET.unlink(missing_ok=True)
    PID_FILE.unlink(missing_ok=True)
    run(qemu_command(code=code, disk=disk, variables=variables, memory=memory, cpus=cpus, ssh_port=ssh_port,
                     installer=installer, seed=seed, loadvm=loadvm, cluster_tap=cluster_tap,
                     smbios_uuid=smbios_uuid), capture=True)
    log(f"QEMU started (pid {PID_FILE.read_text().strip()}); serial log {SERIAL_LOG}")


def vm_pid() -> int | None:
    """The VM's QEMU process while it runs. A PID can be reused, so the process has to be QEMU
    started with this harness's PID file."""
    try:
        pid = int(PID_FILE.read_text().strip())
        args = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
    except (OSError, ValueError):
        return None
    if not args or b"qemu-system" not in args[0] or str(PID_FILE).encode() not in args:
        return None
    return pid


class QMP:
    """A client for QEMU's machine protocol socket: hard resets, quitting, and snapshots, which
    go through the human monitor's savevm and loadvm, as QMP has no synchronous command for them."""

    def __init__(self, timeout: float = 30) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        try:
            self.sock.connect(str(QMP_SOCKET))
        except OSError as error:
            self.sock.close()
            raise StepError(f"can't reach QEMU's control socket {QMP_SOCKET}: {error}") from error
        self.stream = self.sock.makefile("rwb")
        self._receive()
        self.execute("qmp_capabilities")

    def __enter__(self) -> QMP:
        return self

    def __exit__(self, *_: object) -> None:
        with contextlib.suppress(OSError):
            self.stream.close()
        with contextlib.suppress(OSError):
            self.sock.close()

    def _receive(self) -> dict[str, Any]:
        while True:
            line = self.stream.readline()
            if not line:
                raise StepError("QEMU closed its control socket")
            message = json.loads(line)
            if "event" not in message:
                return message

    def execute(self, command: str, arguments: dict[str, Any] | None = None, *,
                timeout: float | None = None) -> Any:
        if timeout is not None:
            self.sock.settimeout(timeout)
        request: dict[str, Any] = {"execute": command}
        if arguments:
            request["arguments"] = arguments
        try:
            self.stream.write(json.dumps(request).encode() + b"\n")
            self.stream.flush()
            reply = self._receive()
        except (OSError, json.JSONDecodeError) as error:
            raise StepError(f"QEMU {command}: {error}") from error
        if "error" in reply:
            raise StepError(f"QEMU {command}: {reply['error'].get('desc', reply['error'])}")
        return reply.get("return")

    def hmp(self, command_line: str, *, timeout: float = 600) -> str:
        """Run a human monitor command. It returns its output, which is empty when savevm, loadvm,
        and delvm succeed, and an error message when they don't."""
        output = self.execute("human-monitor-command", {"command-line": command_line}, timeout=timeout)
        return str(output or "").strip()

    def snapshot_tags(self) -> set[str]:
        tags = set()
        for line in self.hmp("info snapshots").splitlines():
            parts = line.split()
            if len(parts) >= 2 and parts[0] != "ID" and not line.startswith(("List", "There")):
                tags.add(parts[1])
        return tags


def stop_vm(*, hard: bool = False, timeout: float = 120) -> None:
    """Power the VM off, or with hard, stop QEMU at once, as pulling the power would."""
    pid = vm_pid()
    if pid is None:
        PID_FILE.unlink(missing_ok=True)
        return
    if not hard:
        log("powering the VM off")
        ssh("sudo systemctl poweroff", check=False, timeout=30)
        if poll(timeout, lambda: vm_pid() is None, 2):
            PID_FILE.unlink(missing_ok=True)
            return
        warn("the VM didn't power off; stopping QEMU")
    else:
        log(f"stopping QEMU (pid {pid})")
    with contextlib.suppress(StepError), QMP(timeout=10) as qmp:
        qmp.execute("quit")
    if not poll(15, lambda: vm_pid() is None, 1):
        with contextlib.suppress(ProcessLookupError):
            os.kill(pid, signal.SIGTERM)
        if not poll(10, lambda: vm_pid() is None, 1):
            with contextlib.suppress(ProcessLookupError):
                os.kill(pid, signal.SIGKILL)
    PID_FILE.unlink(missing_ok=True)


def use_port(port: int) -> None:
    global _ssh_port
    _ssh_port = port


def ssh_command() -> list[str]:
    return ["ssh", *SSH_OPTS, "-p", str(_ssh_port), f"{SSH_USER}@127.0.0.1"]


def ssh(command: str, *, check: bool = True, timeout: float = 120,
        input: str | None = None) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run([*ssh_command(), command], capture_output=True, text=True,
                                timeout=timeout, input=input, check=False)
    except subprocess.TimeoutExpired:
        result = subprocess.CompletedProcess(["ssh"], 255, "", f"timed out after {duration(timeout)}: {command}")
    if check and result.returncode != 0:
        raise StepError(f"on the VM, {command!r} exited {result.returncode}: {last_lines(result.stderr, 6)}")
    return result


def ssh_bytes(command: str, data: bytes, *, timeout: float = 600) -> subprocess.CompletedProcess[bytes]:
    """Run a command on the VM with binary input, such as a tar stream. It never raises."""
    try:
        return subprocess.run([*ssh_command(), command], input=data, capture_output=True, timeout=timeout, check=False)
    except subprocess.TimeoutExpired:
        return subprocess.CompletedProcess(["ssh"], 255, b"", f"timed out after {duration(timeout)}".encode())


def copy_to_vm(local: Path, remote: str, timeout: float = 3600) -> None:
    """Copy a file into the VM through ssh, which needs no SFTP server in the image."""
    with local.open("rb") as source:
        result = subprocess.run([*ssh_command(), f"cat > {shlex.quote(remote)}"], stdin=source,
                                capture_output=True, timeout=timeout, check=False)
    if result.returncode != 0:
        raise StepError(f"copying {local.name} to the VM failed: {last_lines(result.stderr.decode(errors='replace'), 4)}")


def copy_from_vm(remote: str, local: Path, timeout: float = 1800) -> None:
    with local.open("wb") as target:
        result = subprocess.run([*ssh_command(), f"cat {shlex.quote(remote)}"], stdout=target,
                                stderr=subprocess.PIPE, timeout=timeout, check=False)
    if result.returncode != 0:
        raise StepError(f"copying {remote} from the VM failed: {last_lines(result.stderr.decode(errors='replace'), 4)}")


def boot_id() -> str | None:
    result = ssh("cat /proc/sys/kernel/random/boot_id", check=False, timeout=20)
    if result.returncode != 0:
        return None
    return result.stdout.strip() or None


def wait_for_ssh(timeout: float) -> str:
    deadline = time.monotonic() + timeout
    while True:
        if vm_pid() is None:
            raise StepError(f"the VM exited; see {SERIAL_LOG}")
        current = boot_id()
        if current:
            return current
        if time.monotonic() >= deadline:
            raise StepError(f"timed out after {duration(timeout)} waiting for SSH; see {SERIAL_LOG}")
        time.sleep(5)


def wait_for_new_boot(previous: str, timeout: float, *, required: bool = True) -> str | None:
    """Wait for the VM to answer SSH from a boot other than previous. A reboot shows as a new boot
    ID whether or not the harness saw SSH go down."""
    deadline = time.monotonic() + timeout
    while True:
        if vm_pid() is None:
            raise StepError(f"the VM exited; see {SERIAL_LOG}")
        current = boot_id()
        if current and current != previous:
            return current
        if time.monotonic() >= deadline:
            if not required:
                return None
            raise StepError(f"timed out after {duration(timeout)} waiting for the VM to boot again; see {SERIAL_LOG}")
        time.sleep(5)


def wait_settled(timeout: float = 600) -> None:
    """Wait for systemd to finish booting, so the probes see the boot's final state, including a
    commit that a trident unit runs at boot. Then step the VM's clock to the host's: the image runs
    no NTP client, and a boot after a restore starts from a clock that's behind."""
    result = ssh("systemctl is-system-running --wait", check=False, timeout=timeout)
    state = result.stdout.strip()
    if not state and result.returncode == 255:
        log("SSH dropped while the boot finished; the VM may be rebooting again")
        return
    if state == "degraded":
        failed = ssh("systemctl --failed --no-legend --plain", check=False).stdout.strip()
        warn(f"systemd reports the boot degraded; failed units:\n{failed}")
    elif state != "running":
        warn(f"systemd reports {state or 'nothing'} rather than running")
    sync_guest_clock()


def read_serial(offset: int) -> tuple[str, int]:
    try:
        with SERIAL_LOG.open("rb") as serial:
            serial.seek(offset)
            data = serial.read()
    except OSError:
        return "", offset
    return data.decode("utf-8", "replace"), offset + len(data)


_ESCAPES = re.compile(r"\x1b\[[0-?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b[=>]")


def last_serial_line(text: str) -> str:
    for line in reversed(_ESCAPES.sub("", text).replace("\r", "\n").splitlines()):
        line = "".join(char for char in line if char.isprintable()).strip()
        if line:
            return line[:120]
    return "(nothing on the console yet)"


# ---------------------------------------------------------------------------------------------
# Probe

# One SSH round trip reads everything the checks compare. Each section is parsed on this side.
PROBE_SCRIPT = r"""
section() { printf '\n### %s\n' "$1"; }
efivar() {
  f=/sys/firmware/efi/efivars/$1-@GUID@
  [ -e "$f" ] && printf '%s=%s\n' "$1" "$(sudo od -An -tx1 -v "$f" | tr -d ' \n')"
}
section boot_id; cat /proc/sys/kernel/random/boot_id
section btime; awk '/^btime/{print $2}' /proc/stat
section boots; journalctl --list-boots --no-pager -q 2>/dev/null | wc -l
section os_release; cat /etc/os-release
section kernel; uname -r
section verity; sudo veritysetup status usr 2>&1
section status
# stdout has the Host Status alone; the log goes to stderr. Not --outfile: root's trident can't write
# a file the user made in /tmp, which fs.protected_regular forbids.
err=$(mktemp)
if status=$(sudo trident get status 2>"$err"); then printf '%s\n' "$status"; else echo '!error'; tail -n 5 "$err"; fi
rm -f "$err"
section efi
efivar BootCurrent; efivar BootNext; efivar BootOrder
for f in /sys/firmware/efi/efivars/Boot[0-9A-F][0-9A-F][0-9A-F][0-9A-F]-@GUID@; do
  [ -e "$f" ] && efivar "$(basename "$f" | cut -c1-8)"
done
section usr_writable
if sudo touch /usr/.acl-abupdate-probe 2>/dev/null; then sudo rm -f /usr/.acl-abupdate-probe; echo yes; else echo no; fi
section markers
for p in @MARKERS@; do printf '%s\t%s\n' "$p" "$(sudo cat "$p" 2>/dev/null | tr '\n' ' ')"; done
section system_state; systemctl is-system-running 2>/dev/null
section trident_units; systemctl list-units 'trident*' --all --no-legend --plain 2>/dev/null
section trident_version; sudo trident --version 2>/dev/null
section trident_acl_agent
printf 'is-enabled=%s\n' "$(systemctl is-enabled @AGENT@ 2>/dev/null)"
systemctl show @AGENT@ --no-pager -p LoadState -p ActiveState -p SubState -p NRestarts \
  -p InactiveExitTimestampMonotonic -p DropInPaths -p Environment 2>/dev/null
if [ -S @SOCKET@ ]; then echo socket=present; else echo socket=missing; fi
section trident_acl_agent_preflight
# -a: once the agent connects, its log has bytes that make grep call the stream binary.
sudo journalctl -b -u @AGENT@ -o cat --no-pager 2>/dev/null | grep -a 'trident-acl-agent preflight' | tail -n 4
true
"""


class Probe:
    """What one SSH round trip reads from the VM: its boot, /usr's verity device, Trident's Host
    Status, the UEFI boot variables, and the harness's markers."""

    def __init__(self, sections: dict[str, str]) -> None:
        self.sections = sections
        self.verity = parse_verity(sections.get("verity", ""))
        status = sections.get("status", "")
        self.status_error = status[len("!error"):].strip() if status.startswith("!error") else ""
        self.status_text = "" if status.startswith("!error") else status
        self.status: dict[str, str | None] | None = parse_yaml(self.status_text) if self.status_text else None
        self.efi = dict(line.split("=", 1) for line in sections.get("efi", "").splitlines() if "=" in line)
        self.os_release = parse_os_release(sections.get("os_release", ""))

    @property
    def boot_id(self) -> str:
        return self.sections.get("boot_id", "").strip()

    @property
    def booted_at(self) -> datetime.datetime | None:
        """When this boot started, by the VM's clock."""
        value = self.sections.get("btime", "").strip()
        return datetime.datetime.fromtimestamp(int(value), datetime.timezone.utc) if value.isdigit() else None

    @property
    def data_device(self) -> str:
        return self.verity.get("data device", "")

    @property
    def pair(self) -> tuple[str, str]:
        """The booted slot, as /usr's verity data and hash devices. findmnt shows /dev/dm-0 for
        either slot, so these are what tell them apart."""
        return self.data_device, self.verity.get("hash device", "")

    @property
    def pair_text(self) -> str:
        return ", ".join(device for device in self.pair if device) or "no verity device"

    @property
    def roothash(self) -> str:
        return self.verity.get("root hash", "")

    @property
    def state(self) -> str | None:
        return self.status.get("servicingState") if self.status else None

    @property
    def active_volume(self) -> str | None:
        return self.status.get("abActiveVolume") if self.status else None

    @property
    def last_error(self) -> str | None:
        if not self.status:
            return None
        value = self.status.get("lastError")
        if value == BLOCK:
            return yaml_block(self.status_text, "lastError") or "(set)"
        return value

    @property
    def image_url(self) -> str | None:
        return self.status.get("spec.image.url") if self.status else None

    @property
    def image_sha384(self) -> str | None:
        return self.status.get("spec.image.sha384") if self.status else None

    @property
    def boot_current(self) -> str:
        return efi_boot_number(self.efi.get("BootCurrent", ""))

    @property
    def boot_next(self) -> str:
        return efi_boot_number(self.efi.get("BootNext", ""))

    @property
    def boot_order(self) -> list[str]:
        return efi_boot_order(self.efi.get("BootOrder", ""))

    def boot_label(self, number: str) -> str:
        description = efi_load_option_description(self.efi.get(f"Boot{number}", ""))
        return f"{number} ({description})" if description else number

    @property
    def usr_writable(self) -> bool:
        return self.sections.get("usr_writable", "").strip() == "yes"

    @property
    def markers(self) -> dict[str, list[str]]:
        markers = {}
        for line in self.sections.get("markers", "").splitlines():
            path, _, tokens = line.partition("\t")
            markers[path.strip()] = tokens.split()
        return markers

    @property
    def boot_count(self) -> int:
        value = self.sections.get("boots", "").strip()
        return int(value) if value.isdigit() else 0

    @property
    def kernel(self) -> str:
        return self.sections.get("kernel", "").strip()

    @property
    def trident_version(self) -> str:
        return self.sections.get("trident_version", "").strip()

    @property
    def trident_agent(self) -> dict[str, str]:
        """trident-acl-agent.service's state: `systemctl show` properties, is-enabled, and
        whether tridentd's socket exists."""
        return parse_properties(self.sections.get("trident_acl_agent", ""))

    @property
    def trident_agent_preflight(self) -> list[str]:
        """This boot's last preflight lines from the drop-in's ExecStartPre."""
        return [line for line in self.sections.get("trident_acl_agent_preflight", "").splitlines() if line.strip()]

    def text(self) -> str:
        return "".join(f"### {name}\n{content}\n\n" for name, content in self.sections.items())


def trident_agent_pinned() -> dict[str, str]:
    """The settings the harness's drop-in pins for trident-acl-agent."""
    return unit_environment(TRIDENT_AGENT_DROPIN.read_text())


def trident_socket_path() -> str:
    return trident_agent_pinned().get("TRIDENT_ACL_AGENT_TRIDENT_SOCKET", "").removeprefix("unix://") or \
        "/run/trident/trident.sock"


def probe_script() -> str:
    return (PROBE_SCRIPT.replace("@GUID@", EFI_GLOBAL_GUID)
            .replace("@MARKERS@", " ".join(shlex.quote(path) for path in MARKER_PATHS))
            .replace("@AGENT@", TRIDENT_AGENT_UNIT)
            .replace("@SOCKET@", shlex.quote(trident_socket_path())))


def trident_agent_findings(current: Probe, pinned: dict[str, str]) -> list[tuple[bool, str, str]]:
    """Check T1 on a probe: trident-acl-agent.service is installed, enabled, and started this
    boot, with the harness's drop-in loaded and every setting it pins in effect, and its preflight
    ran this boot. Each finding is (ok, description, detail)."""
    agent = current.trident_agent
    unit = TRIDENT_AGENT_UNIT
    load_state = agent.get("LoadState", "")
    findings = [(load_state == "loaded", f"{unit} is installed",
                 f"its LoadState is {load_state or 'unknown'}; the image may not have the trident-acl package")]
    if load_state != "loaded":
        return findings
    enabled = agent.get("is-enabled", "")
    findings.append((enabled == "enabled", f"{unit} is enabled", f"it's {enabled or 'unknown'}"))
    started = agent.get("InactiveExitTimestampMonotonic", "0") not in ("", "0")
    findings.append((started, f"{unit} started this boot",
                     f"it's {agent.get('ActiveState', '?')}/{agent.get('SubState', '?')}"))
    drop_ins = agent.get("DropInPaths", "").split()
    findings.append((TRIDENT_AGENT_DROPIN_PATH in drop_ins, "the harness's drop-in is loaded",
                     f"DropInPaths is {' '.join(drop_ins) or 'empty'}"))
    environment = split_environment(agent.get("Environment", ""))
    differing = [f"{name}={environment.get(name, '(unset)')}" for name, value in pinned.items()
                 if environment.get(name) != value]
    findings.append((not differing, f"the {len(pinned)} settings the drop-in pins are in the unit's environment",
                     f"these differ: {', '.join(differing)}"))
    kubeconfig = pinned.get(TRIDENT_AGENT_KUBECONFIG_VARIABLE, "")
    findings.append((any(kubeconfig in line for line in current.trident_agent_preflight),
                     f"this boot's preflight checked {kubeconfig}",
                     "no preflight line this boot names it"))
    return findings


def probe() -> Probe:
    result = ssh("bash -s", input=probe_script(), timeout=180, check=False)
    if "### boot_id" not in result.stdout:
        raise StepError(f"couldn't read the VM's state (ssh exited {result.returncode}): "
                        f"{last_lines(result.stderr, 4)}")
    return Probe(parse_sections(result.stdout))


# ---------------------------------------------------------------------------------------------
# Harness state


@dataclasses.dataclass
class State:
    """What the harness knows of its VM between commands, in state.json. Trident's Host Status is
    the source of truth for the VM's servicing; this keeps what it doesn't hold: the harness's
    markers, the update staged, and which devices each slot's /usr is on."""

    base_tag: str = BASE_TAG
    update_tag: str = UPDATE_TAG
    # Where the images come from, as up found them set.
    registry: str = ""
    iso_repo: str = ""
    cosi_repo: str = ""
    ssh_port: int = SSH_PORT
    memory: int = VM_MEMORY
    cpus: int = VM_CPUS
    installed: str = ""
    # Whether this Trident can stage without finalizing; None until asked.
    two_step: bool | None = None
    # Trident's volume name (volume-a) to the [data, hash] devices of /usr in that slot.
    slot_pairs: dict[str, list[str]] = dataclasses.field(default_factory=dict)
    # A slot's /usr data device to the image last seen in it: {"roothash", "image"}.
    slots: dict[str, dict[str, str]] = dataclasses.field(default_factory=dict)
    markers: list[str] = dataclasses.field(default_factory=list)
    staged: dict[str, Any] | None = None
    committed_update: dict[str, Any] | None = None
    # The cluster `up --cluster` made: flexcluster.ClusterSpec's fields. None without one.
    cluster: dict[str, Any] | None = None
    # The VM's SMBIOS UUID, its product_uuid; empty for a VM made before the harness gave it one.
    smbios_uuid: str = ""
    # The agent image the VM's flex node runs, by digest in the local registry.
    agent_image: str = ""
    # The harness cordoned the node for a reboot, and hasn't uncordoned it yet.
    cordoned: bool = False
    history: list[dict[str, Any]] = dataclasses.field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> State:
        known = {field.name for field in dataclasses.fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})

    @classmethod
    def load(cls) -> State:
        try:
            return cls.from_dict(json.loads(STATE_FILE.read_text()))
        except (OSError, json.JSONDecodeError) as error:
            die(f"can't read {STATE_FILE}: {error}")

    def save(self) -> None:
        partial = STATE_FILE.with_suffix(".partial")
        partial.write_text(json.dumps(dataclasses.asdict(self), indent=2) + "\n")
        os.replace(partial, STATE_FILE)


def claim_state_dir() -> None:
    """Make the state directory and mark it, for down to remove. An existing one has to be the
    default, marked already, or empty: down mustn't remove a directory that holds anything else."""
    claimable = STATE.exists() and not STATE_MARKER.exists() and STATE.resolve() != DEFAULT_STATE.resolve()
    if claimable and (not STATE.is_dir() or any(STATE.iterdir())):
        die(f"{STATE} isn't a directory up made, or an empty one; set ACL_AB_STATE_DIR to a new or empty directory")
    STATE.mkdir(parents=True, exist_ok=True)
    STATE.chmod(0o700)
    STATE_MARKER.touch()


@contextlib.contextmanager
def state_lock() -> Iterator[None]:
    """Keep two commands from driving the VM at once."""
    if not STATE.is_dir():
        die(f"there's no VM in {STATE}; run `up` first")
    with LOCK_FILE.open("a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            die("another abupdate.py command is driving this VM; wait for it, or set another ACL_AB_STATE_DIR")
        yield


def require_state() -> State:
    if not STATE_FILE.is_file():
        die(f"there's no VM in {STATE}; run `up` first")
    state = State.load()
    use_port(state.ssh_port)
    use_images(state)
    return state


def require_running() -> State:
    state = require_state()
    if vm_pid() is None:
        die("the VM isn't running; `start` it")
    return state


def launch(state: State, *, loadvm: str = "") -> None:
    # A snapshot's devices have to match the VM it's loaded into, so a VM brought up with a cluster
    # always has its NIC.
    launch_vm(disk=VM_DISK, variables=VM_VARS, memory=state.memory, cpus=state.cpus,
              ssh_port=state.ssh_port, loadvm=loadvm,
              cluster_tap=str(state.cluster.get("tap", "")) if state.cluster else "",
              smbios_uuid=state.smbios_uuid)


def sync_guest_clock() -> None:
    """Step the VM's clock to the host's. A snapshot holds the clock as it was when it was saved,
    so after a restore the guest is behind by however long ago that was, and chronyd only steps
    it at its next poll, which can be many minutes away. In a cluster the node's Lease renewals
    would go back in time meanwhile."""
    result = ssh(f"before=$(date +%s.%N); sudo date -u -s @{time.time():.3f} >/dev/null && "
                 "(sudo chronyc -a makestep >/dev/null 2>&1 || true); echo \"$before $(date +%s.%N)\"",
                 check=False, timeout=30)
    try:
        before, after = (float(value) for value in result.stdout.split()[-2:])
    except ValueError:
        warn(f"couldn't step the VM's clock: {last_lines(result.stderr, 3)}")
        return
    log(f"stepped the VM's clock {after - before:+.0f}s to the host's (now {after - time.time():+.1f}s off)")


def cluster_of(state: State) -> Any:
    """The cluster `up --cluster` made, as a flexcluster.Cluster, or None. The cluster modules are
    imported only when there's a cluster, so a plain run doesn't depend on them."""
    if not state.cluster:
        return None
    import flexcluster

    FLEX_DIR.mkdir(mode=0o700, parents=True, exist_ok=True)
    return flexcluster.Cluster(flexcluster.ClusterSpec.from_dict(state.cluster), kubeconfig=KUBECONFIG,
                               repo=REPO, work_dir=FLEX_DIR, run=host_run, log=log)


def system_uuid_findings(node: dict[str, Any] | None, product_uuid: str, expected: str) -> list[tuple[bool, str, str]]:
    """Whether the Node's systemUUID, which kubelet reads inside the nspawn machine, is the VM's
    product_uuid. trident-acl-agent's VALIDATE_NODE_UUID compares exactly those, so this is what
    tells whether that setting can be turned on for flex nodes."""
    if not expected:
        return []
    reported = ((node or {}).get("status") or {}).get("nodeInfo", {}).get("systemUUID", "")
    return [
        (product_uuid.lower() == expected.lower(), "the VM's product_uuid is the SMBIOS UUID the harness gave it",
         f"it's {product_uuid or 'missing'}, not {expected}"),
        (bool(reported) and reported.lower() == product_uuid.lower(),
         "the Node's systemUUID, which kubelet reads in its nspawn machine, is the VM's product_uuid",
         f"the Node reports {reported or 'none'}, and the VM's is {product_uuid or 'missing'}"),
    ]


def annotation_prefix() -> str:
    """The Node annotations' prefix, as the drop-in pins it for trident-acl-agent."""
    return trident_agent_pinned().get("TRIDENT_ACL_AGENT_KUBERNETES_ANNOTATION_PREFIX", "acl.azure.com")


def annotation_track(version: str) -> str:
    """The track a version is offered on, one per version, as the contract's example pins them."""
    return f"pin-{version}"


def bump_patch(version: str) -> str:
    """Another semver version than version, for requests that must not match what's offered."""
    major, minor, patch = version.split("+")[0].split("-")[0].split(".")
    return f"{major}.{minor}.{int(patch) + 1}"


def parse_duration(value: str) -> float:
    """Seconds in a duration as the drop-in writes them, such as 60s, 10m, or 1h."""
    match = re.fullmatch(r"\s*(\d+(?:\.\d+)?)\s*(ms|s|m|h)?\s*", value)
    if not match:
        raise ValueError(f"{value!r} isn't a duration")
    return float(match.group(1)) * {"ms": 0.001, "s": 1, "m": 60, "h": 3600, None: 1}[match.group(2)]


def parse_utc(value: str) -> datetime.datetime:
    """An RFC 3339 time as the agent writes it, with up to nanoseconds, which fromisoformat may not take."""
    match = re.fullmatch(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(?:Z|[+-]00:00)", value)
    if not match:
        raise ValueError(f"{value!r} isn't a UTC time")
    fraction = (match.group(2) or "0")[:6].ljust(6, "0")
    return datetime.datetime.fromisoformat(f"{match.group(1)}.{fraction}+00:00")


def heartbeat_findings(observed: list[dict[str, Any]], final: dict[str, Any],
                       interval: float) -> list[tuple[bool, str, str]]:
    """Check N6 on the statuses read while a stage ran: as the contract says, an InProgress
    status's lastUpdatedUtc refreshes on the heartbeat's cadence, startedUtc stays, finishedUtc is
    absent until the code is terminal, and the terminal status has it."""
    in_progress = [status for status in observed if status.get("code") == "InProgress"]
    if not in_progress:
        return [(False, "the agent reported the stage InProgress while it ran", "no InProgress status was read")]
    try:
        times = sorted({parse_utc(status["lastUpdatedUtc"]) for status in in_progress if status.get("lastUpdatedUtc")})
    except ValueError as error:
        return [(False, "lastUpdatedUtc is a UTC time", str(error))]
    gaps = [round((later - earlier).total_seconds()) for earlier, later in zip(times, times[1:], strict=False)]
    started = {status.get("startedUtc") for status in in_progress}
    finished = [status["finishedUtc"] for status in in_progress if status.get("finishedUtc")]
    bound = interval * 1.5
    return [
        (len(times) >= 3, "lastUpdatedUtc refreshed at least twice while the stage was InProgress",
         f"it had {len(times)} value(s) in {len(in_progress)} reads"),
        (bool(gaps) and max(gaps) <= bound, f"each refresh came within {bound:.0f}s of the last, for a {interval:.0f}s heartbeat",
         f"the gaps were {gaps}s"),
        (len(started) == 1, "startedUtc stayed the same while InProgress", f"it took {sorted(map(str, started))}"),
        (not finished, "finishedUtc was absent while InProgress", f"it was {finished[0] if finished else ''}"),
        (final.get("code") == "Success" and bool(final.get("finishedUtc")), "the terminal status has finishedUtc",
         f"it's {final.get('code')}, finishedUtc {final.get('finishedUtc')}"),
    ]


def status_content(raw: dict[str, Any] | None) -> dict[str, Any] | None:
    """A status as the agent compares them (UpdateStatus::same_content): without lastUpdatedUtc,
    which every write refreshes, and tridentVersion, which it restamps."""
    if raw is None:
        return None
    return {key: value for key, value in raw.items() if key not in ("lastUpdatedUtc", "tridentVersion")}


def kernel_slot(command_line: str) -> str:
    """The A/B slot a kernel command line boots, from ACL's acl.slot= argument, or ""."""
    match = re.search(r"(?:^|\s)acl\.slot=(\S+)", command_line)
    return match.group(1) if match else ""


def reset_at_next_kernel(offset: int, timeout: float) -> str:
    """Reset the VM as soon as the serial console, past offset, shows a kernel's command line, and
    return it. By then the firmware has booted that kernel, and so used its one-time BootNext: the
    reset boots the first entry of BootOrder, as a failed first boot of a new slot would."""
    deadline = time.monotonic() + timeout
    text = ""
    while time.monotonic() < deadline:
        chunk, offset = read_serial(offset)
        text = (text + chunk)[-65536:]
        match = re.search(r"Command line: ([^\r\n]*)[\r\n]", text)
        if match:
            with QMP() as qmp:
                qmp.execute("system_reset")
            log(f"reset the VM as its next kernel started, slot {kernel_slot(match.group(1)) or '?'}")
            return match.group(1)
        if vm_pid() is None:
            raise StepError(f"the VM exited; see {SERIAL_LOG}")
        time.sleep(0.2)
    raise StepError(f"no kernel started in {duration(timeout)}; see {SERIAL_LOG}")


def configure_cluster_nic(spec: Any) -> None:
    """Give the VM's cluster NIC its address, with a drop-in on the seed's network file, which
    persists under /etc like the rest of the node's configuration. Then check the API server
    answers on the bridge."""
    content = f"[Network]\nAddress={spec.vm_ip}/24\n"
    dropin = f"{CLUSTER_NETWORK}.d/50-address.conf"
    ssh(f"sudo install -D -m 0644 /dev/stdin {shlex.quote(dropin)} && sudo networkctl reload && "
        f"for link in /sys/class/net/*; do [ \"$(cat $link/address)\" = {CLUSTER_NIC_MAC} ] && "
        "sudo networkctl reconfigure \"${link##*/}\"; done; true", input=content)

    def answers() -> bool:
        return ssh(f"curl -sk --max-time 5 https://{spec.api_server}/healthz", check=False, timeout=20).stdout == "ok"

    if not poll(90, answers, 3):
        raise StepError(f"the VM can't reach the API server at {spec.api_server} on the cluster bridge; "
                        "`ip -br addr` on the VM and `ip -br link` here show the links")
    log(f"the VM is {spec.vm_ip} on the cluster bridge, and reaches the API server at {spec.api_server}")


# ---------------------------------------------------------------------------------------------
# Snapshots


def snapshot_names() -> list[str]:
    return sorted(path.stem for path in SNAPSHOT_DIR.glob("*.json"))


def save_snapshot(state: State, name: str, *, force: bool = False, summary: str = "") -> None:
    """Save the running VM, memory included, in its disk and UEFI variables, and the harness's
    state beside them, so restore returns to this moment, mid-update or not."""
    if not SNAPSHOT_PATTERN.fullmatch(name):
        die(f"{name!r} isn't a snapshot name: use letters, digits, '.', '_', and '-'")
    meta = SNAPSHOT_DIR / f"{name}.json"
    if meta.exists() and not force:
        die(f"snapshot {name} exists; pass --force to replace it")
    if vm_pid() is None:
        die("snapshots are taken of the running VM; `start` it first")
    log(f"saving snapshot {name}; the VM pauses while its memory is written")
    started = time.monotonic()
    cluster = cluster_of(state)
    record = cluster_record(cluster)
    with QMP() as qmp:
        if name in qmp.snapshot_tags():
            qmp.hmp(f"delvm {name}", timeout=600)
        output = qmp.hmp(f"savevm {name}", timeout=1800)
    if output:
        raise StepError(f"savevm {name}: {output}")
    SNAPSHOT_DIR.mkdir(exist_ok=True)
    meta.write_text(json.dumps({"name": name, "created": now_iso(), "summary": summary, "cluster": record,
                                "state": dataclasses.asdict(state)}, indent=2) + "\n")
    log(f"saved snapshot {name} in {duration(time.monotonic() - started)}")
    if record and record.get("node"):
        # The node may go NotReady while the VM is paused.
        cluster.wait_ready(NODE_NAME, READY_TIMEOUT)


def cluster_record(cluster: Any) -> dict[str, Any] | None:
    """What a snapshot keeps of the cluster: which cluster it is, and the fields of the Node the
    harness manages, to set back on a restore. None without a cluster."""
    if cluster is None:
        return None
    node = cluster.record(NODE_NAME, annotation_prefix()) if cluster.node(NODE_NAME) else None
    return {"identity": cluster.identity(), "node": node}


def snapshot_record(name: str) -> dict[str, Any] | None:
    try:
        return json.loads((SNAPSHOT_DIR / f"{name}.json").read_text()).get("cluster")
    except (OSError, json.JSONDecodeError):
        return None


def reset_cluster_for(cluster: Any, record: dict[str, Any] | None) -> None:
    """Set the cluster back to what a snapshot recorded, before the VM it's restored into runs
    again. Otherwise the restored node would see what the scenarios since left: an update request
    annotation trident-acl-agent would act on, a cordon, the harness's pods. A snapshot taken before
    the node joined gets no Node at all."""
    if record and record.get("node"):
        cluster.reset_to(NODE_NAME, record["node"], annotation_prefix())
    else:
        cluster.kubectl("delete", "node", NODE_NAME, "--ignore-not-found", check=False)


def restore_snapshot(name: str, current: State | None = None) -> State:
    """Return the VM, and the harness's state, to a snapshot. The history of what the harness ran
    is kept, with the restore added to it. In cluster mode, the cluster's side is reset to what the
    snapshot recorded while the VM is paused, and the snapshot has to be of this cluster."""
    meta = SNAPSHOT_DIR / f"{name}.json"
    if not meta.is_file():
        die(f"there's no snapshot {name}; `snapshot list` shows them")
    saved = json.loads(meta.read_text())
    state = State.from_dict(saved["state"])
    record = saved.get("cluster")
    if current is not None:
        state.history = [*current.history]
        # A snapshot saved before up recorded the image settings keeps the VM's.
        state.registry = state.registry or current.registry
        state.iso_repo = state.iso_repo or current.iso_repo
        state.cosi_repo = state.cosi_repo or current.cosi_repo
    state.history.append({"time": now_iso(), "operation": "restore", "snapshot": name})
    use_port(state.ssh_port)
    log(f"restoring snapshot {name}")
    cluster = cluster_of(state)
    if cluster is not None:
        cluster.ensure()
        if record and record.get("identity") and cluster.identity() != record["identity"]:
            raise StepError(f"snapshot {name} was saved in another kind cluster than {cluster.spec.name}, "
                            "so its node can't rejoin this one; `down` and `up --cluster` make both again")
    if vm_pid() is not None:
        with QMP() as qmp:
            if cluster is not None:
                qmp.execute("stop")
            try:
                if cluster is not None:
                    reset_cluster_for(cluster, record)
                # loadvm leaves a paused VM paused, for the cont below.
                output = qmp.hmp(f"loadvm {name}", timeout=1800)
            finally:
                if cluster is not None:
                    qmp.execute("cont")
        if output:
            raise StepError(f"loadvm {name}: {output}")
    else:
        if cluster is not None:
            reset_cluster_for(cluster, record)
        launch(state, loadvm=name)
    state.save()
    wait_for_ssh(120)
    sync_guest_clock()
    if cluster is not None and record and record.get("node"):
        # The agent's watch on the Node is a connection the API server closed long before the
        # restore; the restored agent would wait on it without seeing new requests.
        ssh(f"sudo systemctl restart {TRIDENT_AGENT_UNIT}", check=False, timeout=60)
    return state


def delete_snapshot(name: str) -> None:
    meta = SNAPSHOT_DIR / f"{name}.json"
    if vm_pid() is not None:
        with QMP() as qmp:
            if name in qmp.snapshot_tags():
                output = qmp.hmp(f"delvm {name}", timeout=600)
                if output:
                    raise StepError(f"delvm {name}: {output}")
    else:
        for image in (VM_DISK, VM_VARS):
            if image.exists():
                run(["qemu-img", "snapshot", "-d", name, str(image)], check=False, capture=True)
    meta.unlink(missing_ok=True)
    log(f"deleted snapshot {name}")


# ---------------------------------------------------------------------------------------------
# Checks and steps


class Checks:
    """Check results, labelled with their scopes: forward/A1, or pingpong/P2/A3."""

    def __init__(self) -> None:
        self.results: list[dict[str, str]] = []
        self.scopes: list[str] = []

    @contextlib.contextmanager
    def scope(self, name: str) -> Iterator[None]:
        self.scopes.append(name)
        try:
            yield
        finally:
            self.scopes.pop()

    def label(self, check_id: str) -> str:
        return "/".join([*self.scopes, check_id])

    def expect(self, ok: bool, check_id: str, description: str, detail: str = "") -> bool:
        label = self.label(check_id)
        result = {"id": label, "description": description, "result": "pass" if ok else "fail"}
        if detail and not ok:
            result["detail"] = detail
        self.results.append(result)
        if ok:
            log(f"ok: {label} {description}")
        else:
            log(f"FAIL: {label} {description}" + (f": {detail}" if detail else ""))
        return ok

    def skip(self, check_id: str, description: str, reason: str) -> None:
        label = self.label(check_id)
        self.results.append({"id": label, "description": description, "result": "skip", "detail": reason})
        log(f"skip: {label} {description}: {reason}")

    def failed(self, since: int = 0) -> list[dict[str, str]]:
        return [result for result in self.results[since:] if result["result"] == "fail"]


class Context:
    """One command's run: the harness's state, its checks, and the directory its artifacts go to.
    Its step methods are what the step commands and the scenarios run."""

    def __init__(self, command: str, state: State) -> None:
        self.command = command
        self.state = state
        self.checks = Checks()
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.run_dir = RUNS_DIR / f"{stamp}-{command}"
        suffix = 1
        while self.run_dir.exists():
            suffix += 1
            self.run_dir = RUNS_DIR / f"{stamp}-{command}-{suffix}"
        self.run_dir.mkdir(parents=True)
        self.started = time.monotonic()
        self.timings: list[tuple[str, float]] = []
        self.errors: list[str] = []
        self.scenarios: list[dict[str, Any]] = []
        # The servicing states drive_to_terminal passed through, for the scenarios that look.
        self.seen: list[str] = []
        self.target_spec = ""
        self.rounds = 2
        self.no_setup = False
        self.single_step = False
        # Cordon and drain the node around each reboot, in cluster mode; --no-drain turns it off.
        self.drain = True
        # The agent's state before a reboot, which K4 compares the state after it with.
        self.flex_baseline: dict[str, str] | None = None
        # What a run found that isn't a check's result, such as where the agent and its contract differ.
        self.notes: list[str] = []
        self._sequence = 0
        log(f"artifacts go to {self.run_dir}")

    # Bookkeeping

    def save(self, name: str, content: str | bytes) -> Path:
        self._sequence += 1
        path = self.run_dir / f"{self._sequence:02d}-{name}"
        path.write_bytes(content if isinstance(content, bytes) else content.encode())
        return path

    def scope(self, name: str) -> contextlib.AbstractContextManager[None]:
        return self.checks.scope(name)

    @contextlib.contextmanager
    def timed(self, name: str) -> Iterator[None]:
        started = time.monotonic()
        try:
            yield
        finally:
            seconds = time.monotonic() - started
            self.timings.append((self.checks.label(name), seconds))
            log(f"{name} took {duration(seconds)}")

    def record(self, operation: str, **fields: Any) -> None:
        self.state.history.append({"time": now_iso(), "operation": operation, "run": self.run_dir.name, **fields})
        self.state.save()

    def probe(self, label: str = "") -> Probe:
        result = probe()
        if label:
            self.save(f"{label}.txt", result.text())
        self.learn(result)
        return result

    def learn(self, probe: Probe, image: str = "") -> None:
        """Note the image in the booted slot, and once Trident is provisioned, which volume the
        slot is: until commit, abActiveVolume still names the slot the update came from."""
        if probe.data_device and probe.roothash:
            self.state.slots[probe.data_device] = {"roothash": probe.roothash,
                                                   "image": image or image_name(probe.roothash)}
        if probe.state == "provisioned" and probe.active_volume and probe.data_device:
            self.state.slot_pairs.setdefault(probe.active_volume, list(probe.pair))
        self.state.save()

    def target(self, default: str, *, allow_override: bool = True) -> Image:
        spec = self.target_spec if allow_override and self.target_spec else default
        return resolve_target(spec, self.state, lambda: self.probe().roothash)

    def alternative(self, image: Image) -> Image:
        base = cosi(self.state.base_tag, self.state)
        return cosi(self.state.update_tag, self.state) if image.roothash == base.roothash else base

    def trident(self, arguments: str, label: str, *, timeout: float = COMMAND_TIMEOUT) -> subprocess.CompletedProcess[str]:
        log(f"trident {arguments}")
        result = ssh(f"sudo trident {arguments}", check=False, timeout=timeout)
        self.save(f"trident-{label}.log", f"$ sudo trident {arguments}\n# exit {result.returncode}\n\n"
                                          f"## stdout\n{result.stdout}\n## stderr\n{result.stderr}")
        return result

    # Scenarios

    def ensure(self, scenario: Scenario) -> None:
        """Set the VM up for what the scenario needs, unless --no-setup."""
        if scenario.needs is Needs.NONE:
            return
        current = self.probe()
        if scenario.needs is Needs.OFF_TARGET:
            target = self.target(scenario.target)
            if current.roothash != target.roothash:
                return
            reason = f"the VM runs {target.name} already"

            def setup() -> None:
                self.update(self.alternative(target))
        else:
            if self.state.committed_update and current.state == "provisioned":
                return
            reason = "there's no committed A/B update to work from"

            def setup() -> None:
                self.update(resolve_target("other", self.state, lambda: self.probe().roothash))
        if self.no_setup:
            raise StepError(f"{reason}, and --no-setup was given")
        log(f"setting up: {reason}")
        with self.checks.scope("setup"):
            setup()

    def run_scenario(self, scenario: Scenario) -> str:
        started, first = time.monotonic(), len(self.checks.results)
        result, reason = "pass", ""
        log(f"scenario {scenario.name}: {scenario.description}")
        try:
            with self.checks.scope(scenario.name):
                if scenario.cluster and self.cluster is None:
                    raise Skip("it needs a cluster: `down`, then `up --cluster`")
                self.ensure(scenario)
                scenario.run(self)
        except Skip as skip:
            result, reason = "skip", str(skip)
        except StepError as error:
            result, reason = "fail", str(error)
            log(f"FAIL: {scenario.name} stopped: {error}")
        if result != "skip" and self.checks.failed(first):
            result = "fail"
        if result == "fail":
            self.collect_diagnostics(scenario.name)
        seconds = time.monotonic() - started
        self.scenarios.append({"name": scenario.name, "result": result, "reason": reason,
                               "experimental": scenario.experimental, "seconds": round(seconds, 1)})
        log(f"scenario {scenario.name}: {result}" + (f" ({reason})" if reason else "") + f" in {duration(seconds)}")
        return result

    # Steps

    def write_markers(self) -> None:
        token = f"{len(self.state.markers) + 1}-{secrets.token_hex(4)}"
        commands = [f"sudo mkdir -p {shlex.quote(str(Path(path).parent))} && "
                    f"echo {token} | sudo tee -a {shlex.quote(path)} >/dev/null" for path in MARKER_PATHS]
        ssh(" && ".join(commands) + " && sync")
        self.state.markers.append(token)
        self.state.save()
        log(f"wrote marker {token} to {', '.join(MARKER_PATHS)}")

    def copy_cosi(self, target: Image, check_id: str) -> str:
        directory = shlex.quote(GUEST_DIR)
        name = f"{target.sha384[:16]}.cosi"
        remote = f"{GUEST_DIR}/{name}"
        ssh(f"sudo mkdir -p {directory} && sudo chown {SSH_USER}: {directory}")
        size = target.path.stat().st_size
        present = ssh(f"stat -c %s {shlex.quote(remote)} 2>/dev/null", check=False).stdout.strip()
        if present == str(size):
            log(f"{target.name}'s COSI is in the VM already, at {remote}")
        else:
            ssh(f"rm -f {shlex.quote(remote + '.partial')}", check=False)
            needed = size + 256 * 2**20
            if self.guest_free(directory) < needed:
                # Pingpong alternates two COSIs, so they're kept while there's room for both.
                ssh(f"find {directory} -maxdepth 1 -name '*.cosi' ! -name {shlex.quote(name)} -delete", check=False)
            free = self.guest_free(directory)
            if free < needed:
                raise StepError(f"{GUEST_DIR} in the VM has {human_size(free)} free, and {target.name}'s "
                                f"COSI is {human_size(size)}; set ACL_AB_GUEST_DIR to a directory with room")
            log(f"copying {target.name}'s COSI ({human_size(size)}) to {remote}")
            with self.timed("copy"):
                copy_to_vm(target.path, f"{remote}.partial")
                ssh(f"mv {shlex.quote(remote + '.partial')} {shlex.quote(remote)}")
        hashed = ssh(f"tar -xOf {shlex.quote(remote)} metadata.json | sha384sum", check=False, timeout=600)
        digest = hashed.stdout.split()[0] if hashed.returncode == 0 and hashed.stdout.split() else ""
        if digest:
            self.checks.expect(digest == target.sha384, check_id,
                               "the COSI in the VM has the expected metadata.json sha384", f"{digest} != {target.sha384}")
        else:
            warn(f"couldn't hash the COSI's metadata.json in the VM: {last_lines(hashed.stderr, 3)}")
        return remote

    def guest_free(self, directory: str) -> int:
        """Free bytes in a directory in the VM; a large number if df can't say, so the copy tries."""
        free = ssh(f"df -B1 --output=avail {directory} | tail -n 1", check=False).stdout.strip()
        return int(free) if free.isdigit() else 2**62

    def write_config(self, target: Image, guest_cosi: str, health_check: bool) -> str:
        """An image-only Host Configuration, which keeps the installed storage layout."""
        content = f'image:\n  url: file://{guest_cosi}\n  sha384: "{target.sha384}"\n'
        if health_check:
            content += HEALTH_CHECK_CONFIG
        remote = f"{GUEST_DIR}/trident-{target.sha384[:16]}{'-health' if health_check else ''}.yaml"
        ssh(f"cat > {shlex.quote(remote)}", input=content)
        self.save("trident-config.yaml", content)
        return remote

    def two_step(self) -> bool:
        if self.single_step:
            return False
        if self.state.two_step is None:
            usage = ssh("sudo trident update --help 2>&1", check=False).stdout
            self.state.two_step = "--allowed-operations" in usage
            self.state.save()
            if not self.state.two_step:
                warn("this Trident's update has no --allowed-operations, so it stages and finalizes in one step")
        return bool(self.state.two_step)

    def stage(self, target: Image, *, health_check: bool = False, check_id: str = "A1") -> Probe:
        """Write target to the inactive slot, without rebooting: copy the COSI in, write the Host
        Configuration, and run trident update --allowed-operations stage."""
        before = self.probe("before-stage")
        if before.state not in (None, "provisioned", "ab-update-staged"):
            raise StepError(f"Trident is in {before.state}; finish that, or restore a snapshot, before staging")
        if before.state is None:
            log(f"Trident has no Host Status yet ({first_line(before.status_error) or 'no output'}); "
                "the first update initializes its datastore")
        log(f"staging {target.name} (OS {target.version or '?'}, /usr root hash {short(target.roothash)}) "
            f"over {image_name(before.roothash)} on {before.pair_text}")
        self.write_markers()
        guest_cosi = self.copy_cosi(target, check_id)
        config = self.write_config(target, guest_cosi, health_check)
        two_step = self.two_step()
        self.state.staged = {
            "image": target.name, "sha384": target.sha384, "roothash": target.roothash,
            "guestCosi": guest_cosi, "config": config, "healthCheck": health_check,
            "fromPair": list(before.pair), "fromVolume": before.active_volume,
            "fromRoothash": before.roothash, "twoStep": two_step,
        }
        self.state.save()
        if not two_step:
            self.checks.skip(check_id, "staging leaves the VM on its slot", "this Trident stages and finalizes in one step")
            return before
        with self.timed("stage"):
            # With "=", so the option, which takes several values, doesn't take the config's path too.
            result = self.trident(f"update --allowed-operations=stage {shlex.quote(config)}", "stage",
                                  timeout=STAGE_TIMEOUT)
        after = self.probe("after-stage")
        if result.returncode != 0:
            output = f"{result.stdout}\n{result.stderr}"
            self.state.staged = None
            self.state.save()
            if health_check and "health" in output.lower() and "unknown field" in output.lower():
                raise Skip(f"this Trident rejects a health section in an image-only Host Configuration: "
                           f"{last_lines(result.stderr or result.stdout, 3)}")
            self.checks.expect(False, check_id, "trident update --allowed-operations stage succeeds",
                               f"exit {result.returncode}: {last_lines(result.stderr or result.stdout)}")
            raise StepError("staging failed")
        c = self.checks
        c.expect(after.boot_id == before.boot_id, check_id, "staging doesn't reboot the VM")
        c.expect(after.state == "ab-update-staged", check_id, "servicingState is ab-update-staged", f"it's {after.state}")
        c.expect(after.pair == before.pair, check_id, "the VM still runs the slot it ran",
                 f"{before.pair_text} -> {after.pair_text}")
        if before.active_volume:
            c.expect(after.active_volume == before.active_volume, check_id, "abActiveVolume is unchanged",
                     f"{before.active_volume} -> {after.active_volume}")
        c.expect(after.last_error is None, check_id, "lastError is empty", after.last_error or "")
        self.record("stage", image=target.name, healthCheck=health_check)
        return after

    def wait_for_reboot(self, before: Probe, result: subprocess.CompletedProcess[str], what: str) -> str:
        """Wait for the reboot a Trident command starts, and return the new boot's ID. The reboot
        ends the SSH session, so ssh exits 255, or 0 if the command returned first. Any other
        status is the command's failure, unless the VM rebooted anyway."""
        if result.returncode not in (0, 255):
            new = wait_for_new_boot(before.boot_id, 60, required=False)
            if new is None:
                raise StepError(f"{what} exited {result.returncode} and didn't reboot the VM: "
                                f"{last_lines(result.stderr or result.stdout)}")
        else:
            new = wait_for_new_boot(before.boot_id, BOOT_TIMEOUT)
        wait_settled()
        return str(new)

    def probe_after_boot(self, boot: str, label: str) -> Probe:
        """Probe the VM in the boot it came up in. If that boot goes down first, as it does when a
        trident unit commits at boot and a health check rolls it back, probe the next one."""
        try:
            return self.probe(label)
        except StepError:
            wait_for_new_boot(boot, BOOT_TIMEOUT)
            wait_settled()
            return self.probe(label)

    def finalize(self, *, verify: bool = True) -> Probe:
        """Finalize what's staged, which sets BootNext and reboots into the new slot, and check
        the VM booted it: A2, the slot and its root hash, and A3, the markers and a read-only /usr."""
        staged = self.state.staged
        if not staged:
            raise StepError("nothing is staged; `stage` first")
        before = self.probe("before-finalize")
        config = shlex.quote(staged["config"])
        if staged["twoStep"]:
            if before.state != "ab-update-staged":
                raise StepError(f"Trident is in {before.state}, not ab-update-staged; stage again")
            arguments, timeout = f"update --allowed-operations=finalize {config}", COMMAND_TIMEOUT
        else:
            arguments, timeout = f"update {config}", STAGE_TIMEOUT
        self.before_reboot()
        with self.timed("finalize"):
            result = self.trident(arguments, "finalize", timeout=timeout)
            boot = self.wait_for_reboot(before, result, "trident update")
        after = self.probe_after_boot(boot, "after-finalize")
        self.record("finalize", image=staged["image"])
        log(f"the VM booted {image_name(after.roothash)} on {after.pair_text}; Trident is {after.state}")
        if not verify:
            return after
        c = self.checks
        c.expect(after.pair != tuple(staged["fromPair"]), "A2", "the VM booted the other slot",
                 f"it booted {after.pair_text} again")
        if staged["roothash"]:
            c.expect(after.roothash == staged["roothash"], "A2", f"/usr's verity root hash is {staged['image']}'s",
                     f"{short(after.roothash)} != {short(staged['roothash'])}")
        self.learn(after, staged["image"])
        self.check_persistence("A3", after)
        self.check_trident_agent(after)
        self.after_boot(after)
        return after

    def commit(self, *, check_id: str = "A4") -> Probe:
        """Commit the boot into the new slot, unless a trident unit did at boot, and check A4: the
        Host Status is provisioned on the new volume, with the update's image, and no lastError."""
        before = self.probe("before-commit")
        staged = self.state.staged
        if before.state == "provisioned":
            log("Trident has committed this boot already (the image's trident service commits at boot)")
            after = before
        elif before.state in FINALIZED_STATES or before.state == HEALTH_FAILED_STATE:
            with self.timed("commit"):
                after = self.drive_to_terminal(before)
        else:
            raise StepError(f"Trident is in {before.state}; there's nothing to commit")
        if not staged:
            return after
        c = self.checks
        c.expect(after.state == "provisioned", check_id, "servicingState is provisioned", f"it's {after.state}")
        c.expect(after.last_error is None, check_id, "lastError is empty", after.last_error or "")
        if staged["fromVolume"]:
            c.expect(after.active_volume not in (None, staged["fromVolume"]), check_id, "abActiveVolume flipped",
                     f"{staged['fromVolume']} -> {after.active_volume}")
        else:
            c.expect(after.active_volume is not None, check_id, "abActiveVolume is set", "it's empty")
        c.expect(after.pair != tuple(staged["fromPair"]), check_id, "the VM runs the new slot",
                 f"it runs {after.pair_text}")
        if staged["roothash"]:
            c.expect(after.roothash == staged["roothash"], check_id, f"/usr's root hash is {staged['image']}'s",
                     f"{short(after.roothash)} != {short(staged['roothash'])}")
        c.expect(after.image_url == f"file://{staged['guestCosi']}", check_id,
                 "the Host Status names the update's image URL", f"it names {after.image_url}")
        c.expect((after.image_sha384 or "").lower() == staged["sha384"], check_id,
                 "the Host Status names the update's metadata sha384", f"it names {after.image_sha384}")
        self.check_slot_agrees(check_id, after)
        self.check_boot_order(check_id, after)
        self.state.committed_update = {"image": staged["image"], "fromPair": staged["fromPair"],
                                       "fromRoothash": staged["fromRoothash"], "toPair": list(after.pair)}
        self.state.staged = None
        self.learn(after, staged["image"])
        self.record("commit", image=staged["image"], activeVolume=after.active_volume)
        self.release()
        return after

    def update(self, target: Image, *, commit: bool = True) -> Probe:
        self.stage(target)
        after = self.finalize()
        return self.commit() if commit else after

    def drive_to_terminal(self, current: Probe) -> Probe:
        """Take Trident from a finalized state to provisioned: commit, which reboots into the
        previous slot when a health check fails, and commit again there. The states passed are
        kept in self.seen."""
        from_pair = tuple(self.state.staged["fromPair"]) if self.state.staged else None
        deadline = time.monotonic() + 3 * BOOT_TIMEOUT
        commits = 0
        boot = current.boot_id
        while True:
            self.seen.append(current.state or "none")
            if current.state == "provisioned":
                return current
            if time.monotonic() > deadline:
                raise StepError(f"Trident didn't reach provisioned in {duration(3 * BOOT_TIMEOUT)}; it's {current.state}")
            if current.state == HEALTH_FAILED_STATE and from_pair and current.pair != from_pair:
                log("a health check failed; waiting for Trident to reboot into the previous slot")
                boot = str(wait_for_new_boot(boot, BOOT_TIMEOUT))
                wait_settled()
            elif current.state in FINALIZED_STATES or current.state == HEALTH_FAILED_STATE:
                commits += 1
                if commits > 3:
                    raise StepError(f"Trident is still {current.state} after {commits - 1} commits")
                result = self.trident("commit", f"commit-{commits}")
                if result.returncode == 255:
                    # ssh lost the VM: commit rebooted it, as it does into the previous slot after a
                    # failed health check.
                    boot = str(wait_for_new_boot(boot, BOOT_TIMEOUT))
                    wait_settled()
                elif result.returncode != 0:
                    # Commit exits non-zero after it completes a rollback too, to report the failed
                    # health check that caused it, so what counts is whether the state moved on.
                    after = self.probe_after_boot(boot, f"after-commit-{commits}")
                    if (after.boot_id, after.state) == (boot, current.state):
                        # Nothing moved yet: give a reboot that's on its way a minute.
                        new = wait_for_new_boot(boot, 60, required=False)
                        if new is None:
                            raise StepError(f"trident commit exited {result.returncode} and changed nothing: "
                                            f"{last_lines(result.stderr or result.stdout)}")
                        boot = new
                        wait_settled()
                    elif after.state != current.state:
                        log(f"trident commit exited {result.returncode}, and Trident is {after.state}")
            elif current.state is None:
                raise StepError(f"Trident's Host Status can't be read: {last_lines(current.status_error, 3)}")
            else:
                raise StepError(f"Trident is {current.state}, which commit doesn't finish")
            current = self.probe_after_boot(boot, f"state-{len(self.seen)}")
            boot = current.boot_id

    def reboot(self, *, hard: bool = False) -> tuple[Probe, Probe]:
        before = self.probe("before-reboot")
        self.before_reboot()
        log("resetting the VM" if hard else "rebooting the VM")
        with self.timed("reboot"):
            if hard:
                with QMP() as qmp:
                    qmp.execute("system_reset")
            else:
                ssh("sudo systemctl reboot", check=False, timeout=30)
            boot = str(wait_for_new_boot(before.boot_id, BOOT_TIMEOUT))
            wait_settled()
        after = self.probe_after_boot(boot, "after-reboot")
        log(f"the VM booted {image_name(after.roothash)} on {after.pair_text}; Trident is {after.state}")
        self.record("reboot", hard=hard)
        self.after_boot(after)
        self.release()
        return before, after

    def previous_slot(self, current: Probe) -> dict[str, str] | None:
        for device, slot in self.state.slots.items():
            if device != current.data_device:
                return slot
        return None

    def rollback(self, *, check_only: bool = False, check_id: str = "R1") -> Probe:
        """Roll the last A/B update back with trident rollback --ab, and check R1: the VM runs the
        previous slot and image, provisioned, with no lastError."""
        before = self.probe("before-rollback")
        if ssh("sudo trident rollback --help", check=False).returncode != 0:
            raise Skip("this Trident has no rollback command")
        checked = self.trident("rollback --check", "rollback-check")
        kind = (checked.stdout.strip().splitlines() or [""])[-1].strip().lower()
        if check_only:
            log(f"trident rollback --check: {kind or 'nothing'} (exit {checked.returncode})")
            return before
        c = self.checks
        if not c.expect(checked.returncode == 0 and kind == "ab", check_id,
                        "trident rollback --check reports an A/B rollback", f"it reports {kind!r} (exit {checked.returncode})"):
            raise StepError("there's no A/B update to roll back")
        self.save("rollback-target.yaml", self.trident("get rollback-target", "rollback-target").stdout)
        expected = self.previous_slot(before)
        self.seen = []
        self.before_reboot()
        with self.timed("rollback"):
            result = self.trident("rollback --ab", "rollback")
            boot = self.wait_for_reboot(before, result, "trident rollback --ab")
            after = self.drive_to_terminal(self.probe_after_boot(boot, "after-rollback-boot"))
        c.expect(after.pair != before.pair, check_id, "the VM booted the other slot", f"it booted {after.pair_text} again")
        if expected:
            c.expect(after.roothash == expected["roothash"], check_id,
                     f"/usr's root hash is {expected['image']}'s, which that slot held",
                     f"it's {image_name(after.roothash)}")
        c.expect(after.state == "provisioned", check_id, "servicingState is provisioned", f"it's {after.state}")
        if before.active_volume:
            c.expect(after.active_volume not in (None, before.active_volume), check_id, "abActiveVolume flipped back",
                     f"{before.active_volume} -> {after.active_volume}")
        c.expect(after.last_error is None, check_id, "lastError is empty", after.last_error or "")
        self.check_slot_agrees(check_id, after)
        self.check_boot_order(check_id, after)
        self.check_persistence(check_id, after)
        self.check_trident_agent(after)
        self.after_boot(after)
        self.release()
        self.state.committed_update = None
        self.state.staged = None
        self.learn(after)
        self.record("rollback", image=image_name(after.roothash), states=self.seen)
        return after

    def health_rollback(self, target: Image, *, check_id: str = "H1") -> Probe:
        """Update to target with a health check that fails, and check H1: commit rolls back, and
        the VM ends provisioned on the slot and image it ran, with lastError naming the check."""
        before = self.probe("before-health-rollback")
        if before.state not in (None, "provisioned"):
            raise StepError(f"Trident is in {before.state}; finish that before updating")
        self.stage(target, health_check=True, check_id=check_id)
        staged = self.state.staged or {}
        self.seen = []
        with self.timed("health-rollback"):
            booted = self.finalize(verify=False)
            after = self.drive_to_terminal(booted)
        log(f"Trident went {' -> '.join(self.seen)}")
        last_error = after.last_error or self.trident("get last-error", "last-error").stdout.strip()
        c = self.checks
        c.expect(after.state == "provisioned", check_id, "Trident ends provisioned", f"it's {after.state}")
        c.expect(after.pair == before.pair, check_id, "the VM is back on the slot it ran before the update",
                 f"{before.pair_text} -> {after.pair_text}")
        c.expect(after.roothash == before.roothash, check_id, "/usr's root hash is the previous image's",
                 f"it's {image_name(after.roothash)}")
        c.expect(HEALTH_CHECK_NAME in last_error or "health" in last_error.lower(), check_id,
                 "lastError reports the failed health check", last_error[:400] or "lastError is empty")
        logs = ssh("sudo sh -c 'ls /var/lib/trident/trident-health-check-failure-*.log' 2>/dev/null",
                   check=False).stdout.split()
        if c.expect(bool(logs), check_id, "Trident wrote a health check failure log in /var/lib/trident"):
            self.save("health-check-failure.log", ssh(f"sudo cat {shlex.quote(logs[-1])}", check=False).stdout)
        if before.boot_count > 1 or after.boot_count > 1:
            booted_times = after.boot_count - before.boot_count
            c.expect(booted_times >= 2, check_id, "the VM booted the update, then the previous slot",
                     f"{booted_times} boot(s) since the update")
        else:
            c.skip(check_id, "the VM booted the update, then the previous slot", "the journal keeps no earlier boots")
        self.check_boot_order(check_id, after)
        self.check_persistence(check_id, after)
        self.check_trident_agent(after)
        self.after_boot(after)
        self.release()
        if booted.data_device and booted.data_device != after.data_device and staged:
            self.state.slots[booted.data_device] = {"roothash": staged.get("roothash", ""), "image": staged.get("image", "")}
        self.state.staged = None
        self.state.committed_update = None
        self.record("health-rollback", image=staged.get("image", ""), states=self.seen)
        return after

    def verify(self, *, expect_target: str = "", expect_slot: str = "", expect_state: str = "",
               check_id: str = "V1") -> Probe:
        current = self.probe("verify")
        log(f"the VM runs {image_name(current.roothash)} on {current.pair_text}; Trident is {current.state}")
        c = self.checks
        if expect_target:
            image = resolve_target(expect_target, self.state, lambda: current.roothash)
            c.expect(current.roothash == image.roothash, check_id, f"the VM runs {image.name}",
                     f"it runs {image_name(current.roothash)}")
        if expect_slot:
            volume = f"volume-{expect_slot}"
            c.expect(current.active_volume == volume, check_id, f"abActiveVolume is {volume}",
                     f"it's {current.active_volume}")
        if expect_state:
            c.expect(current.state == expect_state, check_id, f"servicingState is {expect_state}", f"it's {current.state}")
        c.expect(current.data_device.startswith("/dev/"), check_id, "/usr is a dm-verity device", current.pair_text)
        self.check_slot_agrees(check_id, current)
        if current.state == "provisioned":
            self.check_boot_order(check_id, current)
        self.check_persistence(check_id, current)
        self.check_trident_agent(current)
        self.after_boot(current)
        return current

    # Shared checks

    def check_persistence(self, check_id: str, current: Probe) -> None:
        c = self.checks
        if not self.state.markers:
            c.skip(check_id, "the markers persist", "no markers have been written yet")
        else:
            for path in MARKER_PATHS:
                missing = [token for token in self.state.markers if token not in current.markers.get(path, [])]
                c.expect(not missing, check_id, f"{path} holds every marker the harness wrote",
                         f"missing {', '.join(missing)}")
        c.expect(not current.usr_writable, check_id, "/usr is read-only")

    def check_trident_agent(self, current: Probe, check_id: str = "T1") -> None:
        for ok, description, detail in trident_agent_findings(current, trident_agent_pinned()):
            self.checks.expect(ok, check_id, description, detail)
        agent = current.trident_agent
        if agent.get("LoadState") == "loaded":
            preflight = "; ".join(line.removeprefix("trident-acl-agent preflight: ")
                                  for line in current.trident_agent_preflight[-2:]) or "none"
            log(f"{TRIDENT_AGENT_UNIT} is {agent.get('ActiveState', '?')}/{agent.get('SubState', '?')} after "
                f"{agent.get('NRestarts', '?')} restarts; tridentd socket {agent.get('socket', '?')}; "
                f"last preflight: {preflight}")

    def check_boot_order(self, check_id: str, current: Probe) -> None:
        if not current.boot_current or not current.boot_order:
            self.checks.skip(check_id, "BootCurrent is first in BootOrder", "the UEFI variables can't be read")
            return
        self.checks.expect(current.boot_order[0] == current.boot_current, check_id,
                           f"BootCurrent {current.boot_label(current.boot_current)} is first in BootOrder",
                           f"BootOrder is {', '.join(current.boot_label(n) for n in current.boot_order)}")

    def check_slot_agrees(self, check_id: str, current: Probe) -> None:
        """Trident's abActiveVolume and the devices /usr was booted from name the same slot."""
        if current.state != "provisioned" or not current.active_volume:
            return
        known = self.state.slot_pairs.get(current.active_volume)
        if known is None:
            self.state.slot_pairs[current.active_volume] = list(current.pair)
            return
        self.checks.expect(tuple(known) == current.pair, check_id,
                           f"abActiveVolume {current.active_volume} is the slot the VM booted",
                           f"{current.active_volume} was {', '.join(known)}; the VM booted {current.pair_text}")

    def expect_same_slot(self, check_id: str, before: Probe, after: Probe) -> None:
        c = self.checks
        c.expect(after.boot_id != before.boot_id, check_id, "the VM rebooted")
        c.expect(after.pair == before.pair, check_id, "the VM booted the same slot again",
                 f"{before.pair_text} -> {after.pair_text}")
        c.expect(after.roothash == before.roothash, check_id, "/usr's root hash is unchanged",
                 f"{image_name(before.roothash)} -> {image_name(after.roothash)}")
        c.expect(after.state == "provisioned", check_id, "servicingState is provisioned", f"it's {after.state}")
        if before.active_volume:
            c.expect(after.active_volume == before.active_volume, check_id, "abActiveVolume is unchanged",
                     f"{before.active_volume} -> {after.active_volume}")
        self.check_boot_order(check_id, after)
        self.check_persistence(check_id, after)
        self.check_trident_agent(after)

    # Cluster mode

    @property
    def cluster(self) -> Any:
        return cluster_of(self.state)

    def expect_all(self, check_id: str, findings: list[tuple[bool, str, str]]) -> bool:
        ok = True
        for passed, description, detail in findings:
            ok = self.checks.expect(passed, check_id, description, detail) and ok
        return ok

    def join(self) -> None:
        """Join the VM to the cluster as a flex node, with the agent in containers, as
        docs/usage/azure-container-linux.md installs it: build the agent's image from this
        repository, push it to the local registry, render scripts/aks-flex-node-agent-container.bu
        for it, apply that on the VM, and start bootstrap. Then check it (K1)."""
        import flexinstall

        cluster = self.cluster
        spec = cluster.spec
        with self.timed("agent image"):
            flexinstall.build_agent_image(REPO, AGENT_IMAGE_TAG, run=host_run, log=log)
            registry = flexinstall.Registry(LOCAL_REGISTRY, LOCAL_REGISTRY_PORT, guest_host=GUEST_HOST,
                                            run=host_run, log=log)
            registry.start()
            self.state.agent_image = registry.push(AGENT_IMAGE_TAG, AGENT_REPOSITORY, "acl-abupdate")
            self.state.save()
        log(f"the VM pulls the agent's image as {self.state.agent_image}")
        token = cluster.create_bootstrap_token()
        cluster.register_machine(NODE_NAME)
        import flexcluster

        node = flexinstall.FlexNode(node_name=NODE_NAME, node_ip=spec.vm_ip, api_server=spec.api_server,
                                    ca_pem=cluster.ca_pem(), kubernetes_version=cluster.kubernetes_version(),
                                    bootstrap_token=token, agent_image=self.state.agent_image,
                                    dns_service_ip=cluster.dns_service_ip(), agent_pool=flexcluster.AGENT_POOL)
        with self.timed("join"):
            flexinstall.provision(node, repo=REPO, ssh=ssh, ssh_bytes=ssh_bytes, run=host_run, log=log)
            flexinstall.wait_for_bootstrap(ssh, timeout=JOIN_TIMEOUT, log=log)
            flexinstall.wait_for_agent(ssh, timeout=600, log=log)
            cluster.wait_ready(NODE_NAME, JOIN_TIMEOUT)
        self.expect_all("K1", flexinstall.bootstrap_findings(ssh) + flexinstall.agent_findings(ssh)
                        + cluster.node_findings(NODE_NAME, expected_ip=spec.vm_ip))
        product_uuid = ssh("sudo cat /sys/class/dmi/id/product_uuid 2>/dev/null", check=False).stdout.strip()
        self.expect_all("K1", system_uuid_findings(cluster.node(NODE_NAME), product_uuid, self.state.smbios_uuid))
        self.record("join", node=NODE_NAME, image=self.state.agent_image)

    def connect_trident_agent(self) -> None:
        """Write trident-acl-agent's kubeconfig, with the kubelet's identity, which is what the AKS
        contract's /var/lib/kubelet/kubeconfig would give it, and check it connects (T2). The flex
        agent is to write this file itself; until it does, the harness stands in."""
        import flexinstall

        cluster = self.cluster
        script = flexinstall.trident_kubeconfig_script(
            server=f"https://{cluster.spec.api_server}", ca_pem=cluster.ca_pem(),
            path=trident_agent_pinned()[TRIDENT_AGENT_KUBECONFIG_VARIABLE], unit=TRIDENT_AGENT_UNIT)
        result = ssh("sudo bash -s", input=script, timeout=120, check=False)
        if result.returncode != 0:
            raise StepError(f"couldn't write trident-acl-agent's kubeconfig: {last_lines(result.stderr or result.stdout)}")
        self.check_trident_connection()

    def check_trident_connection(self, check_id: str = "T2") -> None:
        """trident-acl-agent reaches the API server as the node, and stays running."""
        if self.cluster is None:
            return
        c = self.checks
        pinned = trident_agent_pinned()
        path = pinned[TRIDENT_AGENT_KUBECONFIG_VARIABLE]
        mode = ssh(f"sudo stat -c %a {shlex.quote(path)}", check=False).stdout.strip()
        c.expect(mode == "600", check_id, f"{path} exists, with mode 0600", f"its mode is {mode or 'missing'}")
        environment = " ".join(shlex.quote(f"{name}={value}") for name, value in pinned.items())
        result = ssh(f"sudo env {environment} /usr/bin/trident-acl-agent --validate-connection kubernetes",
                     check=False, timeout=60)
        if result.returncode == 2 and "validate-connection" in result.stderr:
            c.skip(check_id, "trident-acl-agent reaches the API server as the node",
                   "this trident-acl-agent has no --validate-connection")
        else:
            c.expect(result.returncode == 0, check_id, "trident-acl-agent reaches the API server as the node",
                     last_lines(result.stderr or result.stdout, 4))

        def restarts() -> str:
            return ssh(f"systemctl show {TRIDENT_AGENT_UNIT} -p NRestarts --value", check=False).stdout.strip()

        first = restarts()
        time.sleep(15)
        second = restarts()
        active = ssh(f"systemctl is-active {TRIDENT_AGENT_UNIT}", check=False).stdout.strip()
        c.expect(active == "active" and first == second, check_id, f"{TRIDENT_AGENT_UNIT} stays running",
                 f"it's {active}, and restarted {first} -> {second} times in 15s")

    def agent_manifest(self) -> dict[str, str]:
        import flexinstall

        result = ssh("sudo bash -s", input=flexinstall.STATE_MANIFEST_SCRIPT, timeout=180, check=False)
        return flexinstall.parse_state_manifest(result.stdout)

    def before_reboot(self, check_id: str = "D1") -> None:
        """In cluster mode, before the harness reboots the VM: record the agent's state for K4,
        then cordon and drain the node, as docs/usage/azure-container-linux.md says to before an
        OS update."""
        cluster = self.cluster
        if cluster is None:
            return
        self.flex_baseline = self.agent_manifest()
        if not self.drain:
            return
        cluster.cordon(NODE_NAME)
        self.state.cordoned = True
        self.state.save()
        try:
            with self.timed("drain"):
                cluster.drain(NODE_NAME)
        except StepError as error:
            self.checks.expect(False, check_id, "the node drains", str(error))
            raise
        node = cluster.node(NODE_NAME) or {}
        self.checks.expect(bool(node.get("spec", {}).get("unschedulable")), check_id, "the node is cordoned and drained",
                           "it's schedulable")

    def wait_lease(self, since: datetime.datetime | None, timeout: float = 90) -> None:
        """Wait for the kubelet to renew the node's Lease after since. It renews every 10s, but the
        renewal right after a boot or a restore may carry the clock from before the harness stepped
        it, so the check waits for the next one rather than reading that."""
        if since is None:
            return

        def renewed() -> bool:
            renewal = self.cluster.lease_renew_time(NODE_NAME)
            return renewal is not None and renewal > since

        poll(timeout, renewed, 5)

    def after_boot(self, current: Probe, check_id: str = "K2") -> None:
        """In cluster mode, after a boot: the node is Ready again with a Lease renewed this boot,
        the agent's container and machine run (K2), its state is what it was before the reboot
        (K4), no new AVC denial names it (K5), and trident-acl-agent still connects (T2)."""
        cluster = self.cluster
        if cluster is None:
            return
        import flexinstall

        try:
            with self.timed("ready"):
                cluster.wait_ready(NODE_NAME, READY_TIMEOUT)
                self.wait_lease(current.booted_at)
        except StepError as error:
            self.checks.expect(False, check_id, "the node is Ready again", str(error))
        self.expect_all(check_id, cluster.node_findings(NODE_NAME, expected_ip=cluster.spec.vm_ip,
                                                        since=current.booted_at)
                        + flexinstall.agent_findings(ssh))
        if self.flex_baseline is not None:
            self.expect_all("K4", flexinstall.manifest_findings(self.flex_baseline, self.agent_manifest()))
            self.flex_baseline = None
        self.check_denials()
        self.check_trident_connection()

    def check_denials(self, check_id: str = "K5") -> None:
        import flexinstall

        denials = flexinstall.avc_denials(ssh)
        unknown = [line for line in denials if not flexinstall.denial_issue(line)]
        if denials:
            self.save("avc-denials.txt", "\n".join(denials) + "\n")
        log(f"{len(denials)} AVC denial(s) this boot name the agent, {len(denials) - len(unknown)} of them known")
        self.checks.expect(not unknown, check_id, "no AVC denial this boot names the agent, other than known ones",
                           f"{len(unknown)} do, such as {unknown[0][:300]}" if unknown else "")

    def release(self, check_id: str = "D2") -> None:
        """Uncordon the node the harness drained, and check a workload runs on it again (K3)."""
        cluster = self.cluster
        if cluster is None or not self.state.cordoned:
            return
        cluster.uncordon(NODE_NAME)
        self.state.cordoned = False
        self.state.save()
        node = cluster.node(NODE_NAME) or {}
        self.checks.expect(not node.get("spec", {}).get("unschedulable"), check_id, "the node is schedulable again",
                           "it's still cordoned")
        self.expect_all("K3", cluster.probe_findings(NODE_NAME))

    def check_restored(self, record: dict[str, Any] | None, since: datetime.datetime, check_id: str = "S1") -> None:
        """After a restore in cluster mode: the VM's clock is the host's, the node is Ready with a
        Lease renewed since the restore, and the fields of the Node the harness manages are as
        they were when the snapshot was saved."""
        cluster = self.cluster
        if cluster is None or not record or not record.get("node"):
            return
        c = self.checks
        guest = ssh("date +%s.%N", check=False).stdout.strip()
        offset = abs(float(guest) - time.time()) if guest.replace(".", "", 1).isdigit() else float("inf")
        c.expect(offset < 2, check_id, "the VM's clock is within 2s of the host's", f"it's {offset:.1f}s off")
        try:
            cluster.wait_ready(NODE_NAME, READY_TIMEOUT)
            self.wait_lease(since)
        except StepError as error:
            c.expect(False, check_id, "the node is Ready after the restore", str(error))
        self.expect_all(check_id, cluster.node_findings(NODE_NAME, expected_ip=cluster.spec.vm_ip, since=since))
        saved, now = record["node"], cluster.record(NODE_NAME, annotation_prefix())
        differing = sorted(key for key in saved if key not in ("uid", "cluster", "identity")
                           and now.get(key) != saved.get(key))
        c.expect(not differing, check_id, "the Node's annotations, cordon, and taints are as the snapshot recorded them",
                 f"these differ: {', '.join(differing)}")

    # Annotation-driven updates: the harness is the orchestrator AKS will be. It writes
    # acl.azure.com/update-request on the Node, and trident-acl-agent stages, finalizes, or rolls
    # back through tridentd, getting the image from the harness's fake update server, omaha.py.

    def trust_update_server(self) -> Any:
        """Make the update server's TLS files, and have the VM trust its CA, as trident's own tests
        do: in the system store, through update-ca-trust, and hashed into /etc/pki/tls/certs, which
        OpenSSL reads and update-ca-trust doesn't fill. ACL keeps /etc across A/B updates, which the
        agent needs: it reports an update done to the server after the reboot."""
        import omaha

        tls = omaha.make_tls(OMAHA_DIR, list(omaha.DEFAULT_SANS), run=host_run, log=log)
        ca = tls.ca_cert.read_text()
        digest = hashlib.sha256(ca.encode()).hexdigest()
        present = ssh(f"sudo sha256sum {OMAHA_CA_ANCHOR} {OMAHA_CA_CERTS} 2>/dev/null", check=False).stdout.split()
        if present[::2] != [digest, digest]:
            ssh(f"sudo install -D -m 0644 /dev/stdin {OMAHA_CA_ANCHOR} && sudo update-ca-trust extract && "
                f"sudo install -m 0644 {OMAHA_CA_ANCHOR} {OMAHA_CA_CERTS} && sudo openssl rehash /etc/pki/tls/certs",
                input=ca, timeout=180)
            log("the VM trusts the update server's CA")
        return tls

    @contextlib.contextmanager
    def update_server(self, package: Any) -> Iterator[Any]:
        """Serve package from the fake update server while the block runs, for the harness's app
        and the package's own track, and check trident-acl-agent reaches it over TLS (N0). What the
        server saw goes in the artifacts."""
        import omaha

        tls = self.trust_update_server()
        server = omaha.OmahaServer(port=OMAHA_PORT, tls=tls, public_host=GUEST_HOST, log=log)
        with server:
            server.offer(package, app_id=ANNOTATION_APP_ID, track=annotation_track(package.version))
            try:
                environment = " ".join(shlex.quote(f"{name}={value}") for name, value in {
                    **trident_agent_pinned(), "TRIDENT_ACL_AGENT_NEBRASKA_ENDPOINT": server.update_url}.items())
                result = ssh(f"sudo env {environment} /usr/bin/trident-acl-agent --validate-connection nebraska",
                             check=False, timeout=60)
                if not self.checks.expect(result.returncode == 0, "N0",
                                          "trident-acl-agent reaches the update server over TLS",
                                          last_lines(result.stderr or result.stdout, 4)):
                    raise StepError("trident-acl-agent can't reach the update server; see the update server's "
                                    "requests in the artifacts for a TLS error")
                yield server
            finally:
                self.save("update-server.json", json.dumps({"events": server.events, "requests": server.requests},
                                                           indent=2, default=str) + "\n")

    def annotate(self, request: dict[str, Any], label: str = "") -> None:
        import omaha

        patch = omaha.request_patch(annotation_prefix(), request)
        self.cluster.kubectl("patch", "node", NODE_NAME, "--type", "merge", "-p", json.dumps(patch))
        self.save(f"request-{label or request['operation']}.json", json.dumps(request, indent=2) + "\n")
        log(f"requested {request['operation']} {request['operationId']} on node {NODE_NAME}")

    def annotate_raw(self, value: str, label: str) -> None:
        """Write the request annotation exactly as given, which may be a request the agent can't
        parse, or the same request written differently."""
        import omaha

        key = omaha.annotation_key(annotation_prefix(), omaha.REQUEST_KEY)
        self.cluster.kubectl("patch", "node", NODE_NAME, "--type", "merge", "-p",
                             json.dumps({"metadata": {"annotations": {key: value}}}))
        self.save(f"request-{label}.txt", value + "\n")
        log(f"wrote {label} to {key} on node {NODE_NAME}")

    def node_status(self, key: str) -> Any:
        """A status annotation on the Node now, whichever operation it's for, or None."""
        import omaha

        node = self.cluster.node(NODE_NAME)
        return omaha.parse_status(node, annotation_prefix(), key) if node else None

    def wait_status(self, operation_id: str, key: str, timeout: float, *,
                    observe: Callable[[Any], None] | None = None) -> Any:
        """Wait for trident-acl-agent's status annotation for an operation to be terminal. observe
        sees each status read on the way, every 5s."""
        import omaha

        deadline = time.monotonic() + timeout
        last = None
        while True:
            node = self.cluster.node(NODE_NAME)
            status = omaha.parse_status(node, annotation_prefix(), key, operation_id=operation_id) if node else None
            if status and observe:
                observe(status)
            if status and (last is None or (status.code, status.message) != (last.code, last.message)):
                log(f"{key}: {status.code}{': ' + status.message if status.message else ''}")
                last = status
            if status and status.terminal:
                return status
            if time.monotonic() > deadline:
                raise StepError(f"{key} for {operation_id} isn't terminal after {duration(timeout)}; it's "
                                f"{last.code if last else 'absent'}")
            time.sleep(5)

    def annotation_request(self, operation: str, common: dict[str, str], **changes: str) -> dict[str, str]:
        """A request for this update, with a new operationId."""
        import omaha

        return omaha.update_request(operation=operation, operation_id=str(uuid.uuid4()), **{**common, **changes})

    def annotation_stage(self, server: Any, target: Image, package: Any, common: dict[str, str], *,
                         observe: Callable[[Any], None] | None = None) -> tuple[Probe, dict[str, str]]:
        """Stage target by annotation and check it (N1): it ends Success, without a reboot, with
        Trident ab-update-staged and the COSI downloaded from the update server. Returns the probe
        from before, and the request."""
        c = self.checks
        before = self.probe("before-annotation-stage")
        if before.state not in (None, "provisioned"):
            raise StepError(f"Trident is in {before.state}; finish that before updating")
        self.write_markers()
        request, first = self.annotation_request("stage", common), len(server.requests)
        self.annotate(request)
        with self.timed("annotation stage"):
            staged = self.wait_status(request["operationId"], STATUS_KEY, STAGE_TIMEOUT, observe=observe)
        after = self.probe("after-annotation-stage")
        if not c.expect(staged.code == "Success", "N1", "trident-acl-agent stages the update the annotation requests",
                        f"{staged.code}: {staged.message}"):
            raise StepError("the annotation's stage failed")
        c.expect(after.boot_id == before.boot_id, "N1", "staging doesn't reboot the VM")
        c.expect(after.state == "ab-update-staged", "N1", "servicingState is ab-update-staged", f"it's {after.state}")
        c.expect(after.pair == before.pair, "N1", "the VM still runs the slot it ran")
        c.expect(any(r["kind"] == "image" for r in server.requests[first:]), "N1",
                 "tridentd downloaded the COSI from the update server")
        self.state.staged = {
            "image": target.name, "sha384": target.sha384, "roothash": target.roothash,
            "guestCosi": "", "config": "", "url": server.image_url(package), "healthCheck": False,
            "fromPair": list(before.pair), "fromVolume": before.active_volume, "fromRoothash": before.roothash,
            "twoStep": True, "nodeUpdateId": common["node_update_id"], "byAnnotation": True,
        }
        self.state.save()
        return before, request

    def annotation_finalize(self, server: Any, common: dict[str, str], before: Probe) -> str:
        """Finalize what the annotation staged, and check the agent reboots the VM into it, commits
        the boot, and reports the update done to the update server (N2). Returns the new boot's ID."""
        c = self.checks
        self.before_reboot()
        request, first_event = self.annotation_request("finalize", common), len(server.events)
        self.annotate(request)
        with self.timed("annotation finalize"):
            boot = str(wait_for_new_boot(before.boot_id, ANNOTATION_REBOOT_TIMEOUT))
            wait_settled()
            committed = self.wait_status(request["operationId"], COMMIT_STATUS_KEY, ANNOTATION_REBOOT_TIMEOUT)
        finalized = self.wait_status(request["operationId"], STATUS_KEY, 60)
        c.expect(finalized.code == "Success", "N2", "trident-acl-agent finalizes the update the annotation requests",
                 f"{finalized.code}: {finalized.message}")
        c.expect(committed.code == "Success", "N2", "trident-acl-agent commits the boot into the new slot",
                 f"{committed.code}: {committed.message}")
        try:
            server.wait_for_event("3", "2", timeout=300, start=first_event)
            reported = True
        except StepError:
            reported = False
        c.expect(reported, "N2", "the agent reported the update complete to the update server after the reboot",
                 f"it sent {[(e['eventtype'], e['eventresult']) for e in server.events[first_event:]]}")
        return boot

    def annotation_update(self, target: Image, *, contract_checks: bool = True, heartbeat: bool = False) -> Probe:
        """An A/B update the way AKS will drive one: stage, then finalize, by annotation, with
        trident-acl-agent pulling the image from the update server and committing after the boot
        (N1, N2). The VM ends where an update by the harness would leave it, so the same checks
        follow: A2, A3, A4, T1, and the node's.

        With contract_checks, the contract's rules for a staged update are checked between stage
        and finalize: a repeated request and a removed status (N5), and a finalize for another
        version (N4). With heartbeat, the update server serves the COSI slowly, so the stage runs
        for a few of the agent's heartbeats, and checks them (N6)."""
        package = package_for(target)
        common = {"target_version": package.version, "app_id": ANNOTATION_APP_ID,
                  "track": annotation_track(package.version), "node_update_id": str(uuid.uuid4())}
        observed: list[dict[str, Any]] = []
        with self.update_server(package) as server:
            common["server"] = server.update_url
            if heartbeat:
                server.throttle(max(1, package.size // ANNOTATION_HEARTBEAT_STAGE_SECONDS))
            before, stage = self.annotation_stage(server, target, package, common,
                                                  observe=(lambda status: observed.append(dict(status.raw)))
                                                  if heartbeat else None)
            if heartbeat:
                server.throttle(0)
                final = self.node_status(STATUS_KEY)
                interval = parse_duration(trident_agent_pinned().get(
                    "TRIDENT_ACL_AGENT_ORCHESTRATION_HEARTBEAT_INTERVAL", "60s"))
                self.save("stage-statuses.json", json.dumps(observed, indent=2) + "\n")
                self.expect_all("N6", heartbeat_findings(observed, final.raw if final else {}, interval))
            if contract_checks:
                self.check_staged_contract(server, stage, common)
            boot = self.annotation_finalize(server, common, before)
        after = self.probe_after_boot(boot, "after-annotation-finalize")
        self.record("annotation-update", image=target.name, nodeUpdateId=common["node_update_id"])
        self.check_update_result(after, url=server.image_url(package))
        return after

    def check_staged_contract(self, server: Any, stage: dict[str, str], common: dict[str, str]) -> None:
        """The contract's rules for an update that's staged. The agent keys work by operationId: a
        request it has answered is answered again from its cache, without doing anything (N5), and
        a status removed while its request stays is written back. A finalize names the version
        staged for its nodeUpdateId, or it's refused (N4)."""
        import omaha

        c = self.checks
        before = self.probe("before-staged-contract")
        status = self.node_status(STATUS_KEY)
        first = len(server.requests)
        # The same request, written differently, so the Node changes and the agent looks at it again.
        self.annotate_raw(json.dumps(stage, indent=2), "stage-again")
        time.sleep(ANNOTATION_SETTLE_SECONDS)
        again = self.node_status(STATUS_KEY)
        c.expect(again is not None and status is not None and status_content(again.raw) == status_content(status.raw),
                 "N5", "re-sending the stage request leaves its status as the agent wrote it",
                 f"{status.raw if status else None} -> {again.raw if again else None}")
        made = server.requests[first:]
        c.expect(not made, "N5", "re-sending the stage request doesn't reach the update server again",
                 f"it made {len(made)} request(s): {', '.join(r['kind'] for r in made)}")
        after = self.probe("after-stage-again")
        c.expect((after.boot_id, after.state) == (before.boot_id, "ab-update-staged"), "N5",
                 "Trident is still ab-update-staged, and the VM didn't reboot", f"it's {after.state}")

        self.cluster.kubectl("patch", "node", NODE_NAME, "--type", "merge", "-p",
                             json.dumps(omaha.clear_patch(annotation_prefix(), [omaha.STATUS_KEY])))
        log(f"removed {STATUS_KEY} from node {NODE_NAME}, leaving the stage request")
        try:
            restored = self.wait_status(stage["operationId"], STATUS_KEY, 60)
        except StepError:
            restored = None
        c.expect(restored is not None and status is not None
                 and status_content(restored.raw) == status_content(status.raw), "N5",
                 "the agent writes the stage's status back, the same but for lastUpdatedUtc, when it's removed",
                 f"it wrote {restored.raw if restored else 'nothing'}")

        wrong = self.annotation_request("finalize", common, target_version=bump_patch(common["target_version"]))
        self.annotate(wrong, "finalize-wrong-version")
        answer = self.wait_status(wrong["operationId"], STATUS_KEY, 120)
        after = self.probe("after-finalize-wrong-version")
        c.expect(answer.code == "InvalidRequest", "N4",
                 "a finalize whose targetVersion isn't the one staged is refused with InvalidRequest",
                 f"it was answered {answer.code}: {answer.message}")
        c.expect((after.boot_id, after.state) == (before.boot_id, "ab-update-staged"), "N4",
                 "the refused finalize leaves the update staged, without a reboot", f"it's {after.state}")

    def annotation_target_boot_failed(self, target: Image) -> None:
        """A finalize whose reboot lands on the old slot: the VM is reset as the new slot's kernel
        starts, after the firmware has used its one-time BootNext, so it boots its BootOrder's first
        entry, the slot it ran. The agent's commit after that boot must report TargetBootFailed, with
        Trident's ab-update-reboot-check, and a new update must then succeed (N7)."""
        c = self.checks
        package = package_for(target)
        common = {"target_version": package.version, "app_id": ANNOTATION_APP_ID,
                  "track": annotation_track(package.version), "node_update_id": str(uuid.uuid4())}
        with self.update_server(package) as server:
            common["server"] = server.update_url
            before, _ = self.annotation_stage(server, target, package, common)
            slot = kernel_slot(ssh("cat /proc/cmdline", check=False).stdout)
            self.before_reboot()
            request = self.annotation_request("finalize", common)
            offset = SERIAL_LOG.stat().st_size if SERIAL_LOG.exists() else 0
            self.annotate(request)
            command_line = reset_at_next_kernel(offset, ANNOTATION_REBOOT_TIMEOUT)
            booted = kernel_slot(command_line)
            if slot and booted:
                c.expect(booted != slot, "N7", f"the reset hit the first boot of the new slot, {booted}",
                         f"the kernel the reset stopped was slot {booted}'s, the slot the VM ran")
            boot = str(wait_for_new_boot(before.boot_id, ANNOTATION_REBOOT_TIMEOUT))
            wait_settled()
            committed = self.wait_status(request["operationId"], COMMIT_STATUS_KEY, ANNOTATION_REBOOT_TIMEOUT)
        after = self.probe_after_boot(boot, "after-target-boot-failed")
        running = kernel_slot(ssh("cat /proc/cmdline", check=False).stdout)
        log(f"after the reset, the VM runs {image_name(after.roothash)} on {after.pair_text} (slot {running or '?'}); "
            f"Trident is {after.state}, with {after.active_volume} active; the commit reports {committed.code}")
        self.state.staged = None
        if after.pair != before.pair:
            self.learn(after)
            self.after_boot(after)
            self.release()
            raise Skip("the firmware booted the new slot after the reset, so no boot failed")
        error = committed.raw.get("tridentError") or {}
        c.expect(committed.code == "TargetBootFailed", "N7", "the commit after the fallback reports TargetBootFailed",
                 f"it reports {committed.code} ({committed.message}), toVersion {committed.raw.get('toVersion')}, "
                 f"while the VM runs {image_name(after.roothash)}, the image it ran before the update")
        c.expect(error.get("subkind") == "ab-update-reboot-check", "N7",
                 "its tridentError is Trident's ab-update-reboot-check", f"it's {error or 'absent'}")
        c.expect(after.roothash == before.roothash, "N7", "the VM runs the image it ran", f"it runs {image_name(after.roothash)}")
        # Trident's Host Status has to name the slot the VM runs, or its next update would write the
        # slot that's running, which it takes for the inactive one.
        consistent = bool(running) and after.active_volume == f"volume-{running}"
        c.expect(consistent, "N7", "Trident's Host Status names the slot the VM runs",
                 f"abActiveVolume is {after.active_volume}, and the VM booted slot {running or '?'}")
        c.expect((after.image_sha384 or "").lower() != target.sha384, "N7",
                 "Trident's Host Status doesn't claim the update's image, which didn't boot",
                 f"it names {after.image_url}")
        self.check_trident_agent(after)
        self.after_boot(after)
        self.release()
        self.record("annotation-target-boot-failed", image=target.name, state=after.state, commit=committed.code)
        if not consistent:
            self.note("after the firmware fell back to the old slot, Trident's Host Status names "
                      f"{after.active_volume} and the update's image, while the VM runs slot {running or '?'} and the "
                      "image before it; a new update could write the running slot, so the VM is left as it is")
            c.skip("retry/N1", "a new update after the failed one",
                   "Trident's Host Status doesn't name the slot the VM runs; restore a snapshot before updating")
            return
        self.learn(after)
        with self.scope("retry"):
            log("a new update, with its own nodeUpdateId, after the failed one")
            self.annotation_update(target, contract_checks=False)

    def annotation_rollback(self) -> Probe:
        """Roll the last A/B update back by annotation (N3), then check as rollback does (R1)."""
        import omaha

        c = self.checks
        before = self.probe("before-annotation-rollback")
        if before.state != "provisioned":
            raise StepError(f"Trident is in {before.state}; there's nothing to roll back")
        expected = self.previous_slot(before)
        self.before_reboot()
        operation_id = str(uuid.uuid4())
        self.annotate(omaha.update_request(operation="rollback", operation_id=operation_id,
                                           node_update_id=str(uuid.uuid4())))
        with self.timed("annotation rollback"):
            boot = str(wait_for_new_boot(before.boot_id, ANNOTATION_REBOOT_TIMEOUT))
            wait_settled()
            committed = self.wait_status(operation_id, COMMIT_STATUS_KEY, ANNOTATION_REBOOT_TIMEOUT)
        rolled = self.wait_status(operation_id, STATUS_KEY, 60)
        after = self.probe_after_boot(boot, "after-annotation-rollback")
        c.expect(rolled.code == "Success", "N3", "trident-acl-agent rolls back as the annotation requests",
                 f"{rolled.code}: {rolled.message}")
        c.expect(committed.code == "Success", "N3", "trident-acl-agent commits the boot into the previous slot",
                 f"{committed.code}: {committed.message}")
        c.expect(after.pair != before.pair, "R1", "the VM booted the other slot", f"it booted {after.pair_text} again")
        if expected:
            c.expect(after.roothash == expected["roothash"], "R1",
                     f"/usr's root hash is {expected['image']}'s, which that slot held", f"it's {image_name(after.roothash)}")
        c.expect(after.state == "provisioned", "R1", "servicingState is provisioned", f"it's {after.state}")
        c.expect(after.last_error is None, "R1", "lastError is empty", after.last_error or "")
        self.check_slot_agrees("R1", after)
        self.check_boot_order("R1", after)
        self.check_persistence("R1", after)
        self.check_trident_agent(after)
        self.after_boot(after)
        self.release()
        self.state.committed_update = None
        self.state.staged = None
        self.learn(after)
        self.record("annotation-rollback", image=image_name(after.roothash))
        return after

    def annotation_rejects(self, target: Image) -> None:
        """Requests the agent must refuse, none of which may touch Trident (N4).

        Requests it can't parse come first: the contract says they get InvalidRequest, and v0.30.0
        logs and ignores them, as it has no operationId to answer. Either way they mustn't act, which
        is what's checked, and what the agent did is noted. Then requests that parse but break a
        rule, each with the code the contract gives it; the first also shows the agent still
        answers after the unparseable ones."""
        import omaha

        c = self.checks
        package = package_for(target)
        common = {"target_version": package.version, "app_id": ANNOTATION_APP_ID,
                  "track": annotation_track(package.version), "node_update_id": str(uuid.uuid4())}
        before = self.probe("before-annotation-rejects")
        with self.update_server(package) as server:
            common["server"] = server.update_url
            stage = self.annotation_request("stage", common)
            unparseable = [
                ("a request that isn't JSON", json.dumps(stage)[:60]),
                ("a request with a field the protocol doesn't have", json.dumps({**stage, "priority": "high"})),
                ("a request whose nodeUpdateId isn't a UUID", json.dumps({**stage, "nodeUpdateId": "node-1"})),
            ]
            for description, value in unparseable:
                self.check_unparseable(server, description, value)

            cases = [
                ("a stage whose update server isn't https", "InvalidRequest",
                 {**stage, "operationId": str(uuid.uuid4()), "server": f"http://{GUEST_HOST}:{OMAHA_PORT}/v1/update/"}),
                ("a request of another schemaVersion", "InvalidRequest",
                 {**stage, "operationId": str(uuid.uuid4()), "schemaVersion": "2.0"}),
                ("a stage with no track", "InvalidRequest",
                 {key: value for key, value in self.annotation_request("stage", common).items() if key != "track"}),
                ("a request whose operationId isn't a UUID", "InvalidRequest",
                 {**stage, "operationId": f"operation-{secrets.token_hex(4)}"}),
                ("a stage for a version the update server doesn't offer", "InvalidRequest",
                 self.annotation_request("stage", common, target_version=bump_patch(package.version))),
                ("a stage on a track the update server has nothing on", "OperationFailed",
                 self.annotation_request("stage", common, track=annotation_track("0.0.1"))),
                ("a finalize with nothing staged", "NotStaged",
                 self.annotation_request("finalize", common, node_update_id=str(uuid.uuid4()))),
                ("a rollback that names a target", "InvalidRequest",
                 {**omaha.update_request(operation="rollback", operation_id=str(uuid.uuid4()),
                                         node_update_id=str(uuid.uuid4())), "targetVersion": package.version}),
            ]
            for index, (description, expected, request) in enumerate(cases):
                self.annotate_raw(json.dumps(request), f"reject-{index + 1}")
                try:
                    status = self.wait_status(request["operationId"], STATUS_KEY, 120)
                except StepError as error:
                    c.expect(False, "N4", f"trident-acl-agent answers {description}", str(error))
                    continue
                c.expect(status.code == expected, "N4", f"trident-acl-agent answers {description} with {expected}",
                         f"it answered {status.code}: {status.message}")
        after = self.probe("after-annotation-rejects")
        c.expect(after.boot_id == before.boot_id, "N4", "the VM didn't reboot")
        c.expect((after.state, after.active_volume, after.pair) == (before.state, before.active_volume, before.pair),
                 "N4", "Trident's servicing state and slot are unchanged",
                 f"{before.state}/{before.active_volume} -> {after.state}/{after.active_volume}")
        self.cluster.kubectl("patch", "node", NODE_NAME, "--type", "merge", "-p",
                             json.dumps(omaha.clear_patch(annotation_prefix())))

    def check_unparseable(self, server: Any, description: str, value: str) -> None:
        """A request the agent can't parse doesn't act: no status for it, no update server request,
        no change to Trident. Notes what the agent did, against the contract's InvalidRequest."""
        c = self.checks
        before = self.probe()
        status = self.node_status(STATUS_KEY)
        since = ssh("date +%s", check=False).stdout.strip() or "0"
        first = len(server.requests)
        self.annotate_raw(value, "unparseable")
        time.sleep(ANNOTATION_SETTLE_SECONDS)
        after = self.probe()
        now = self.node_status(STATUS_KEY)
        made = server.requests[first:]
        c.expect(not made and (after.boot_id, after.state) == (before.boot_id, before.state), "N4",
                 f"trident-acl-agent doesn't act on {description}",
                 f"Trident went {before.state} -> {after.state}; the update server got {len(made)} request(s)")
        logged = ssh(f"sudo journalctl -u {TRIDENT_AGENT_UNIT} --since @{since} -o cat --no-pager | "
                     "grep -a -c 'ignoring malformed'", check=False).stdout.strip()
        if now is not None and (status is None or now.raw != status.raw):
            answer = f"answered {now.code}: {now.message}"
        else:
            answer = "wrote no status"
        ignored = "logged" if logged not in ("", "0") else "didn't log"
        self.note(f"for {description}, trident-acl-agent {answer}, and {ignored} that it ignored it; the contract "
                  "says InvalidRequest (Trident-ACL-Agent docs, The annotation contract)")

    def note(self, message: str) -> None:
        """Something a run found that isn't a check's pass or fail, for the summary."""
        self.notes.append(message)
        log(f"note: {message}")

    def check_update_result(self, after: Probe, *, url: str) -> None:
        """The checks after an update's boot and commit, for an update the agent drove: as
        finalize's A2 and A3, and commit's A4, then the node's."""
        staged = self.state.staged or {}
        c = self.checks
        c.expect(after.pair != tuple(staged["fromPair"]), "A2", "the VM booted the other slot",
                 f"it booted {after.pair_text} again")
        c.expect(after.roothash == staged["roothash"], "A2", f"/usr's verity root hash is {staged['image']}'s",
                 f"{short(after.roothash)} != {short(staged['roothash'])}")
        self.learn(after, staged["image"])
        self.check_persistence("A3", after)
        c.expect(after.state == "provisioned", "A4", "servicingState is provisioned", f"it's {after.state}")
        c.expect(after.last_error is None, "A4", "lastError is empty", after.last_error or "")
        if staged["fromVolume"]:
            c.expect(after.active_volume not in (None, staged["fromVolume"]), "A4", "abActiveVolume flipped",
                     f"{staged['fromVolume']} -> {after.active_volume}")
        c.expect(after.image_url == url, "A4", "the Host Status names the image URL the update server gave",
                 f"it names {after.image_url}")
        c.expect((after.image_sha384 or "").lower() == staged["sha384"], "A4",
                 "the Host Status names the update's metadata sha384", f"it names {after.image_sha384}")
        self.check_slot_agrees("A4", after)
        self.check_boot_order("A4", after)
        self.check_trident_agent(after)
        self.after_boot(after)
        self.state.committed_update = {"image": staged["image"], "fromPair": staged["fromPair"],
                                       "fromRoothash": staged["fromRoothash"], "toPair": list(after.pair)}
        self.state.staged = None
        self.state.save()
        self.release()

    # Reporting

    def collect_diagnostics(self, label: str) -> None:
        directory = self.run_dir / f"diagnostics-{label}"
        directory.mkdir(exist_ok=True)
        try:
            if SERIAL_LOG.exists():
                with SERIAL_LOG.open("rb") as serial:
                    serial.seek(max(0, SERIAL_LOG.stat().st_size - 4 * 2**20))
                    (directory / "serial.log").write_bytes(serial.read())
            if self.state.cluster:
                cluster_dir = directory / "cluster"
                cluster_dir.mkdir(exist_ok=True)
                self.cluster.diagnostics(NODE_NAME, cluster_dir)
            if vm_pid() is None or boot_id() is None:
                log(f"the VM isn't reachable; its serial log is in {directory}")
                return
            commands = {
                "journal.txt": "sudo journalctl -b --no-pager -n 5000",
                "journal-previous-boot.txt": "sudo journalctl -b -1 --no-pager -n 5000",
                "trident-status.yaml": "sudo trident get status 2>&1",
                "trident-last-error.yaml": "sudo trident get last-error 2>&1",
                "trident-units.txt": "systemctl status 'trident*' --all --no-pager -n 50",
                "trident-acl-agent.txt": f"systemctl cat {TRIDENT_AGENT_UNIT} --no-pager 2>&1; "
                                         f"sudo journalctl -b -u {TRIDENT_AGENT_UNIT} --no-pager -n 500",
                "failed-units.txt": "systemctl --failed --no-pager",
                "lsblk.txt": "lsblk -o NAME,SIZE,TYPE,FSTYPE,LABEL,PARTLABEL,MOUNTPOINTS",
                "efibootmgr.txt": "sudo efibootmgr -v 2>&1",
            }
            if self.state.cluster:
                commands.update({
                    "flex-units.txt": "systemctl status 'aks-flex-node*' --all --no-pager -n 0 2>&1",
                    "flex-bootstrap.txt": "sudo journalctl -b -u aks-flex-node-bootstrap.service --no-pager -n 500",
                    "flex-agent.txt": "sudo journalctl -b -u aks-flex-node-agent.service --no-pager -n 1000",
                    "flex-containers.txt": "sudo ctr --namespace aks-flex task ls 2>&1; "
                                           "sudo ctr --namespace aks-flex images ls 2>&1; machinectl list --all 2>&1",
                    "flex-network.txt": "ip -br addr; ip route; networkctl list --no-pager",
                })
            for name, command in commands.items():
                result = ssh(command, check=False, timeout=180)
                (directory / name).write_text(result.stdout + (f"\n## stderr\n{result.stderr}" if result.stderr.strip() else ""))
            bundle = f"{GUEST_DIR}/trident-diagnose.tar.zst"
            made = ssh(f"sudo mkdir -p {shlex.quote(GUEST_DIR)} && sudo rm -f {bundle} && "
                       f"sudo trident diagnose --output {bundle} --journal >/dev/null 2>&1 && sudo chmod a+r {bundle}",
                       check=False, timeout=600)
            if made.returncode == 0:
                copy_from_vm(bundle, directory / "trident-diagnose.tar.zst")
            log(f"diagnostics are in {directory}")
        except (StepError, OSError, subprocess.SubprocessError) as error:
            warn(f"couldn't collect every diagnostic: {error}")

    def finish(self) -> int:
        results = self.checks.results
        counts = {kind: sum(1 for result in results if result["result"] == kind) for kind in ("pass", "fail", "skip")}
        ok = counts["fail"] == 0 and not self.errors and all(s["result"] != "fail" for s in self.scenarios)
        elapsed = time.monotonic() - self.started
        summary = {
            "command": self.command, "ok": ok, "runDir": str(self.run_dir), "seconds": round(elapsed, 1),
            "counts": counts, "errors": self.errors, "notes": self.notes, "scenarios": self.scenarios,
            "checks": results,
            "timings": [{"name": name, "seconds": round(seconds, 1)} for name, seconds in self.timings],
        }
        (self.run_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        for scenario in self.scenarios:
            log(f"{scenario['result'].upper():4} {scenario['name']} ({duration(scenario['seconds'])})"
                + (f": {scenario['reason']}" if scenario["reason"] else ""))
        for note in self.notes:
            log(f"note: {note}")
        for error in self.errors:
            log(f"error: {error}")
        log(f"{counts['pass']} passed, {counts['fail']} failed, {counts['skip']} skipped in {duration(elapsed)}; "
            f"{self.run_dir / 'summary.json'}")
        if _json_output:
            print(json.dumps(summary, indent=2))
        return 0 if ok else 1


# ---------------------------------------------------------------------------------------------
# Install


def install_dir(tag: str) -> Path:
    return INSTALLED / (f"local-{Path(BASE_ISO).name}" if BASE_ISO else tag)


def install_needed(installed: Path, fresh: bool) -> str:
    """Why up has to run the installer, or "" if the cached install will do."""
    if fresh:
        return "--fresh-install was given"
    try:
        info = json.loads((installed / "info.json").read_text())
    except (OSError, json.JSONDecodeError):
        return f"there's no installed base in {installed}"
    if info.get("seedDigest") != seed_digest():
        return ("the Ignition seed changed since the base was installed, and the image reads it on its "
                "first boot only")
    return ""


def ensure_ssh_key() -> str:
    SSH_KEY.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if not SSH_KEY.exists():
        run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", "acl-abupdate", "-f", str(SSH_KEY)])
    return SSH_KEY.with_suffix(".pub").read_text().strip()


def wait_for_install(timeout: float) -> None:
    """Wait for the installed image's first boot to finish provisioning: the installer streams
    the base COSI to the disk and reboots, and the disk's first boot reads the seed."""
    started = reported = time.monotonic()
    offset, tail = 0, ""
    waiting_since: float | None = None
    while True:
        if vm_pid() is None:
            raise StepError(f"QEMU exited during the install; see {SERIAL_LOG}")
        text, offset = read_serial(offset)
        tail = (tail + text)[-8192:]
        if "WAITING FOR CONFIGURATION MEDIA" in text and waiting_since is None:
            waiting_since = time.monotonic()
            log(f"the installed image is waiting for its configuration media, the {SEED_LABEL} volume")
        if waiting_since is not None and time.monotonic() - waiting_since > 300:
            raise StepError(f"the installed image waited five minutes for a volume labelled {SEED_LABEL}; see {SERIAL_LOG}")
        if boot_id():
            return
        now = time.monotonic()
        if now - started > timeout:
            raise StepError(f"the install didn't finish in {duration(timeout)}; see {SERIAL_LOG}")
        if now - reported >= 60:
            reported = now
            log(f"installing, {duration(now - started)}: {last_serial_line(tail)}")
        time.sleep(10)


def install_base(state: State, iso: Image, destination: Path) -> None:
    """Install the base image from its ISO on a new disk, with the seed attached for its first
    boot, then power off. The disk, UEFI variables, and SSH key are kept in the cache, and each
    up starts from an overlay on them."""
    partial = destination.with_name(destination.name + ".partial")
    shutil.rmtree(partial, ignore_errors=True)
    partial.mkdir(parents=True)
    seed = build_seed(partial, ensure_ssh_key())
    disk, variables = partial / "disk.qcow2", partial / "vars.qcow2"
    run(["qemu-img", "create", "-q", "-f", "qcow2", str(disk), DISK_SIZE], capture=True)
    _, template = ovmf_firmware()
    run(["qemu-img", "convert", "-f", "raw", "-O", "qcow2", str(template), str(variables)], capture=True)
    log(f"installing {iso.name} on a new {DISK_SIZE} disk; `{PROG} console -f` follows it")
    started = time.monotonic()
    launch_vm(disk=disk, variables=variables, memory=state.memory, cpus=state.cpus, ssh_port=state.ssh_port,
              installer=iso.path, seed=seed)
    try:
        wait_for_install(INSTALL_TIMEOUT)
        installed = probe()
        if not installed.data_device.startswith("/dev/vd"):
            raise StepError(f"SSH reached a system whose /usr isn't on the VM's disk "
                            f"({installed.pair_text}); see {SERIAL_LOG}")
        log(f"installed in {duration(time.monotonic() - started)}: /usr on {installed.pair_text}; "
            "powering off to detach the installer and seed")
        # A disk that stopped booting would fall through to the installer, which would install the
        # base again and look like a rollback. So later boots have only the disk attached.
        stop_vm()
    except BaseException:
        stop_vm(hard=True)
        raise
    shutil.copytree(SSH_KEY.parent, partial / "ssh")
    seed.unlink()
    shutil.rmtree(partial / "seed")
    (partial / "info.json").write_text(json.dumps({
        "iso": iso.name, "isoInfo": iso.info, "installed": now_iso(), "diskSize": DISK_SIZE,
        "roothash": installed.roothash, "seedDigest": seed_digest(),
    }, indent=2) + "\n")
    shutil.rmtree(destination, ignore_errors=True)
    partial.rename(destination)


def prepare_overlay(installed: Path) -> None:
    SSH_KEY.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    for name in ("id_ed25519", "id_ed25519.pub"):
        shutil.copyfile(installed / "ssh" / name, SSH_KEY.parent / name)
    SSH_KEY.chmod(0o600)
    VM_DISK.unlink(missing_ok=True)
    run(["qemu-img", "create", "-q", "-f", "qcow2", "-b", str((installed / "disk.qcow2").resolve()),
         "-F", "qcow2", str(VM_DISK)], capture=True)
    shutil.copyfile(installed / "vars.qcow2", VM_VARS)


def baseline(ctx: Context) -> None:
    """B1: the VM runs the installed base from its disk."""
    current = ctx.probe("baseline")
    c = ctx.checks
    c.expect(current.data_device.startswith("/dev/vd"), "B1", "the VM runs from its disk",
             f"/usr's data device is {current.data_device or 'not a verity device'}")
    c.expect(bool(current.roothash), "B1", "/usr is a dm-verity device with a root hash")
    c.expect(not current.usr_writable, "B1", "/usr is read-only")
    name = ""
    base = None
    try:
        base = cosi_if_present(ctx.state.base_tag, ctx.state)
    except StepError as error:
        warn(f"can't read the base COSI: {error}")
    if base is None:
        log(f"the {ctx.state.base_tag} COSI isn't cached, so /usr's root hash isn't compared with it; "
            "`image fetch` pulls it")
    elif current.roothash == base.roothash:
        name = base.name
        log(f"/usr's root hash is the base COSI's ({base.name})")
    else:
        warn(f"/usr's root hash {short(current.roothash)} isn't the {base.name} COSI's "
             f"({short(base.roothash)}); the installer ISO may embed another build")
    status = f"servicingState {current.state}" if current.state else (
        f"no Host Status yet ({first_line(current.status_error) or 'no output'}); the first update initializes it")
    log(f"{current.os_release.get('PRETTY_NAME', 'unknown OS')}, kernel {current.kernel}, "
        f"Trident {current.trident_version or '?'}; {status}")
    ctx.check_trident_agent(current)
    ctx.learn(current, name or f"installed from {ctx.state.base_tag}")


# ---------------------------------------------------------------------------------------------
# Commands


def image_fetch(args: argparse.Namespace) -> None:
    state = State.load() if STATE_FILE.is_file() else None
    if state:
        use_images(state)
    base, update = (state.base_tag, state.update_tag) if state else (BASE_TAG, UPDATE_TAG)
    if not args.tags and not (base and update):
        die(missing_settings(["ACL_AB_BASE_TAG" if not base else "", "ACL_AB_UPDATE_TAG" if not update else ""]))
    tags = args.tags or [base, update]
    if not args.tags:
        iso_tags = [base]
    else:
        iso_tags = args.tags if args.iso else []
    for tag in iso_tags:
        log(base_iso(tag, refresh=args.refresh).describe())
    for tag in tags:
        log(cosi(tag, state, refresh=args.refresh).describe())


def image_list(_: argparse.Namespace) -> None:
    if STATE_FILE.is_file():
        use_images(State.load())
    rows = []
    for kind, repo in (("iso", ISO_REPO), ("cosi", COSI_REPO)):
        for directory in sorted((IMAGES / repo).iterdir()) if repo and (IMAGES / repo).is_dir() else []:
            image = cached_image(directory, kind)
            if image:
                rows.append({"kind": kind, "tag": image.name, "build": image.build, "os": image.version,
                             "roothash": image.roothash, "metadataSha384": image.sha384,
                             "size": int(image.info.get("size", 0)), "path": str(image.path)})
    builds = [{"name": image.name, "from": image.built.get("from", {}).get("name", ""),
               "version": image.built.get("version", ""), "created": image.built.get("created", ""),
               "files": image.built.get("files", []), "config": image.built.get("config"),
               "roothash": image.roothash, "metadataSha384": image.sha384,
               "size": int(image.info.get("size", 0)), "path": str(image.path)} for image in known_builds()]
    installed = [{"tag": path.name, **json.loads((path / "info.json").read_text())}
                 for path in sorted(INSTALLED.glob("*")) if (path / "info.json").is_file()]
    if _json_output:
        print(json.dumps({"images": rows, "builds": builds, "installed": installed}, indent=2))
        return
    if not rows and not builds:
        log(f"no images in {IMAGES}; `image fetch` pulls them")
    for row in rows:
        print(f"{row['kind']:5} {row['tag']:14} build {row['build'] or '-':10} OS {row['os'] or '-':22} "
              f"root hash {short(row['roothash']):12} sha384 {short(row['metadataSha384']):12} {human_size(row['size'])}")
    for row in builds:
        print(f"build {row['name']:14} from {row['from'] or '-':10} version {row['version'] or '-':30} "
              f"root hash {short(row['roothash']):12} sha384 {short(row['metadataSha384']):12} {human_size(row['size'])}")
    for entry in installed:
        print(f"installed base {entry['tag']} from {entry.get('iso', '?')}, {entry.get('installed', '?')}")


def check_build_prerequisites() -> None:
    missing = [tool for tool in ("podman", "zstd", "mdir", "mtype", "mcopy") if not shutil.which(tool)]
    if missing:
        die(f"image build needs {', '.join(missing)}; mdir, mtype, and mcopy are in mtools")
    if run(["sudo", "-n", "true"], check=False, capture=True).returncode != 0:
        die("image build needs sudo without a password prompt, for Image Customizer's privileged container; "
            "run `sudo -v` first if your sudo caches credentials")


def build_source(spec: str, state: State | None) -> Image:
    """The image a build starts from: a target as `update --to` takes one, but not `other`. Without
    a VM, base and update are the tags the settings give."""
    if spec == "other":
        raise StepError("--from takes base, update, a build, a tag, or a .cosi file")
    if state is None and spec in ("base", "update"):
        tag, variable = (BASE_TAG, "ACL_AB_BASE_TAG") if spec == "base" else (UPDATE_TAG, "ACL_AB_UPDATE_TAG")
        if not tag:
            raise StepError(missing_settings([variable]))
    return resolve_target(spec, state or State(), lambda: "")


def build_name_problem(name: str) -> str:
    if name in ("base", "update", "other") or not BUILD_NAME_PATTERN.fullmatch(name):
        return (f"{name!r} can't name a build: use up to 40 lowercase letters, digits, and hyphens, starting with "
                "a letter, other than base, update, and other")
    return ""


def image_build(args: argparse.Namespace) -> None:
    import omaha

    name = args.name
    if build_name_problem(name):
        die(build_name_problem(name))
    if not args.files and not args.config:
        die("give --files DIR, --config FILE, or both")
    if args.version and not omaha.is_semver(args.version):
        die(f"--version {args.version!r} isn't a semver version, which the update server offers builds as")
    files_dir = Path(args.files).expanduser().resolve() if args.files else None
    config = Path(args.config).expanduser().resolve() if args.config else None
    if config and not config.is_file():
        die(f"--config {config} isn't a file")
    files = cosibuild.collect_files(files_dir) if files_dir else []
    check_build_prerequisites()
    state = State.load() if STATE_FILE.is_file() else None
    if state:
        use_images(state)
    source = build_source(args.source, state)
    previous = build_image(name)
    version, count = build_version(name, source, previous)
    version = args.version or version
    os_version = build_os_version(name, source, count)

    BUILDS.mkdir(parents=True, exist_ok=True)
    partial = BUILDS / f"{name}.partial"
    shutil.rmtree(partial, ignore_errors=True)
    partial.mkdir()
    adding = []
    if files:
        adding.append(f"{len(files)} file{'s' if len(files) != 1 else ''} from {files_dir}")
    if config:
        adding.append(f"the config {config}")
    log(f"building {name} from {source.name} (/usr root hash {short(source.roothash)}), adding {' and '.join(adding)}")
    started = time.monotonic()
    request = cosibuild.Request(source=source.path, files_dir=files_dir, files=files, config=config,
                                image_customizer=IMAGE_CUSTOMIZER, work=BUILDS / f"{name}.work",
                                output=partial / f"{name}.cosi", keep_work=args.keep_work, os_version=os_version)
    try:
        result = cosibuild.build(request, log)
    except StepError as error:
        raise StepError(f"{error}\nThe build's config and Image Customizer's log are in {partial}") from error
    if config:
        shutil.copy2(config, partial / f"config-{config.name}")
    info = describe_file(request.output, "cosi")
    info["built"] = {"from": {"target": args.source, "name": source.name, "roothash": source.roothash,
                              "path": str(source.path)},
                     "version": version, "count": count, "created": now_iso(),
                     "filesDir": str(files_dir) if files_dir else None, "config": str(config) if config else None,
                     **result}
    (partial / "info.json").write_text(json.dumps(info, indent=2) + "\n")
    shutil.rmtree(BUILDS / name, ignore_errors=True)
    partial.rename(BUILDS / name)
    image = build_image(name)
    if image is None:
        raise StepError(f"the build left no image in {BUILDS / name}")
    if previous and previous.roothash != image.roothash:
        log(f"replaced the previous {name}, whose /usr root hash was {short(previous.roothash)}")
    log(f"built {image.describe()}, in {duration(time.monotonic() - started)}")
    log(f"update to it with `{PROG} update --to {name}`")
    if _json_output:
        print(json.dumps(info, indent=2))


def image_delete(args: argparse.Namespace) -> None:
    name = args.name
    directories = [BUILDS / name, BUILDS / f"{name}.partial", BUILDS / f"{name}.work"]
    if BUILD_NAME_PATTERN.fullmatch(name) and any(directory.exists() for directory in directories):
        for directory in directories:
            cosibuild.remove_work(directory)
        log(f"deleted build {name}")
        return
    if STATE_FILE.is_file():
        use_images(State.load())
    cached = [IMAGES / repo / name for repo in dict.fromkeys((ISO_REPO, COSI_REPO))
              if repo and TAG_PATTERN.fullmatch(name) and (IMAGES / repo / name).is_dir()]
    if not cached:
        die(f"there's no build or cached tag {name!r}; `image list` shows them")
    for directory in cached:
        shutil.rmtree(directory)
        log(f"deleted {directory}")


def cmd_image(args: argparse.Namespace) -> None:
    {"fetch": image_fetch, "list": image_list, "build": image_build, "delete": image_delete}[args.action](args)


def check_cluster_prerequisites() -> None:
    missing = [tool for tool in ("podman", "kind", "kubectl", "go", "ip", "iptables", "nsenter") if not shutil.which(tool)]
    if missing:
        die(f"--cluster needs {', '.join(missing)}")
    if run(["sudo", "-n", "true"], check=False, capture=True).returncode != 0:
        die("--cluster needs sudo without a password prompt, for kind on root Podman and the cluster's bridge; "
            "run `sudo -v` first if your sudo caches credentials")


def create_cluster(cluster: Any) -> None:
    cluster.create()
    cluster.build_and_deploy_controller()


def up_settings_missing(base_tag: str, update_tag: str) -> list[str]:
    """The image settings up needs that aren't set: the tags always, as they name the images; the
    registry and the repositories unless local files stand in for what they'd give."""
    missing = ["ACL_AB_BASE_TAG" if not base_tag else "", "ACL_AB_UPDATE_TAG" if not update_tag else ""]
    needs_iso = not BASE_ISO
    needs_cosi = not (BASE_COSI and UPDATE_COSI)
    if (needs_iso or needs_cosi) and not REGISTRY_PATH:
        missing.append("ACL_AB_REGISTRY")
    if needs_iso and not ISO_REPO:
        missing.append("ACL_AB_ISO_REPO")
    if needs_cosi and not COSI_REPO:
        missing.append("ACL_AB_COSI_REPO")
    return [name for name in missing if name]


def cmd_up(args: argparse.Namespace) -> None:
    check_vm_prerequisites()
    missing = up_settings_missing(args.base_tag, args.update_tag)
    if missing:
        die(missing_settings(missing))
    if args.cluster:
        check_cluster_prerequisites()
    claim_state_dir()
    with state_lock():
        if STATE_FILE.exists() or VM_DISK.exists():
            die(f"{STATE} has a VM already; `start` it, or `down` it first")
        state = State(base_tag=args.base_tag, update_tag=args.update_tag, ssh_port=args.ssh_port,
                      memory=args.memory, cpus=args.cpus, smbios_uuid=str(uuid.uuid4()),
                      registry=REGISTRY_PATH, iso_repo=ISO_REPO, cosi_repo=COSI_REPO)
        use_port(state.ssh_port)
        ctx = Context("up", state)
        try:
            cluster = None
            if args.cluster:
                import flexcluster

                spec = flexcluster.ClusterSpec(name=KIND_CLUSTER, subnet=CLUSTER_SUBNET, node_image=KIND_NODE_IMAGE,
                                               user=getpass.getuser())
                spec.validate()
                # Before anything of the cluster exists, so down can remove what a failed up leaves.
                CLUSTER_FILE.write_text(json.dumps(spec.to_dict(), indent=2) + "\n")
                state.cluster = spec.to_dict()
                cluster = ctx.cluster
                if cluster.exists():
                    raise StepError(f"kind cluster {spec.name} exists already; delete it, or set ACL_AB_KIND_CLUSTER")
            installed = install_dir(state.base_tag)
            reason = install_needed(installed, args.fresh_install)
            # The cluster and the controller build while the base installs: neither waits on the other.
            with concurrent.futures.ThreadPoolExecutor(1) as pool:
                building = pool.submit(create_cluster, cluster) if cluster else None
                if reason:
                    log(f"installing the base: {reason}")
                    install_base(state, base_iso(state.base_tag), installed)
                else:
                    log(f"starting from the base installed in {installed}; --fresh-install installs it again")
                if building:
                    building.result()
            prepare_overlay(installed)
            state.installed = str(installed)
            state.save()
            launch(state)
            wait_for_ssh(BOOT_TIMEOUT)
            wait_settled()
            baseline(ctx)
            summary = f"{state.base_tag}, freshly installed"
            if cluster:
                configure_cluster_nic(cluster.spec)
                save_snapshot(state, "prejoin", force=True, summary=f"{summary}, on the cluster's bridge, not joined")
                ctx.join()
                ctx.connect_trident_agent()
                ctx.trust_update_server()
                cluster.ensure_probe(NODE_NAME)
                ctx.expect_all("K3", cluster.probe_findings(NODE_NAME))
                summary = f"{state.base_tag}, joined to kind cluster {cluster.spec.name} as {NODE_NAME}"
            save_snapshot(state, "base", force=True, summary=summary)
        except StepError as error:
            log(f"up failed: {error}")
            ctx.errors.append(str(error))
            ctx.collect_diagnostics("up")
            ctx.finish()
            die(f"the serial log is {SERIAL_LOG}; `{PROG} down` removes what up made", 1)
        code = ctx.finish()
    log(f"the VM is up; try `{PROG} status`, `{PROG} test`, or `{PROG} update`")
    sys.exit(code)


def cmd_start(args: argparse.Namespace) -> None:
    with state_lock():
        state = require_state()
        if vm_pid() is not None:
            die("the VM is running already")
        cluster = cluster_of(state)
        if cluster is not None:
            # After a host restart, the bridge and TAP are gone and the kind node stopped.
            cluster.ensure()
        if args.snapshot:
            restore_snapshot(args.snapshot, state)
        else:
            launch(state)
            wait_for_ssh(BOOT_TIMEOUT)
            wait_settled()
            if cluster is not None and cluster.node(NODE_NAME):
                cluster.wait_ready(NODE_NAME, READY_TIMEOUT)
    log("the VM is running")


def cmd_stop(args: argparse.Namespace) -> None:
    with state_lock():
        require_state()
        stop_vm(hard=args.hard)
    log("the VM is stopped")


def remove_cluster(spec: dict[str, Any]) -> list[str]:
    """Delete the kind cluster, its network, and the local registry, as far as possible; returns
    what failed."""
    import flexcluster
    import flexinstall

    cluster = flexcluster.Cluster(flexcluster.ClusterSpec.from_dict(spec), kubeconfig=KUBECONFIG, repo=REPO,
                                  work_dir=FLEX_DIR, run=host_run, log=log)
    failures = cluster.delete()
    flexinstall.Registry(LOCAL_REGISTRY, LOCAL_REGISTRY_PORT, guest_host=GUEST_HOST, run=host_run, log=log).stop()
    return failures


def cmd_down(args: argparse.Namespace) -> None:
    failures: list[str] = []
    if STATE.is_dir():
        with state_lock():
            state = State.load() if STATE_FILE.is_file() else None
            if state:
                use_port(state.ssh_port)
            stop_vm(hard=True)
            spec = state.cluster if state and state.cluster else None
            if spec is None and CLUSTER_FILE.is_file():
                spec = json.loads(CLUSTER_FILE.read_text())
            if spec:
                failures = remove_cluster(spec)
    if failures and not args.force:
        die("couldn't remove all of the cluster:\n  " + "\n  ".join(failures) +
            f"\nRemove those by hand, then run down again, or `down --force` to remove {STATE} anyway", 1)
    if STATE.resolve() == DEFAULT_STATE.resolve() or STATE_MARKER.exists():
        shutil.rmtree(STATE, ignore_errors=True)
        log(f"removed the VM and {STATE}")
    else:
        log(f"stopped the VM; not removing {STATE}, which up didn't make")
    if CACHE.is_dir():
        log(f"kept the downloads and installed bases in {CACHE}; remove it to reclaim the space")


def cmd_status(_: argparse.Namespace) -> None:
    if not STATE_FILE.is_file():
        if _json_output:
            print(json.dumps({"exists": False, "running": False}))
        log(f"there's no VM in {STATE}; run `up`")
        return
    state = require_state()
    pid = vm_pid()
    info: dict[str, Any] = {
        "exists": True, "running": pid is not None, "pid": pid,
        "ssh": f"ssh -p {state.ssh_port} {SSH_USER}@127.0.0.1",
        "baseTag": state.base_tag, "updateTag": state.update_tag, "snapshots": snapshot_names(),
        "staged": state.staged, "committedUpdate": state.committed_update, "markers": len(state.markers),
    }
    current = None
    if pid is not None:
        try:
            current = probe()
        except StepError as error:
            info["error"] = str(error)
    if current:
        agent = current.trident_agent
        info.update({
            "image": image_name(current.roothash), "roothash": current.roothash, "slot": list(current.pair),
            "servicingState": current.state, "abActiveVolume": current.active_volume,
            "lastError": current.last_error, "statusError": current.status_error or None,
            "imageUrl": current.image_url, "bootCurrent": current.boot_current, "bootNext": current.boot_next,
            "bootOrder": current.boot_order, "os": current.os_release.get("PRETTY_NAME"),
            "kernel": current.kernel, "trident": current.trident_version,
            "tridentAclAgent": {
                "loadState": agent.get("LoadState"), "enabled": agent.get("is-enabled"),
                "activeState": agent.get("ActiveState"), "subState": agent.get("SubState"),
                "restarts": agent.get("NRestarts"), "dropInPaths": agent.get("DropInPaths", "").split(),
                "tridentSocket": agent.get("socket"), "preflight": current.trident_agent_preflight,
            },
        })
    cluster = cluster_of(state)
    if cluster is not None:
        try:
            node = node_summary(cluster.node(NODE_NAME), annotation_prefix())
        except StepError as error:
            node = {"registered": False, "error": str(error)}
        info["cluster"] = {"name": cluster.spec.name, "kubeconfig": str(KUBECONFIG), "node": node,
                           "agentImage": state.agent_image}
    if _json_output:
        print(json.dumps(info, indent=2))
        return
    print(f"VM        {'running, pid ' + str(pid) if pid else 'stopped'}; {info['ssh']} (key {SSH_KEY})")
    print(f"Images    base {state.base_tag}, update {state.update_tag}")
    if current:
        print(f"Booted    {info['image']} on {current.pair_text}")
        print(f"Trident   {current.state or 'no Host Status: ' + first_line(current.status_error)}"
              f", abActiveVolume {current.active_volume or '-'}, lastError {'set' if current.last_error else 'none'}")
        print(f"Image     {current.image_url or '-'}")
        print(f"UEFI      BootCurrent {current.boot_label(current.boot_current) if current.boot_current else '-'}, "
              f"BootNext {current.boot_next or '-'}, BootOrder {', '.join(current.boot_order) or '-'}")
        print(f"OS        {info['os'] or '-'}, kernel {current.kernel or '-'}, Trident {current.trident_version or '-'}")
        agent = current.trident_agent
        if agent.get("LoadState") == "loaded":
            dropin = "with" if TRIDENT_AGENT_DROPIN_PATH in agent.get("DropInPaths", "").split() else "WITHOUT"
            print(f"Agent     {TRIDENT_AGENT_UNIT} {agent.get('is-enabled') or '?'}, "
                  f"{agent.get('ActiveState', '?')}/{agent.get('SubState', '?')}, {agent.get('NRestarts', '?')} restarts, "
                  f"{dropin} the harness's drop-in; tridentd socket {agent.get('socket', '?')}")
        else:
            print(f"Agent     {TRIDENT_AGENT_UNIT} isn't installed ({agent.get('LoadState') or 'unknown'})")
    elif "error" in info:
        print(f"Probe     {info['error']}")
    print(f"Staged    {state.staged['image'] if state.staged else '-'}")
    print(f"Committed {state.committed_update['image'] if state.committed_update else '-'}")
    if "cluster" in info:
        node = info["cluster"]["node"]
        if node.get("registered"):
            print(f"Cluster   kind {info['cluster']['name']}; node {NODE_NAME} {'Ready' if node['ready'] else 'NOT Ready'}, "
                  f"{node['internalIP'] or 'no address'}, {'schedulable' if node['schedulable'] else 'cordoned'}, "
                  f"kubelet {node['kubelet'] or '?'}; agent image {state.agent_image or '-'}")
            for key, value in node["annotations"].items():
                print(f"          {key}: {value[:160]}")
        else:
            print(f"Cluster   kind {info['cluster']['name']}; node {NODE_NAME} {node.get('error') or 'not registered'}")
    print(f"Snapshots {', '.join(info['snapshots']) or '-'}")


def node_summary(node: dict[str, Any] | None, prefix: str) -> dict[str, Any]:
    """What status shows of the Node: whether it's Ready and schedulable, its address, its
    kubelet, and trident-acl-agent's annotations."""
    if not node:
        return {"registered": False}
    status = node.get("status") or {}
    conditions = {condition.get("type"): condition.get("status") for condition in status.get("conditions") or []}
    addresses = {address.get("type"): address.get("address") for address in status.get("addresses") or []}
    annotations = (node.get("metadata") or {}).get("annotations") or {}
    return {
        "registered": True, "ready": conditions.get("Ready") == "True", "internalIP": addresses.get("InternalIP"),
        "schedulable": not (node.get("spec") or {}).get("unschedulable"),
        "kubelet": (status.get("nodeInfo") or {}).get("kubeletVersion"),
        "annotations": {key: value for key, value in annotations.items() if key.startswith(f"{prefix}/")},
    }


def require_cluster() -> State:
    state = require_state()
    if not state.cluster:
        die("this VM has no cluster; `down`, then `up --cluster`")
    return state


def without_separator(command: list[str]) -> list[str]:
    return command[1:] if command[:1] == ["--"] else command


def cmd_kubectl(args: argparse.Namespace) -> None:
    require_cluster()
    os.execvp("kubectl", ["kubectl", "--kubeconfig", str(KUBECONFIG), *without_separator(args.command)])


def cmd_agent(args: argparse.Namespace) -> None:
    """Run a command in the agent's container on the VM, or open a shell there."""
    require_cluster()
    require_running()
    command = without_separator(args.command) or ["/bin/bash", "-l"]
    tty = sys.stdin.isatty() and sys.stdout.isatty()
    remote = shlex.join(["sudo", "ctr", "--namespace", "aks-flex", "task", "exec", *(["--tty"] if tty else []),
                         "--exec-id", f"acl-ab-{secrets.token_hex(4)}", "aks-flex-node", *command])
    base = ssh_command()
    os.execvp("ssh", [*base[:-1], *(["-t"] if tty else []), base[-1], remote])


def cmd_cluster(args: argparse.Namespace) -> None:
    if args.action == "env":
        require_cluster()
        print(f"export KUBECONFIG={shlex.quote(str(KUBECONFIG))}")
        return
    if args.action == "status":
        require_cluster()
        os.execvp("sh", ["sh", "-c", f"kubectl --kubeconfig {shlex.quote(str(KUBECONFIG))} get nodes -o wide && echo && "
                                     f"kubectl --kubeconfig {shlex.quote(str(KUBECONFIG))} get pods -A -o wide "
                                     f"--field-selector spec.nodeName={NODE_NAME}"])
    with state_lock():
        state = require_cluster()
        require_running()
        ctx = Context(f"cluster-{args.action}", state)
        try:
            if args.action == "rejoin":
                rejoin(ctx, from_prejoin=args.from_prejoin)
            else:
                push_agent(ctx)
        except StepError as error:
            ctx.errors.append(str(error))
            log(f"FAIL: {error}")
            ctx.collect_diagnostics(args.action)
        finally:
            ctx.state.save()
        code = ctx.finish()
    sys.exit(code)


def rejoin(ctx: Context, *, from_prejoin: bool) -> None:
    """Join the VM again with a new bootstrap token: from snapshot prejoin, or after the agent's
    reset, which removes the agent and the node and leaves the install, as hack/acl-container's
    DESIGN.md describes."""
    cluster = ctx.cluster
    if from_prejoin:
        ctx.state = restore_snapshot("prejoin", ctx.state)
    else:
        log("resetting the node: aks-flex-node-reset.service")
        result = ssh("sudo systemctl start aks-flex-node-reset.service", check=False, timeout=900)
        if result.returncode != 0:
            raise StepError(f"the reset failed: {last_lines(result.stderr)}")
        cluster.kubectl("delete", "node", NODE_NAME, "--ignore-not-found", check=False)
    ctx.join()
    ctx.connect_trident_agent()
    cluster.ensure_probe(NODE_NAME)
    ctx.expect_all("K3", cluster.probe_findings(NODE_NAME))


def push_agent(ctx: Context) -> None:
    """Build the agent's image again from this repository and switch the node to it, through
    /etc/aks-flex-node/agent-image.env, which selects the image the agent's units run, as an
    AgentUpgrade does."""
    import flexinstall

    flexinstall.build_agent_image(REPO, AGENT_IMAGE_TAG, run=host_run, log=log)
    registry = flexinstall.Registry(LOCAL_REGISTRY, LOCAL_REGISTRY_PORT, guest_host=GUEST_HOST, run=host_run, log=log)
    registry.start()
    image = registry.push(AGENT_IMAGE_TAG, AGENT_REPOSITORY, "acl-abupdate")
    log(f"switching the node to {image}")
    ssh("sudo install -D -m 0644 /dev/stdin /etc/aks-flex-node/agent-image.env && "
        f"sudo ctr --namespace aks-flex image pull --plain-http {shlex.quote(image)} >/dev/null && "
        "sudo systemctl restart aks-flex-node-agent.service", input=f"AKS_FLEX_NODE_IMAGE={image}\n", timeout=900)
    flexinstall.wait_for_agent(ssh, timeout=600, log=log)
    ctx.state.agent_image = image
    ctx.state.save()
    ctx.expect_all("K1", flexinstall.agent_findings(ssh))


def cmd_ssh(args: argparse.Namespace) -> None:
    require_running()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    base = ssh_command()
    os.execvp("ssh", [*base[:-1], *(["-t"] if not command else []), base[-1], *command])


def cmd_console(args: argparse.Namespace) -> None:
    if not SERIAL_LOG.exists():
        die(f"there's no serial log at {SERIAL_LOG}")
    os.execvp("tail", ["tail", "-n", "+1", *(["-f"] if args.follow else []), str(SERIAL_LOG)])


def cmd_snapshot(args: argparse.Namespace) -> None:
    if args.action == "list":
        require_state()
        names = snapshot_names()
        if _json_output:
            print(json.dumps([json.loads((SNAPSHOT_DIR / f"{name}.json").read_text()) | {"state": None}
                              for name in names], indent=2))
            return
        for name in names:
            meta = json.loads((SNAPSHOT_DIR / f"{name}.json").read_text())
            print(f"{name:20} {meta.get('created', '?')}  {meta.get('summary', '')}")
        if not names:
            log("no snapshots")
        return
    with state_lock():
        state = require_state()
        if args.action == "save":
            summary = ""
            if vm_pid() is not None:
                with contextlib.suppress(StepError):
                    current = probe()
                    summary = f"{image_name(current.roothash)} on {current.pair_text}, {current.state or 'no Host Status'}"
            save_snapshot(state, args.name, force=args.force, summary=summary)
        elif args.action == "restore":
            restore_snapshot(args.name, state)
            log(f"restored snapshot {args.name}")
        else:
            delete_snapshot(args.name)


def drive(command: str, action: Callable[[Context], None], *, single_step: bool = False,
          drain: bool = True) -> NoReturn:
    """Run a step command: the action's checks, and diagnostics if it fails."""
    with state_lock():
        state = require_running()
        ctx = Context(command, state)
        ctx.single_step = single_step
        ctx.drain = drain
        try:
            action(ctx)
        except Skip as skip:
            ctx.checks.skip(command, f"the {command} step", str(skip))
        except StepError as error:
            ctx.errors.append(str(error))
            log(f"FAIL: {error}")
            ctx.collect_diagnostics(command)
        finally:
            ctx.state.save()
        code = ctx.finish()
    sys.exit(code)


def cmd_update(args: argparse.Namespace) -> None:
    def action(ctx: Context) -> None:
        target = ctx.target(args.to)
        if args.failing_health_check:
            ctx.health_rollback(target)
        else:
            ctx.update(target, commit=not args.no_commit)

    drive("update", action, single_step=args.single_step, drain=not args.no_drain)


def cmd_stage(args: argparse.Namespace) -> None:
    drive("stage", lambda ctx: ctx.stage(ctx.target(args.to), health_check=args.failing_health_check),
          single_step=args.single_step)


def cmd_finalize(args: argparse.Namespace) -> None:
    drive("finalize", lambda ctx: ctx.finalize(), drain=not args.no_drain)


def cmd_commit(_: argparse.Namespace) -> None:
    drive("commit", lambda ctx: ctx.commit())


def cmd_rollback(args: argparse.Namespace) -> None:
    drive("rollback", lambda ctx: ctx.rollback(check_only=args.check), drain=not args.no_drain)


def cmd_reboot(args: argparse.Namespace) -> None:
    drive("reboot", lambda ctx: ctx.reboot(hard=args.hard), drain=not args.no_drain)


def cmd_verify(args: argparse.Namespace) -> None:
    drive("verify", lambda ctx: ctx.verify(expect_target=args.expect, expect_slot=args.expect_slot,
                                           expect_state=args.expect_state))


def cmd_scenarios(_: argparse.Namespace) -> None:
    if _json_output:
        print(json.dumps([{"name": s.name, "description": s.description, "needs": s.needs.value,
                           "checks": list(s.checks), "target": s.target, "experimental": s.experimental}
                          for s in REGISTRY.values()], indent=2))
        return
    for item in REGISTRY.values():
        flags = f"needs {item.needs.value}" + (", experimental" if item.experimental else "")
        print(f"{item.name:20} {', '.join(item.checks):34} {flags}")
        print(textwrap.indent(textwrap.fill(item.description, 76), " " * 21))


def run_scenarios(command: str, names: list[str], *, from_snapshot: str, rounds: int, target: str,
                  keep_going: bool, no_setup: bool, single_step: bool, drain: bool = True) -> NoReturn:
    if from_snapshot and not (SNAPSHOT_DIR / f"{from_snapshot}.json").is_file():
        die(f"there's no snapshot {from_snapshot}; `snapshot list` shows them")
    with state_lock():
        state = require_state()
        if vm_pid() is None and not from_snapshot:
            die("the VM isn't running; `start` it, or pass --from SNAPSHOT")
        ctx = Context(command, state)
        ctx.rounds, ctx.target_spec, ctx.no_setup, ctx.single_step = rounds, target, no_setup, single_step
        ctx.drain = drain
        for name in names:
            if from_snapshot:
                restored_at = datetime.datetime.now(datetime.timezone.utc)
                try:
                    ctx.state = restore_snapshot(from_snapshot, ctx.state)
                    with ctx.checks.scope(name):
                        ctx.check_restored(snapshot_record(from_snapshot), restored_at)
                except StepError as error:
                    ctx.errors.append(f"restoring {from_snapshot} for {name}: {error}")
                    break
            result = ctx.run_scenario(REGISTRY[name])
            ctx.state.save()
            if result == "fail" and not keep_going and name != names[-1]:
                log("stopping at the first failure; --keep-going runs the rest")
                break
        code = ctx.finish()
    sys.exit(code)


def cmd_run(args: argparse.Namespace) -> None:
    names = list(REGISTRY) if "all" in args.scenarios else list(dict.fromkeys(args.scenarios))
    run_scenarios("run", names, from_snapshot=args.from_snapshot, rounds=args.rounds, target=args.to,
                  keep_going=args.keep_going, no_setup=args.no_setup, single_step=args.single_step,
                  drain=not args.no_drain)


def cmd_test(args: argparse.Namespace) -> None:
    run_scenarios("test", list(REGISTRY), from_snapshot="base", rounds=args.rounds, target="",
                  keep_going=not args.stop_on_failure, no_setup=False, single_step=args.single_step,
                  drain=not args.no_drain)


def positive(value: str) -> int:
    if not value.isdigit() or int(value) < 1:
        raise argparse.ArgumentTypeError(f"{value!r} isn't a positive whole number")
    return int(value)


TARGET_HELP = "base, update, other (whichever of those the VM doesn't run), a build, a registry tag, or a .cosi file"
NO_DRAIN_HELP = "in cluster mode, don't cordon and drain the node around the reboot"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog=PROG, description=__doc__.split("\n\n", 1)[0],
                                     epilog="See hack/acl-abupdate/README.md.")
    parser.add_argument("--json", action="store_true",
                        help="print the result as JSON on stdout, and the log on stderr")
    commands = parser.add_subparsers(dest="command", required=True, metavar="COMMAND")

    def command(name: str, func: Callable[[argparse.Namespace], None], summary: str) -> argparse.ArgumentParser:
        sub = commands.add_parser(name, help=summary, description=summary)
        sub.set_defaults(func=func)
        return sub

    image = command("image", cmd_image, "fetch, list, build, or delete images")
    image_actions = image.add_subparsers(dest="action", required=True, metavar="ACTION")
    fetch = image_actions.add_parser("fetch", help="pull the base tag's installer ISO and the base and update COSIs, "
                                                   "or the COSIs of the tags given, into the cache")
    fetch.add_argument("tags", nargs="*", metavar="TAG")
    fetch.add_argument("--iso", action="store_true", help="pull the given tags' installer ISOs too")
    fetch.add_argument("--refresh", action="store_true", help="pull again what the cache has")
    image_actions.add_parser("list", help="list the cached images, builds, and installed bases")
    build = image_actions.add_parser("build", help="build a COSI from a base and your files or Image Customizer "
                                                   "config, to update to; see README.md")
    build.add_argument("name", metavar="NAME", help="the build's name, which targets take")
    build.add_argument("--files", metavar="DIR", help="files to add, laid out like the root: usr/... and etc/...")
    build.add_argument("--config", metavar="FILE", help="an Image Customizer config to apply too")
    build.add_argument("--from", dest="source", default="base", metavar="TARGET",
                       help="the image to build on: base, update, a build, a tag, or a .cosi file (default base)")
    build.add_argument("--version", default="", help="the semver version the update server offers it as "
                                                     "(default: its source's, with -NAME.N)")
    build.add_argument("--keep-work", action="store_true", help="keep the work directory, with its raw disks")
    image_actions.add_parser("delete", help="delete a build, or a tag's cached images").add_argument("name")

    up = command("up", cmd_up, "install the base image in a new VM, or start from the base installed "
                               "earlier, check it (B1), and save snapshot base")
    up.add_argument("--base-tag", default=BASE_TAG, metavar="TAG", help="image installed (or ACL_AB_BASE_TAG)")
    up.add_argument("--update-tag", default=UPDATE_TAG, metavar="TAG", help="target `update` (or ACL_AB_UPDATE_TAG)")
    up.add_argument("--fresh-install", action="store_true", help="install from the ISO even if the cache has the base")
    up.add_argument("--memory", type=positive, default=VM_MEMORY, metavar="MIB", help=f"VM memory (default {VM_MEMORY})")
    up.add_argument("--cpus", type=positive, default=VM_CPUS, metavar="N", help=f"VM CPUs (default {VM_CPUS})")
    up.add_argument("--ssh-port", type=positive, default=SSH_PORT, metavar="PORT",
                    help=f"host port forwarded to the VM's SSH (default {SSH_PORT})")
    up.add_argument("--cluster", action="store_true",
                    help="also make a local kind cluster and join the VM to it as a flex node, with the agent "
                         "in containers (needs sudo); see README.md")
    start = command("start", cmd_start, "start the stopped VM")
    start.add_argument("--snapshot", metavar="NAME", help="start it from a snapshot")
    stop = command("stop", cmd_stop, "power the VM off; its disk and snapshots stay")
    stop.add_argument("--hard", action="store_true", help="stop QEMU at once, as pulling the power would")
    down = command("down", cmd_down, "stop the VM and remove its state, and its cluster; the cache stays")
    down.add_argument("--force", action="store_true", help="remove the state even if the cluster can't all be removed")
    command("status", cmd_status, "show the VM, its booted slot and image, Trident's state, and the UEFI boot variables")
    ssh_parser = command("ssh", cmd_ssh, "open a shell on the VM, or run a command there")
    ssh_parser.add_argument("command", nargs=argparse.REMAINDER)
    console = command("console", cmd_console, "show the VM's serial console log")
    console.add_argument("-f", "--follow", action="store_true", help="keep following it")
    kubectl_parser = command("kubectl", cmd_kubectl, "run kubectl against the cluster")
    kubectl_parser.add_argument("command", nargs=argparse.REMAINDER)
    agent_parser = command("agent", cmd_agent, "open a shell in the agent's container on the VM, or run a command there")
    agent_parser.add_argument("command", nargs=argparse.REMAINDER)
    cluster_parser = command("cluster", cmd_cluster, "the cluster: its status, kubeconfig, rejoining, and agent image")
    cluster_actions = cluster_parser.add_subparsers(dest="action", required=True, metavar="ACTION")
    cluster_actions.add_parser("status", help="the nodes, and the pods on the VM's node")
    cluster_actions.add_parser("env", help="print the export that points kubectl at the cluster")
    rejoin_parser = cluster_actions.add_parser("rejoin", help="reset the node and join it again, with a new token")
    rejoin_parser.add_argument("--from-prejoin", action="store_true",
                               help="restore snapshot prejoin, from before the join, instead of resetting")
    cluster_actions.add_parser("push", help="rebuild the agent's image from this repository and switch the node to it")

    snapshot = command("snapshot", cmd_snapshot, "save, restore, list, or delete snapshots of the running VM")
    actions = snapshot.add_subparsers(dest="action", required=True, metavar="ACTION")
    save = actions.add_parser("save", help="save the running VM, memory included")
    save.add_argument("name")
    save.add_argument("--force", action="store_true", help="replace a snapshot of that name")
    actions.add_parser("restore", help="return the VM to a snapshot").add_argument("name")
    actions.add_parser("list", help="list the snapshots")
    actions.add_parser("delete", help="delete a snapshot").add_argument("name")

    update = command("update", cmd_update, "stage, finalize, and commit an A/B update, checking each (A1-A4)")
    update.add_argument("--to", default="other", metavar="TARGET", help=f"{TARGET_HELP} (default other)")
    update.add_argument("--no-commit", action="store_true", help="stop after the new slot boots")
    update.add_argument("--failing-health-check", action="store_true",
                        help="add a health check that fails, and check that commit rolls back (H1)")
    update.add_argument("--single-step", action="store_true", help="stage and finalize in one trident update")
    update.add_argument("--no-drain", action="store_true", help=NO_DRAIN_HELP)
    stage = command("stage", cmd_stage, "write an image to the inactive slot without rebooting (A1)")
    stage.add_argument("--to", default="other", metavar="TARGET", help=f"{TARGET_HELP} (default other)")
    stage.add_argument("--failing-health-check", action="store_true", help="add a health check that fails")
    stage.add_argument("--single-step", action="store_true",
                       help="only copy the COSI and write the config; finalize then stages and finalizes")
    finalize = command("finalize", cmd_finalize, "finalize the staged update, which reboots into the new slot (A2, A3)")
    finalize.add_argument("--no-drain", action="store_true", help=NO_DRAIN_HELP)
    command("commit", cmd_commit, "commit the update the VM booted (A4)")
    rollback = command("rollback", cmd_rollback, "roll the last A/B update back with trident rollback --ab (R1)")
    rollback.add_argument("--check", action="store_true", help="only report what trident rollback --check says")
    rollback.add_argument("--no-drain", action="store_true", help=NO_DRAIN_HELP)
    reboot = command("reboot", cmd_reboot, "reboot the VM, and report the slot it boots")
    reboot.add_argument("--hard", action="store_true", help="reset the VM rather than reboot it")
    reboot.add_argument("--no-drain", action="store_true", help=NO_DRAIN_HELP)
    verify = command("verify", cmd_verify, "check the VM's state without changing it (V1)")
    verify.add_argument("--expect", default="", metavar="TARGET", help=f"the image it should run: {TARGET_HELP}")
    verify.add_argument("--expect-slot", choices=("a", "b"), help="the volume Trident should report active")
    verify.add_argument("--expect-state", default="", metavar="STATE", help="the servicingState it should report")

    command("scenarios", cmd_scenarios, "list the scenarios")
    run_parser = command("run", cmd_run, "run scenarios, in the order given, on the running VM or from a snapshot")
    run_parser.add_argument("scenarios", nargs="+", metavar="SCENARIO", choices=[*REGISTRY, "all"],
                            help=f"{', '.join(REGISTRY)}, or all")
    run_parser.add_argument("--from", dest="from_snapshot", default="", metavar="SNAPSHOT",
                            help="restore this snapshot before each scenario")
    run_parser.add_argument("--rounds", type=positive, default=2, help="pingpong's updates (default 2)")
    run_parser.add_argument("--to", default="", metavar="TARGET", help=f"the scenarios' target: {TARGET_HELP}")
    run_parser.add_argument("--keep-going", action="store_true", help="run the rest after a scenario fails")
    run_parser.add_argument("--no-setup", action="store_true", help="fail a scenario the VM isn't set up for")
    run_parser.add_argument("--single-step", action="store_true", help="stage and finalize in one trident update")
    run_parser.add_argument("--no-drain", action="store_true", help=NO_DRAIN_HELP)
    test = command("test", cmd_test, "run every scenario, each from snapshot base")
    test.add_argument("--rounds", type=positive, default=2, help="pingpong's updates (default 2)")
    test.add_argument("--stop-on-failure", action="store_true", help="stop at the first scenario that fails")
    test.add_argument("--single-step", action="store_true", help="stage and finalize in one trident update")
    test.add_argument("--no-drain", action="store_true", help=NO_DRAIN_HELP)
    return parser


def main() -> None:
    global _json_output
    args = build_parser().parse_args()
    _json_output = args.json
    try:
        args.func(args)
    except StepError as error:
        die(str(error), 1)
    except KeyboardInterrupt:
        die("interrupted", 130)


if __name__ == "__main__":
    main()
