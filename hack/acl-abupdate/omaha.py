"""The harness's side of trident-acl-agent's update protocol: a fake Omaha ("Nebraska") server
that offers COSIs and serves them, the TLS material it needs, and the Node annotations that ask
the agent for an update.

AKS asks a node for an OS update by writing `<prefix>/update-request` on its Node. trident-acl-agent,
watching its own Node, sends an update check to the Omaha server the request names, requires the
offered version to be the request's targetVersion, and has tridentd stage the offered package's
URL, verified by the COSI metadata SHA-384 in the package's `cosi.metadata.sha384` label. It
reports progress in `<prefix>/update-status`, and after the finalize reboot commits and reports
`<prefix>/update-commit-status`. Along the way it reports Omaha events: 13/1 and 14/1 around the
stage, 800/1 after finalize, and after the reboot one request that carries 3/2, a ping, and an
update check; 3/0 when something fails.

The wire formats follow microsoft/trident: docs/Explanation/Trident-ACL-Agent.md, the agent's
crates/trident-acl-agent/src/core/nebraska/{wire,client,transport}.rs and annotations/protocol.rs,
and the fakes its storm tests use, tools/storm/aclagent/proxies. Responses are shaped like
Nebraska's (flatcar/nebraska backend/pkg/omaha/omaha.go), without its instance state machine: this
server never answers error-updateInProgressOnInstance, so a retried stage or a VM restored from a
snapshot mid-update isn't wedged. It's tolerant: it logs what it doesn't expect and answers anyway.
"""

from __future__ import annotations

import contextlib
import dataclasses
import datetime
import hashlib
import ipaddress
import json
import os
import re
import secrets
import shutil
import socket
import socketserver
import ssl
import subprocess
import sys
import threading
import time
import urllib.parse
import xml.etree.ElementTree as ET
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from errors import StepError

Run = Callable[..., subprocess.CompletedProcess[str]]
Log = Callable[[str], None]

DEFAULT_PORT = 8297
# What the VM reaches the host as: QEMU user networking maps 10.0.2.2 to the host's loopback, and
# the cluster bridge's gateway is the host too, for a server that listens there. 127.0.0.1 and
# localhost are for checks from the host.
DEFAULT_SANS = ("IP:10.0.2.2", "IP:192.168.113.1", "IP:127.0.0.1", "DNS:localhost")
UPDATE_PATH = "/v1/update/"
IMAGE_PATH = "/images/"
HEALTH_PATH = "/healthz"
OMAHA_PROTOCOL = "3.0"
SERVER_NAME = "acl-abupdate"
# The label the agent reads the COSI metadata SHA-384 from, as 96 hex characters, by default
# (TRIDENT_ACL_AGENT_COSI_SHA384_SOURCE=labels; core/cosi_sha384.rs).
COSI_SHA384_LABEL = "cosi.metadata.sha384"
# The (eventtype, eventresult) pairs Nebraska accepts and silently drops the rest of
# (core/nebraska/event.rs), and what the agent means by each.
EVENT_NAMES = {
    ("3", "0"): "failed",
    ("3", "1"): "completed",
    ("3", "2"): "completed after reboot",
    ("13", "1"): "download started",
    ("14", "1"): "download finished",
    ("800", "1"): "installed",
}
TERMINAL_SUCCESS_EVENTS = frozenset({("3", "1"), ("3", "2")})
# The agent's affinity-cookie handling mirrors an App Service-style load balancer's ASLBSA cookie
# (core/nebraska/transport.rs), which storm's proxy simulates the same way.
AFFINITY_COOKIE_NAME = "ASLBSA"

SCHEMA_VERSION = "1.0"
REQUEST_KEY = "update-request"
STATUS_KEY = "update-status"
COMMIT_STATUS_KEY = "update-commit-status"
ANNOTATION_KEYS = (REQUEST_KEY, STATUS_KEY, COMMIT_STATUS_KEY)
OPERATIONS = ("stage", "finalize", "rollback")
REQUEST_FIELDS = ("schemaVersion", "nodeUpdateId", "operationId", "operation", "targetVersion", "server", "appId", "track")
STATUS_CODES = frozenset({
    "InProgress", "Success", "AlreadyAtTarget", "NotStaged", "OperationFailed", "TargetBootFailed",
    "AgentInternalError", "InvalidRequest",
})
TERMINAL_CODES = STATUS_CODES - {"InProgress"}

CA_DAYS = 3650
SERVER_DAYS = 365
CA_RENEW_SECONDS = 30 * 86400
SERVER_RENEW_SECONDS = 7 * 86400
OPENSSL_TIMEOUT = 60

CHUNK_SIZE = 1 << 20
MAX_OMAHA_BODY = 1 << 20
HANDSHAKE_TIMEOUT = 30
# Long enough for a keep-alive connection between tridentd's range requests, short enough that an
# abandoned one doesn't hold a thread for good.
IDLE_TIMEOUT = 120
PROGRESS_LOG_SECONDS = 30

NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
SHA384_PATTERN = re.compile(r"[0-9a-fA-F]{96}")
UUID_PATTERN = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")
# https://semver.org's pattern. The agent compares a request's targetVersion with the offered
# version as the semver crate prints it, so anything but canonical semver can never match.
SEMVER_PATTERN = re.compile(
    r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)"
    r"(?:-(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*)?"
    r"(?:\+[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*)?", re.ASCII)
HOSTNAME_PATTERN = re.compile(
    r"(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*",
    re.ASCII)
PREFIX_PATTERN = re.compile(r"(?=.{1,253}$)[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?", re.ASCII)
ANNOTATION_NAME_PATTERN = re.compile(r"[A-Za-z0-9](?:[-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?", re.ASCII)
COOKIE_PATTERN = re.compile(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+=[\x21\x23-\x2b\x2d-\x3a\x3c-\x5b\x5d-\x7e]*", re.ASCII)
RANGE_PATTERN = re.compile(r"bytes=([0-9]*)-([0-9]*)", re.ASCII | re.IGNORECASE)


def now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="milliseconds")


def human_size(size: int) -> str:
    value = float(size)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.1f} {unit}" if unit != "B" else f"{size} B"
        value /= 1024
    return f"{size} B"


def url_host(host: str) -> str:
    return f"[{host}]" if ":" in host else host


# ---------------------------------------------------------------------------------------------
# Versions and packages


def tag_to_semver(tag: str) -> str:
    """The semver version an image tag such as 20261106.1 is offered as: 20261106.1.0.

    The agent parses the offered version as semver and requires it to print exactly as the
    request's targetVersion, and Nebraska registers an instance only with a semver version. A tag
    of one to three numbers maps to one; anything else can't, so it's refused rather than guessed.
    """
    parts = tag.split(".")
    if not 1 <= len(parts) <= 3 or not all(re.fullmatch(r"0|[1-9][0-9]*", part, re.ASCII) for part in parts):
        raise StepError(f"tag {tag!r} can't be offered as a semver version: it needs one to three dot-separated "
                        "numbers without leading zeros, as in 20261106.1")
    return ".".join(parts + ["0"] * (3 - len(parts)))


def is_semver(version: str) -> bool:
    return SEMVER_PATTERN.fullmatch(version) is not None


@dataclasses.dataclass(frozen=True)
class Package:
    """A COSI the server offers in update checks and serves at /images/<name>.cosi."""

    name: str
    version: str
    path: Path
    size: int
    metadata_sha384: str

    @property
    def file_name(self) -> str:
        return f"{self.name}.cosi"

    @classmethod
    def for_cosi(cls, name: str, path: Path, metadata_sha384: str, version: str | None = None) -> Package:
        """A package for a COSI on disk, offered as its tag's semver version unless one is given."""
        try:
            size = path.stat().st_size
        except OSError as error:
            raise StepError(f"can't offer {path}: {error}") from error
        return cls(name, version or tag_to_semver(name), path, size, metadata_sha384.lower())


def package_problems(package: Package) -> list[str]:
    """What would stop the agent or tridentd from taking the package, without touching the disk."""
    problems = []
    if not NAME_PATTERN.fullmatch(package.name):
        problems.append(f"name {package.name!r} isn't usable in a URL path")
    if not is_semver(package.version):
        problems.append(f"version {package.version!r} isn't semver, which the agent requires")
    if not SHA384_PATTERN.fullmatch(package.metadata_sha384):
        problems.append("its metadata SHA-384 isn't 96 hex characters")
    if package.size < 0:
        problems.append(f"its size {package.size} is negative")
    return problems


def check_package(package: Package) -> None:
    problems = package_problems(package)
    try:
        actual = package.path.stat().st_size
    except OSError as error:
        problems.append(f"{package.path} can't be read: {error}")
    else:
        if not package.path.is_file():
            problems.append(f"{package.path} isn't a file")
        elif actual != package.size:
            problems.append(f"{package.path} is {actual} bytes, not {package.size}")
    if problems:
        raise StepError(f"can't offer {package.name}: {'; '.join(problems)}")


# ---------------------------------------------------------------------------------------------
# TLS material

CA_CONFIG = """\
[req]
prompt = no
distinguished_name = dn
x509_extensions = v3_ca

[dn]
CN = {common_name}
O = acl-abupdate harness

[v3_ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
"""

SERVER_CONFIG = """\
[req]
prompt = no
distinguished_name = dn

[dn]
CN = acl-abupdate omaha
O = acl-abupdate harness

[v3_server]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = {sans}
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always
"""


@dataclasses.dataclass(frozen=True)
class TLSFiles:
    """The CA the VM trusts, and the server certificate and key the CA signed."""

    directory: Path
    ca_cert: Path
    server_cert: Path
    server_key: Path

    @property
    def ca_key(self) -> Path:
        return self.directory / "ca.key"

    @classmethod
    def at(cls, directory: Path) -> TLSFiles:
        return cls(directory, directory / "ca.pem", directory / "server.pem", directory / "server.key")


def normalize_sans(sans: list[str] | tuple[str, ...]) -> list[str]:
    """Subject alternative names in the form OpenSSL's config takes, IP:... or DNS:..., in order
    and without duplicates. A bare address or name is taken as one."""
    normalized: list[str] = []
    for raw in sans:
        text = raw.strip()
        kind, separator, value = text.partition(":")
        kind = kind.upper()
        if not separator or kind not in ("IP", "DNS"):
            kind, value = "", text
        entry = san_entry(kind, value, raw)
        if entry not in normalized:
            normalized.append(entry)
    if not normalized:
        raise StepError("the server certificate needs at least one subject alternative name")
    return normalized


def san_entry(kind: str, value: str, raw: str) -> str:
    if kind in ("", "IP"):
        try:
            return f"IP:{ipaddress.ip_address(value)}"
        except ValueError:
            if kind == "IP":
                raise StepError(f"SAN {raw!r} isn't an IP address") from None
    if HOSTNAME_PATTERN.fullmatch(value):
        return f"DNS:{value.lower()}"
    raise StepError(f"SAN {raw!r} isn't an IP address or a DNS name")


def parse_openssl_sans(text: str) -> set[str]:
    """The SANs in `openssl x509 -noout -ext subjectAltName` output, normalized."""
    found = set()
    for token in re.split(r"[,\n]", text):
        token = token.strip()
        if token.startswith("IP Address:"):
            with contextlib.suppress(ValueError):
                found.add(f"IP:{ipaddress.ip_address(token.removeprefix('IP Address:').strip())}")
        elif token.startswith("DNS:"):
            found.add(f"DNS:{token.removeprefix('DNS:').strip().lower()}")
    return found


def make_tls(directory: Path, sans: list[str] | tuple[str, ...], *, run: Run, log: Log | None = None) -> TLSFiles:
    """A CA and a server certificate for the given SANs, under a directory only the user can read.

    It's idempotent: what's there is kept while it's valid, so the VM's trust survives harness
    runs. A server certificate that's expiring, doesn't verify, or names other SANs is replaced,
    signed by the same CA, so the VM needn't trust anything new. Only a missing, expiring, or
    broken CA is replaced, and then the VM has to trust the new one.
    """
    if shutil.which("openssl") is None:
        raise StepError("openssl isn't installed; the harness needs it to make the Omaha server's certificate")
    wanted = normalize_sans(sans)
    directory = directory.expanduser()
    try:
        directory.mkdir(parents=True, exist_ok=True)
        os.chmod(directory, 0o700)
    except OSError as error:
        raise StepError(f"can't make {directory}: {error}") from error
    files = TLSFiles.at(directory)
    renewed_ca = False
    if not ca_valid(files, run):
        make_ca(files, run)
        renewed_ca = True
        if log:
            log(f"made a CA for the Omaha server, {files.ca_cert}; the VM has to trust it")
    if renewed_ca or not server_valid(files, wanted, run):
        make_server_cert(files, wanted, run)
        if log:
            log(f"made the Omaha server's certificate for {', '.join(wanted)}")
    try:
        for key in (files.ca_key, files.server_key):
            os.chmod(key, 0o600)
        for cert in (files.ca_cert, files.server_cert):
            os.chmod(cert, 0o644)
    except OSError as error:
        raise StepError(f"can't set the permissions of the TLS files in {directory}: {error}") from error
    if not server_valid(files, wanted, run):
        raise StepError(f"the certificate made in {directory} doesn't verify; is openssl 1.1.1 or later installed?")
    return files


def openssl(run: Run, args: list[str], what: str) -> subprocess.CompletedProcess[str]:
    result = run(["openssl", *args], timeout=OPENSSL_TIMEOUT)
    if result.returncode != 0:
        detail = "\n".join((result.stderr or result.stdout or "").strip().splitlines()[-6:])
        raise StepError(f"can't {what}: openssl {args[0]} exited {result.returncode}: {detail}")
    return result


def openssl_output(run: Run, args: list[str]) -> str | None:
    result = run(["openssl", *args], timeout=OPENSSL_TIMEOUT)
    return (result.stdout or "") if result.returncode == 0 else None


def key_matches(cert: Path, key: Path) -> bool:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    try:
        context.load_cert_chain(cert, key)
    except (ssl.SSLError, OSError, ValueError):
        return False
    return True


def ca_valid(files: TLSFiles, run: Run) -> bool:
    if not (files.ca_cert.is_file() and files.ca_key.is_file()):
        return False
    if openssl_output(run, ["x509", "-in", str(files.ca_cert), "-noout", "-checkend", str(CA_RENEW_SECONDS)]) is None:
        return False
    extensions = openssl_output(run, ["x509", "-in", str(files.ca_cert), "-noout", "-ext", "basicConstraints,keyUsage"])
    if extensions is None or "CA:TRUE" not in extensions or "Certificate Sign" not in extensions:
        return False
    return key_matches(files.ca_cert, files.ca_key)


def server_valid(files: TLSFiles, wanted: list[str], run: Run) -> bool:
    if not (files.server_cert.is_file() and files.server_key.is_file() and files.ca_cert.is_file()):
        return False
    cert = str(files.server_cert)
    if openssl_output(run, ["x509", "-in", cert, "-noout", "-checkend", str(SERVER_RENEW_SECONDS)]) is None:
        return False
    if openssl_output(run, ["verify", "-CAfile", str(files.ca_cert), "-purpose", "sslserver", cert]) is None:
        return False
    sans = openssl_output(run, ["x509", "-in", cert, "-noout", "-ext", "subjectAltName"])
    if sans is None or parse_openssl_sans(sans) != set(wanted):
        return False
    return key_matches(files.server_cert, files.server_key)


def make_ca(files: TLSFiles, run: Run) -> None:
    # A unique subject: a VM that still trusts an earlier CA mustn't find two with one hash.
    config = files.directory / "ca.cnf"
    config.write_text(CA_CONFIG.format(common_name=f"acl-abupdate test CA {secrets.token_hex(4)}"))
    for stale in (files.server_cert, files.server_key):
        stale.unlink(missing_ok=True)
    openssl(run, ["genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", str(files.ca_key)],
            "make the CA's key")
    os.chmod(files.ca_key, 0o600)
    openssl(run, ["req", "-new", "-x509", "-config", str(config), "-extensions", "v3_ca", "-key", str(files.ca_key),
                  "-sha256", "-days", str(CA_DAYS), "-out", str(files.ca_cert)], "make the CA's certificate")


def make_server_cert(files: TLSFiles, sans: list[str], run: Run) -> None:
    config = files.directory / "server.cnf"
    config.write_text(SERVER_CONFIG.format(sans=", ".join(sans)))
    request = files.directory / "server.csr"
    openssl(run, ["genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", str(files.server_key)],
            "make the server's key")
    os.chmod(files.server_key, 0o600)
    try:
        openssl(run, ["req", "-new", "-config", str(config), "-key", str(files.server_key), "-out", str(request)],
                "make the server's certificate request")
        openssl(run, ["x509", "-req", "-in", str(request), "-CA", str(files.ca_cert), "-CAkey", str(files.ca_key),
                      "-set_serial", f"0x{secrets.randbits(127):x}", "-sha256", "-days", str(SERVER_DAYS),
                      "-extfile", str(config), "-extensions", "v3_server", "-out", str(files.server_cert)],
                "sign the server's certificate")
    finally:
        request.unlink(missing_ok=True)


# ---------------------------------------------------------------------------------------------
# The Omaha wire format


@dataclasses.dataclass(frozen=True)
class OmahaEvent:
    eventtype: str
    eventresult: str
    previousversion: str = ""

    @property
    def pair(self) -> tuple[str, str]:
        return self.eventtype, self.eventresult

    @property
    def name(self) -> str:
        return EVENT_NAMES.get(self.pair, "not one Nebraska accepts")

    @property
    def terminal_success(self) -> bool:
        return self.pair in TERMINAL_SUCCESS_EVENTS


@dataclasses.dataclass(frozen=True)
class OmahaApp:
    app_id: str
    version: str
    track: str
    machine_id: str
    events: tuple[OmahaEvent, ...] = ()
    ping: bool = False
    updatecheck: bool = False

    @property
    def kind(self) -> str:
        """What the request is for: an update check (with or without events), events alone, a ping
        alone, or a probe, the bare <app> that `--validate-connection nebraska` sends."""
        if self.updatecheck:
            return "updatecheck"
        if self.events:
            return "event"
        return "ping" if self.ping else "probe"

    @property
    def elements(self) -> list[str]:
        return [f"event {event.eventtype}/{event.eventresult}" for event in self.events] + \
            (["ping"] if self.ping else []) + (["updatecheck"] if self.updatecheck else [])


@dataclasses.dataclass(frozen=True)
class OmahaRequest:
    protocol: str
    apps: tuple[OmahaApp, ...]
    os_arch: str = ""


def local_name(tag: str) -> str:
    return tag.rpartition("}")[2]


def parse_omaha_request(body: bytes) -> OmahaRequest:
    """An Omaha request, as the agent's wire.rs writes it. Raises ValueError if it isn't one.
    Documents with a DTD are refused: nothing legitimate sends one."""
    if re.search(rb"<!\s*(DOCTYPE|ENTITY)", body, re.IGNORECASE):
        raise ValueError("the request has a DTD")
    try:
        root = ET.fromstring(body)
    except ET.ParseError as error:
        raise ValueError(f"the request isn't XML: {error}") from error
    if local_name(root.tag) != "request":
        raise ValueError(f"the document is a <{local_name(root.tag)}>, not a <request>")
    os_arch = ""
    apps = []
    for child in root:
        name = local_name(child.tag)
        if name == "os":
            os_arch = child.get("arch", "")
        elif name == "app":
            events = tuple(OmahaEvent(element.get("eventtype", ""), element.get("eventresult", ""),
                                      element.get("previousversion", ""))
                           for element in child if local_name(element.tag) == "event")
            names = {local_name(element.tag) for element in child}
            apps.append(OmahaApp(child.get("appid", ""), child.get("version", ""), child.get("track", ""),
                                 child.get("machineid", ""), events, "ping" in names, "updatecheck" in names))
    return OmahaRequest(root.get("protocol", ""), tuple(apps), os_arch)


def request_notes(request: OmahaRequest) -> list[str]:
    """What in a request isn't what the agent sends, or would make Nebraska drop something
    silently. The server answers regardless; these are for the log."""
    notes = []
    if request.protocol != OMAHA_PROTOCOL:
        notes.append(f"protocol {request.protocol!r}, not {OMAHA_PROTOCOL}")
    if len(request.apps) != 1:
        notes.append(f"{len(request.apps)} apps, not one")
    if request.os_arch and request.os_arch not in ("amd64", "x64"):
        notes.append(f"os arch {request.os_arch!r}; the packages are for amd64")
    for app in request.apps:
        for attribute, value in (("appid", app.app_id), ("track", app.track), ("machineid", app.machine_id),
                                 ("version", app.version)):
            if not value:
                notes.append(f"no {attribute}")
        if app.version and not is_semver(app.version):
            notes.append(f"version {app.version!r} isn't semver")
        if re.fullmatch(r"\{.{8}-.{4}-.{4}-.{4}-.{12}\}", app.machine_id):
            notes.append("a braced machineid, which Nebraska hides")
        for event in app.events:
            if event.pair not in EVENT_NAMES:
                notes.append(f"event {event.eventtype}/{event.eventresult}, which Nebraska drops")
            elif event.eventtype == "3" and not event.previousversion:
                notes.append(f"event {event.eventtype}/{event.eventresult} without previousversion, which Nebraska drops")
    return notes


def update_decision(app: OmahaApp, package: Package | None, *, app_id: str | None = None,
                    track: str | None = None) -> tuple[Package | None, str]:
    """Whether an update check is answered with the package, and why.

    Nebraska offers a package per application and channel, the request's appId and track, so with
    app_id or track given, a check for another gets noupdate, as Nebraska would answer it.

    Nebraska grants an instance a package newer than the version it reports. The agent always
    reports the version its CURRENT_VERSION settings give, the drop-in's 0.0.0 before an update,
    so any offer is newer; the target's version only comes back in the post-reboot request, which
    also carries the terminal event. Both get noupdate, as Nebraska answers them.
    """
    if package is None:
        return None, "nothing is offered"
    if app_id and app.app_id.lower() != app_id.lower():
        return None, f"it's for app {app.app_id or '(none)'}, and {package.version} is offered for app {app_id}"
    if track and app.track != track:
        return None, f"it's on track {app.track or '(none)'}, and {package.version} is offered on track {track}"
    if any(event.terminal_success for event in app.events):
        return None, "it reports a completed update"
    if app.version == package.version:
        return None, f"it's at {package.version} already"
    return package, f"offered {package.version} ({package.file_name})"


def omaha_response(request: OmahaRequest, package: Package | None, codebase: str, *, app_id: str | None = None,
                   track: str | None = None) -> bytes:
    """The response to a request, shaped like Nebraska's: per app, a ping acknowledgement, the
    update check's answer, and one <event status="ok"/> per event. The agent judges an event
    delivered by its acknowledgement and an offer by its manifest version and label, and joins
    the codebase with the package name for the URL tridentd downloads."""
    root = ET.Element("response", {"protocol": OMAHA_PROTOCOL, "server": SERVER_NAME})
    ET.SubElement(root, "daystart", {"elapsed_seconds": "0"})
    for app in request.apps:
        element = ET.SubElement(root, "app", {"appid": app.app_id, "status": "ok"})
        if app.ping:
            ET.SubElement(element, "ping", {"status": "ok"})
        if app.updatecheck:
            offered, _ = update_decision(app, package, app_id=app_id, track=track)
            if offered is None:
                ET.SubElement(element, "updatecheck", {"status": "noupdate"})
            else:
                add_offer(element, offered, codebase)
        for _ in app.events:
            ET.SubElement(element, "event", {"status": "ok"})
    return b'<?xml version="1.0" encoding="UTF-8"?>\n' + ET.tostring(root, encoding="unicode").encode()


def add_offer(app: ET.Element, package: Package, codebase: str) -> None:
    # The legacy hash attribute is left out, as storm's proxy does, so an agent reading it
    # instead of the label fails rather than passes.
    check = ET.SubElement(app, "updatecheck", {"status": "ok"})
    ET.SubElement(ET.SubElement(check, "urls"), "url", {"codebase": codebase})
    manifest = ET.SubElement(check, "manifest", {"version": package.version})
    item = ET.SubElement(ET.SubElement(manifest, "packages"), "package",
                         {"name": package.file_name, "size": str(package.size), "required": "true"})
    ET.SubElement(ET.SubElement(item, "labels"), "label",
                  {"key": COSI_SHA384_LABEL, "value": package.metadata_sha384.lower()})


def cookie_state(header: str | None, cookie: str) -> str:
    """Whether a request carried the affinity cookie: missing, correct, or wrong. The agent sends
    the name=value it stored, alone, so anything else is a stale or corrupted cookie."""
    if not header or not header.strip():
        return "missing"
    return "correct" if header.strip() == cookie else "wrong"


# ---------------------------------------------------------------------------------------------
# Byte ranges


class RangeError(ValueError):
    """A Range header the server won't serve exactly, which gets 416."""


def parse_range(header: str | None, size: int) -> tuple[int, int] | None:
    """The inclusive byte span a Range header asks for, or None for the whole file.

    Stricter than RFC 9110, which lets a server ignore a Range it doesn't like and send the whole
    file: tridentd reads Content-Length bytes from any 2xx answer as if they were the range it
    asked for (io_utils/http/subfile.rs), so anything but the exact range would corrupt the image.
    Multiple ranges and malformed ones are refused instead.
    """
    if header is None:
        return None
    match = RANGE_PATTERN.fullmatch(header.replace(" ", "").replace("\t", ""))
    if match is None:
        raise RangeError(f"Range {header!r} isn't a single byte range")
    first, last = match.groups()
    if not first:
        if not last or int(last) == 0 or size == 0:
            raise RangeError(f"Range {header!r} asks for nothing of {size} bytes")
        return max(0, size - int(last)), size - 1
    start = int(first)
    if start >= size:
        raise RangeError(f"Range {header!r} starts past the end of {size} bytes")
    if not last:
        return start, size - 1
    end = int(last)
    if end < start:
        raise RangeError(f"Range {header!r} ends before it starts")
    return start, min(end, size - 1)


# ---------------------------------------------------------------------------------------------
# The server


class OmahaServer:
    """The Omaha server and the COSI file server the agent's requests are pointed at, on one
    HTTPS port, served from a daemon thread.

        with OmahaServer(tls=tls, log=log) as server:
            server.offer(package)
            ...patch the Node with request_patch(prefix, update_request(server=server.update_url, ...))
            server.wait_for_event("14", "1", timeout=...)

    It records every request and event for the harness to check, and sets the affinity cookie on
    any Omaha request that doesn't carry it. TLS stops at 1.2, as storm's proxies do: an OpenSSL
    client build the agent and tridentd link mishandles TLS 1.3's post-handshake session tickets.
    """

    def __init__(self, *, host: str = "127.0.0.1", port: int = DEFAULT_PORT, tls: TLSFiles,
                 public_host: str = "10.0.2.2", log: Log, affinity_cookie: str | None = None):
        if affinity_cookie is not None and not COOKIE_PATTERN.fullmatch(affinity_cookie):
            raise StepError(f"affinity cookie {affinity_cookie!r} isn't a name=value cookie")
        self.host = host
        self.public_host = public_host
        self.tls = tls
        self.log = log
        self._port = port
        self._cookie = affinity_cookie
        self._lock = threading.Lock()
        self._condition = threading.Condition(self._lock)
        self._offered: Package | None = None
        self._offered_for: tuple[str | None, str | None] = (None, None)
        # Bytes per second the images are served at, across all connections, or 0 for as fast as
        # they go; and when the next chunk may be sent.
        self._rate = 0
        self._next_send = 0.0
        self._images: dict[str, Package] = {}
        self._requests: list[dict[str, Any]] = []
        self._events: list[dict[str, Any]] = []
        self._served: dict[str, list[float]] = {}
        self._connections: set[ssl.SSLSocket] = set()
        self._server: _HTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._stopping = False

    def __enter__(self) -> OmahaServer:
        self.start()
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.stop()

    def start(self) -> None:
        if self._server is not None:
            return
        if self._cookie is None:
            self._cookie = default_affinity_cookie(self.tls)
        context = server_context(self.tls)
        try:
            server = _HTTPServer((self.host, self._port), context, self)
        except OSError as error:
            raise StepError(f"the Omaha server can't listen on {url_host(self.host)}:{self._port}: {error}") from error
        self._port = server.server_address[1]
        self._server = server
        self._stopping = False
        self._thread = threading.Thread(target=server.serve_forever, name=f"omaha-{self._port}", daemon=True)
        self._thread.start()
        self.log(f"omaha: listening on {url_host(self.host)}:{self._port}; the VM uses {self.update_url}")

    def stop(self) -> None:
        server, thread = self._server, self._thread
        if server is None:
            return
        self._stopping = True
        server.shutdown()
        server.server_close()
        with self._lock:
            connections = list(self._connections)
        # Wakes handler threads blocked on keep-alive connections, which serve_forever's shutdown
        # doesn't reach. The base class's shutdown, because SSLSocket's drops its SSL object.
        for connection in connections:
            with contextlib.suppress(OSError):
                socket.socket.shutdown(connection, socket.SHUT_RDWR)
        if thread is not None:
            thread.join(timeout=10)
        self._server = None
        self._thread = None
        with self._lock:
            count = len(self._requests)
        self.log(f"omaha: stopped after {count} requests")

    @property
    def port(self) -> int:
        return self._port

    @property
    def running(self) -> bool:
        return self._server is not None

    def _public_base(self) -> str:
        if self._port == 0:
            raise StepError("the Omaha server's port isn't known until it starts")
        return f"https://{url_host(self.public_host)}:{self._port}"

    @property
    def update_url(self) -> str:
        """The request's `server`: the agent POSTs to exactly this URL."""
        return self._public_base() + UPDATE_PATH

    @property
    def codebase(self) -> str:
        """Where offered packages are, with the trailing slash the agent's URL join keeps."""
        return self._public_base() + IMAGE_PATH

    def image_url(self, package: Package) -> str:
        return self.codebase + package.file_name

    @property
    def affinity_cookie(self) -> str:
        """The name=value the server sets, which the agent stores in
        /var/lib/trident-acl-agent/nebraska-cookie."""
        if self._cookie is None:
            self._cookie = default_affinity_cookie(self.tls)
        return self._cookie

    @property
    def offered(self) -> Package | None:
        with self._lock:
            return self._offered

    @property
    def events(self) -> list[dict[str, Any]]:
        """Every event received, in order: eventtype, eventresult, version (the app's), previousversion,
        and time, plus name, app_id, track, and machine_id."""
        with self._lock:
            return [dict(event) for event in self._events]

    @property
    def requests(self) -> list[dict[str, Any]]:
        """A summary of every request, in order; each has the same keys. kind is updatecheck, event,
        ping, or probe for Omaha requests, image or health for GETs and HEADs, tls-error for a
        failed handshake, and other for anything unexpected."""
        with self._lock:
            return [dict(request) for request in self._requests]

    def offer(self, package: Package | None, *, app_id: str | None = None, track: str | None = None) -> None:
        """Answer update checks with this package from now on, or with noupdate for None. With
        app_id or track, only checks for that application and track get it, as Nebraska offers a
        package per application and channel. Every package offered stays downloadable, so a stage
        started before a change still completes."""
        if package is not None:
            check_package(package)
            package = dataclasses.replace(package, metadata_sha384=package.metadata_sha384.lower())
        with self._lock:
            self._offered = package
            self._offered_for = (app_id, track)
            if package is not None:
                self._images[package.file_name] = package
        if package is None:
            self.log("omaha: offering nothing; update checks get noupdate")
        else:
            scope = "".join([f", for app {app_id}" if app_id else "", f", on track {track}" if track else ""])
            self.log(f"omaha: offering {package.name} as {package.version}{scope}, {human_size(package.size)}, "
                     f"metadata sha384 {package.metadata_sha384[:12]}")

    @property
    def offered_for(self) -> tuple[str | None, str | None]:
        """The application and track the package is offered for; None for any."""
        with self._lock:
            return self._offered_for

    def throttle(self, bytes_per_second: int) -> None:
        """Serve images at most this fast, across all connections, or as fast as they go for 0. A
        slow download keeps a stage running long enough to watch the agent's heartbeat."""
        if bytes_per_second < 0:
            raise StepError("a throttle can't be negative")
        with self._lock:
            self._rate = bytes_per_second
            self._next_send = 0.0
        self.log(f"omaha: serving images at {human_size(bytes_per_second)}/s" if bytes_per_second
                 else "omaha: serving images as fast as they go")

    def _pace(self, size: int) -> None:
        """Wait until size more bytes may be sent, at the throttle's rate."""
        with self._lock:
            rate = self._rate
            if not rate:
                return
            now = time.monotonic()
            start = max(now, self._next_send)
            self._next_send = start + size / rate
            wait = self._next_send - now
        if wait > 0:
            time.sleep(wait)

    def wait_for_event(self, eventtype: str, eventresult: str | None = None, *, timeout: float,
                       start: int = 0) -> dict[str, Any]:
        """The first event of the type, and result if given, received since events[start]. Pass
        start=len(server.events) before a request to ignore earlier operations' events."""
        wanted_type = str(eventtype)
        wanted_result = None if eventresult is None else str(eventresult)
        what = f"{wanted_type}/{wanted_result or '*'}"
        deadline = time.monotonic() + timeout
        with self._condition:
            while True:
                for event in self._events[start:]:
                    if event["eventtype"] == wanted_type and wanted_result in (None, event["eventresult"]):
                        return dict(event)
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                self._condition.wait(remaining)
            seen = ", ".join(f"{event['eventtype']}/{event['eventresult']}" for event in self._events[start:])
        raise StepError(f"the Omaha server got no event {what} in {timeout:g}s; it got {seen or 'none'}")

    # The handler's callbacks, from connection threads.

    def _image(self, file_name: str) -> Package | None:
        with self._lock:
            return self._images.get(file_name)

    def _record(self, summary: dict[str, Any]) -> None:
        with self._lock:
            self._requests.append(summary)

    def _record_events(self, app: OmahaApp, received: str) -> None:
        with self._condition:
            for event in app.events:
                self._events.append({
                    "eventtype": event.eventtype, "eventresult": event.eventresult, "version": app.version,
                    "previousversion": event.previousversion, "time": received, "name": event.name,
                    "app_id": app.app_id, "track": app.track, "machine_id": app.machine_id,
                })
            self._condition.notify_all()

    def _opened(self, connection: ssl.SSLSocket) -> None:
        with self._lock:
            self._connections.add(connection)

    def _closed(self, connection: ssl.SSLSocket) -> None:
        with self._lock:
            self._connections.discard(connection)

    def _tls_failed(self, client: tuple[Any, ...], error: Exception) -> None:
        if self._stopping:
            return
        summary = request_summary(client[0], "", "", "tls-error")
        summary["error"] = str(error)
        self._record(summary)
        hint = "; the client doesn't trust the harness's CA" if "unknown ca" in str(error).lower() else ""
        self.log(f"omaha: TLS handshake with {client[0]} failed: {error}{hint}")

    def _connection_failed(self, client: tuple[Any, ...], error: BaseException) -> None:
        if self._stopping or isinstance(error, (ConnectionError, TimeoutError, ssl.SSLError)):
            return
        self.log(f"omaha: the connection from {client[0]} failed: {type(error).__name__}: {error}")

    def _served_progress(self, file_name: str, sent: int) -> None:
        """Log a download's progress at most every PROGRESS_LOG_SECONDS: tridentd reads a COSI in
        many range requests, too many to log one by one."""
        now = time.monotonic()
        with self._lock:
            progress = self._served.setdefault(file_name, [0.0, 0.0, now])
            progress[0] += sent
            progress[1] += 1
            due = now - progress[2] >= PROGRESS_LOG_SECONDS
            if due:
                progress[2] = now
            total, count = int(progress[0]), int(progress[1])
        if due:
            self.log(f"omaha: served {human_size(total)} of {file_name} in {count} requests so far")


def default_affinity_cookie(tls: TLSFiles) -> str:
    """A cookie that's the same for every server using the same certificate. The agent keeps the
    cookie across the finalize reboot, and the harness may run the stage and the finalize in
    different processes, so a fresh cookie per server would make the agent's correct behavior look
    like a wrong cookie."""
    try:
        digest = hashlib.sha256(tls.server_cert.read_bytes()).hexdigest()[:16]
    except OSError as error:
        raise StepError(f"can't read {tls.server_cert}: {error}") from error
    return f"{AFFINITY_COOKIE_NAME}=acl-abupdate-{digest}"


def server_context(tls: TLSFiles) -> ssl.SSLContext:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.maximum_version = ssl.TLSVersion.TLSv1_2
    # TLS 1.2 sends its ticket inside the handshake, which is fine; going without makes the
    # server's behavior one thing less to wonder about when a client misbehaves.
    context.options |= ssl.OP_NO_TICKET
    try:
        context.load_cert_chain(tls.server_cert, tls.server_key)
    except (ssl.SSLError, OSError) as error:
        raise StepError(f"can't load the Omaha server's certificate {tls.server_cert}: {error}") from error
    return context


def request_summary(client: str, method: str, path: str, kind: str) -> dict[str, Any]:
    return {
        "time": now_iso(), "client": client, "method": method, "path": path, "kind": kind, "status": None,
        "app_id": None, "version": None, "track": None, "machine_id": None, "elements": [], "updatecheck": None,
        "cookie": None, "cookie_state": None, "set_cookie": False, "range": None, "bytes": 0, "aborted": False,
        "error": None,
    }


class _HTTPServer(ThreadingHTTPServer):
    """One thread per connection, each doing its own TLS handshake: done in accept(), a client
    that stalls mid-handshake would stop the server accepting anyone else."""

    daemon_threads = True

    def __init__(self, address: tuple[str, int], context: ssl.SSLContext, owner: OmahaServer):
        self.address_family = socket.AF_INET6 if ":" in address[0] else socket.AF_INET
        self.tls_context = context
        self.owner = owner
        super().__init__(address, _Handler)

    def server_bind(self) -> None:
        # HTTPServer's own looks the host's FQDN up, which can stall on a host without DNS.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = str(self.server_address[0]), int(self.server_address[1])

    def finish_request(self, request: Any, client_address: Any) -> None:
        request.settimeout(HANDSHAKE_TIMEOUT)
        try:
            connection = self.tls_context.wrap_socket(request, server_side=True)
        except (ssl.SSLError, OSError) as error:
            self.owner._tls_failed(client_address, error)
            return
        self.owner._opened(connection)
        try:
            self.RequestHandlerClass(connection, client_address, self)
        finally:
            self.owner._closed(connection)
            with contextlib.suppress(OSError):
                connection.close()

    def handle_error(self, request: Any, client_address: Any) -> None:
        # socketserver prints a traceback to stderr; a client hanging up isn't worth one.
        error = sys.exc_info()[1]
        if error is not None:
            self.owner._connection_failed(client_address, error)


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = SERVER_NAME
    sys_version = ""
    timeout = IDLE_TIMEOUT
    server: _HTTPServer

    @property
    def owner(self) -> OmahaServer:
        return self.server.owner

    def log_message(self, format: str, *args: Any) -> None:
        pass

    def log_error(self, format: str, *args: Any) -> None:
        # The base class calls this for requests it rejects before a do_ method runs: bad request
        # lines, unsupported methods. A timeout is an idle keep-alive connection closing.
        message = format % args if args else format
        if not message.startswith("Request timed out") and not self.owner._stopping:
            self.owner.log(f"omaha: unexpected request from {self.client_address[0]}: {message}")

    def _send(self, status: int, body: bytes = b"", *, content_type: str = "text/plain; charset=utf-8",
              headers: list[tuple[str, str]] | None = None, close: bool = False) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        for name, value in headers or []:
            self.send_header(name, value)
        if close:
            self.send_header("Connection", "close")
        self.end_headers()
        if body and self.command != "HEAD":
            self.wfile.write(body)

    def _summary(self, kind: str) -> dict[str, Any]:
        summary = request_summary(self.client_address[0], self.command, self.path, kind)
        summary["cookie"] = self.headers.get("Cookie")
        summary["range"] = self.headers.get("Range")
        return summary

    def _unexpected(self, status: int, summary: dict[str, Any], why: str, *, close: bool = False) -> None:
        summary["status"] = status
        summary["error"] = why
        self.owner._record(summary)
        self.owner.log(f"omaha: {self.command} {self.path} from {self.client_address[0]}: {why}; answered {status}")
        self._send(status, f"{why}\n".encode(), close=close)

    def do_POST(self) -> None:
        summary = self._summary("other")
        cookie = self.owner.affinity_cookie
        state = cookie_state(summary["cookie"], cookie)
        summary["cookie_state"] = state
        length = self.headers.get("Content-Length", "")
        if self.headers.get("Transfer-Encoding") or not length.isdigit():
            self._unexpected(411, summary, "the request has no Content-Length", close=True)
            return
        if int(length) > MAX_OMAHA_BODY:
            self._unexpected(413, summary, f"the request is {length} bytes, more than an Omaha request", close=True)
            return
        body = self.rfile.read(int(length))
        if len(body) != int(length):
            self.close_connection = True
            return
        path = urllib.parse.urlsplit(self.path).path
        try:
            request = parse_omaha_request(body)
        except ValueError as error:
            self._unexpected(400, summary, f"not an Omaha request: {error}")
            return
        package = self.owner.offered
        app_id, track = self.owner.offered_for
        codebase = self.owner.codebase
        response = omaha_response(request, package, codebase, app_id=app_id, track=track)
        received = now_iso()
        decision = ""
        if request.apps:
            app = request.apps[0]
            summary.update(kind=app.kind, app_id=app.app_id, version=app.version, track=app.track,
                           machine_id=app.machine_id, elements=app.elements)
            if app.updatecheck:
                offered, decision = update_decision(app, package, app_id=app_id, track=track)
                summary["updatecheck"] = "noupdate" if offered is None else "ok"
        for app in request.apps:
            self.owner._record_events(app, received)
        headers = []
        if state == "missing":
            # As storm's proxy and an App Service load balancer do: set on any request without it,
            # and leave a request carrying any cookie, even a wrong one, alone.
            headers.append(("Set-Cookie", f"{cookie}; Path=/; HttpOnly"))
            summary["set_cookie"] = True
        summary["status"] = 200
        summary["bytes"] = len(response)
        self.owner._record(summary)
        notes = request_notes(request)
        if path not in (UPDATE_PATH, UPDATE_PATH.rstrip("/")):
            notes.append(f"path {path}, not {UPDATE_PATH}")
        self.owner.log(omaha_log_line(request, decision, state, notes))
        self._send(200, response, content_type="application/xml", headers=headers)

    def do_GET(self) -> None:
        self._get(head=False)

    def do_HEAD(self) -> None:
        self._get(head=True)

    def _get(self, *, head: bool) -> None:
        path = urllib.parse.unquote(urllib.parse.urlsplit(self.path).path)
        if path == HEALTH_PATH:
            summary = self._summary("health")
            summary["status"] = 200
            self.owner._record(summary)
            self._send(200, b"ok\n")
            return
        if path.startswith(IMAGE_PATH) and "/" not in path[len(IMAGE_PATH):]:
            self._serve_image(path[len(IMAGE_PATH):], head=head)
            return
        self._unexpected(404, self._summary("other"), f"nothing is served at {path}")

    def _serve_image(self, file_name: str, *, head: bool) -> None:
        summary = self._summary("image")
        package = self.owner._image(file_name)
        if package is None:
            self._unexpected(404, summary, f"{file_name} was never offered")
            return
        try:
            image = package.path.open("rb")
            size = os.fstat(image.fileno()).st_size
        except OSError as error:
            self._unexpected(500, summary, f"can't read {package.path}: {error}")
            return
        with image:
            try:
                span = parse_range(summary["range"], size)
            except RangeError as error:
                summary["status"] = 416
                summary["error"] = str(error)
                self.owner._record(summary)
                self.owner.log(f"omaha: {self.command} {file_name}: {error}; answered 416")
                self._send(416, headers=[("Content-Range", f"bytes */{size}"), ("Accept-Ranges", "bytes")])
                return
            start, end = span if span is not None else (0, size - 1)
            length = end - start + 1
            status = 200 if span is None else 206
            self.send_response(status)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(length))
            # tridentd's HTTP reader refuses a server that doesn't say it takes ranges.
            self.send_header("Accept-Ranges", "bytes")
            if span is not None:
                self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
            self.end_headers()
            summary["status"] = status
            if head:
                self.owner._record(summary)
                self.owner.log(f"omaha: HEAD {file_name} from {self.client_address[0]}: {human_size(size)}")
                return
            sent = self._stream(image, start, length)
        summary["bytes"] = sent
        summary["aborted"] = sent != length
        self.owner._record(summary)
        self.owner._served_progress(file_name, sent)

    def _stream(self, image: Any, start: int, length: int) -> int:
        """Copy the span in chunks, so a multi-GB COSI never sits in memory. A client that hangs up
        early, as tridentd may when it has read what it needs, ends the connection quietly."""
        sent = 0
        try:
            image.seek(start)
            while sent < length:
                size = min(CHUNK_SIZE, length - sent)
                self.owner._pace(size)
                chunk = image.read(size)
                if not chunk:
                    break
                self.wfile.write(chunk)
                sent += len(chunk)
        except OSError:
            pass
        if sent != length:
            self.close_connection = True
        return sent

    def _method_not_allowed(self) -> None:
        self._unexpected(405, self._summary("other"), f"{self.command} isn't served")

    do_PUT = do_DELETE = do_PATCH = do_OPTIONS = _method_not_allowed


def omaha_log_line(request: OmahaRequest, decision: str, state: str, notes: list[str]) -> str:
    if not request.apps:
        line = "omaha: a request with no app"
    else:
        app = request.apps[0]
        parts = []
        for event in app.events:
            previous = f", previousversion {event.previousversion}" if event.previousversion else ""
            parts.append(f"event {event.eventtype}/{event.eventresult} ({event.name}{previous})")
        if app.ping:
            parts.append("ping")
        if app.updatecheck:
            parts.append(f"updatecheck: {decision}")
        line = (f"omaha: {app.kind} at {app.version or '?'} from machine {app.machine_id[:12] or '?'} "
                f"(app {app.app_id or '?'}, track {app.track or '?'}): {'; '.join(parts) or 'nothing'}")
    cookie = {"missing": "cookie set", "correct": "cookie ok", "wrong": "WRONG cookie"}[state]
    line += f"; {cookie}"
    if notes:
        line += f"; unexpected: {', '.join(notes)}"
    return line


# ---------------------------------------------------------------------------------------------
# The Node annotations


def annotation_key(prefix: str, key: str) -> str:
    """The annotation's full key: <prefix>/<key>, unless key is a full key already."""
    if "/" in key:
        return key
    if not PREFIX_PATTERN.fullmatch(prefix):
        raise StepError(f"annotation prefix {prefix!r} isn't a DNS subdomain")
    if not ANNOTATION_NAME_PATTERN.fullmatch(key):
        raise StepError(f"annotation name {key!r} isn't valid")
    return f"{prefix}/{key}"


def update_request(*, operation: str, target_version: str | None = None, server: str | None = None,
                   app_id: str | None = None, track: str | None = None, node_update_id: str,
                   operation_id: str) -> dict[str, str]:
    """An update-request annotation's value, checked as the agent checks it (protocol.rs,
    UpdateRequest::validate and the formal schema), so a mistake fails here rather than as an
    InvalidRequest status, or worse, an annotation the agent ignores without a status.

    stage and finalize need targetVersion, server, appId, and track; nodeUpdateId stays the same
    through one update's stage and finalize, and every operationId is new. rollback takes no
    targetVersion, and its server, appId, and track are only included if given.
    """
    if operation not in OPERATIONS:
        raise StepError(f"operation {operation!r} isn't one of {', '.join(OPERATIONS)}")
    for name, value in (("nodeUpdateId", node_update_id), ("operationId", operation_id)):
        if not UUID_PATTERN.fullmatch(value or ""):
            raise StepError(f"{name} {value!r} isn't a UUID")
    if operation in ("stage", "finalize"):
        missing = [name for name, value in (("targetVersion", target_version), ("server", server), ("appId", app_id),
                                            ("track", track)) if not value]
        if missing:
            raise StepError(f"a {operation} request needs {', '.join(missing)}")
    elif target_version:
        raise StepError("a rollback request can't name a targetVersion")
    if target_version and not is_semver(target_version):
        raise StepError(f"targetVersion {target_version!r} isn't semver, so it can never match the offered version")
    if server:
        parts = urllib.parse.urlsplit(server)
        if parts.scheme != "https" or not parts.hostname:
            raise StepError(f"server {server!r} isn't an https URL, which the agent requires")
    request = {"schemaVersion": SCHEMA_VERSION, "nodeUpdateId": node_update_id, "operationId": operation_id,
               "operation": operation}
    for name, value in (("targetVersion", target_version), ("server", server), ("appId", app_id), ("track", track)):
        if value:
            request[name] = value
    return request


def request_patch(prefix: str, request: dict[str, Any]) -> dict[str, Any]:
    """A JSON merge patch (kubectl patch --type merge) that writes the request annotation. The agent
    refuses a request with a field it doesn't know, and silently: it can't name the operationId."""
    unknown = sorted(set(request) - set(REQUEST_FIELDS))
    if unknown:
        raise StepError(f"the update request has fields the agent rejects: {', '.join(unknown)}")
    value = json.dumps(request, separators=(",", ":"))
    return {"metadata": {"annotations": {annotation_key(prefix, REQUEST_KEY): value}}}


def clear_patch(prefix: str, keys: list[str] | tuple[str, ...] = ANNOTATION_KEYS) -> dict[str, Any]:
    """A JSON merge patch that removes annotations, by name (update-status) or full key.

    The agent caches each operation's terminal status by operationId in its state.json, and
    publishes it again while the request is there, so removing a status alone doesn't stick."""
    return {"metadata": {"annotations": {annotation_key(prefix, key): None for key in keys}}}


@dataclasses.dataclass(frozen=True)
class Status:
    """An update-status or update-commit-status annotation. A commit status repeats the operationId
    of the finalize or rollback whose reboot it follows."""

    operation_id: str
    code: str
    message: str
    raw: dict[str, Any] = dataclasses.field(compare=False, hash=False)
    terminal: bool
    operation: str = ""
    node_update_id: str = ""


def parse_status(node: dict[str, Any], prefix: str, key: str = STATUS_KEY, *,
                 operation_id: str | None = None) -> Status | None:
    """The status annotation on a Node object (kubectl get node -o json), or None if there's none,
    or, given operation_id, none for that operation yet. A status that isn't the protocol's JSON
    is an error: the agent never writes one."""
    annotations = ((node or {}).get("metadata") or {}).get("annotations") or {}
    full_key = annotation_key(prefix, key)
    text = annotations.get(full_key)
    if text is None:
        return None
    try:
        raw = json.loads(text)
    except json.JSONDecodeError as error:
        raise StepError(f"{full_key} isn't JSON: {error}: {text[:200]}") from error
    if not isinstance(raw, dict):
        raise StepError(f"{full_key} isn't a JSON object: {text[:200]}")
    code = raw.get("code")
    status_operation_id = raw.get("operationId")
    if not isinstance(code, str) or not isinstance(status_operation_id, str):
        raise StepError(f"{full_key} has no code or operationId: {text[:200]}")
    if code not in STATUS_CODES:
        raise StepError(f"{full_key} has code {code!r}, which the protocol doesn't define")
    if operation_id is not None and status_operation_id != operation_id:
        return None
    return Status(status_operation_id, code, str(raw.get("message", "")), raw, code in TERMINAL_CODES,
                  str(raw.get("operation", "")), str(raw.get("nodeUpdateId", "")))
