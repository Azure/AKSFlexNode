"""Install the AKS Flex Node agent on the A/B update harness's VM, as containerd containers of its image.

The VM is Azure Container Linux, provisioned once, by the harness's own seed, before the agent is wanted.
So the agent's install, scripts/aks-flex-node-agent-container.bu, can't be its first-boot Ignition config.
Instead it's rendered with Butane, as an operator would, and the Ignition config Butane gives is applied to
the running VM, as Ignition would have applied it: its files, directories, and units, by a tar archive over
SSH, then relabelled, since SELinux is enforcing, and its units enabled. Then the bootstrap unit is started,
which it would have been at boot.

The agent image is built from this repository with rootless Podman and pushed to a plain-HTTP registry
that listens on the host's loopback only. The VM's QEMU user-mode NIC reaches that as 10.0.2.2.

The module also reads the agent's state back: findings for the bootstrap and the agent, a manifest of the
files, images, and machines that an OS update has to keep, and the kubelet's client certificate, for
trident-acl-agent's kubeconfig. It never logs or saves a secret: the bootstrap token, the rendered Ignition
config, which holds it, and the kubelet's key stay in memory or on the VM.

Commands run through the callables the caller passes in, so the module has no state of its own:
run(cmd, *, input, timeout, stream) on the host, ssh(command, *, input, timeout, check) and
ssh_bytes(command, data, *, timeout) on the VM, as user core with passwordless sudo, and log(message).
"""

from __future__ import annotations

import base64
import dataclasses
import gzip
import http.client
import io
import json
import os
import posixpath
import re
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import textwrap
import time
import urllib.parse
import urllib.request
from collections.abc import Callable
from pathlib import Path

from errors import StepError

Run = Callable[..., subprocess.CompletedProcess[str]]
Ssh = Callable[..., subprocess.CompletedProcess[str]]
SshBytes = Callable[..., subprocess.CompletedProcess[bytes]]
Log = Callable[[str], None]

# The agent's image and the install that runs it, in this repository.
DOCKERFILE = "Dockerfile.aks-flex-node-agent"
BUTANE_TEMPLATE = Path("scripts") / "aks-flex-node-agent-container.bu"
AGENT_REPOSITORY = "aks-flex-node-agent"
# Pinned by the digest of their indexes, as the agent image's own base images are.
BUTANE_IMAGE = "quay.io/coreos/butane:v0.29.0@sha256:13fec166cb47a8e053dcc23256c0ca41aaa1c61cab39793832aaf8894ca78c8f"
REGISTRY_IMAGE = "docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
# The host's loopback, as the VM's QEMU user-mode NIC reaches it.
GUEST_HOST = "10.0.2.2"

# The in-cluster machine endpoint, through the API server's service proxy, as hack/acl-container's.
PROXY_PATH = "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy"
# A cluster the in-cluster machine client never asks ARM about, though the config has to name one.
FAKE_CLUSTER_ID = ("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/acl-abupdate"
                   "/providers/Microsoft.ContainerService/managedClusters/acl-abupdate")
ZERO_ID = "00000000-0000-0000-0000-000000000000"
# kind's service CIDR is 10.96.0.0/16, and CoreDNS takes its tenth address. The agent's default,
# 10.0.0.10, is AKS's.
KIND_DNS_SERVICE_IP = "10.96.0.10"
# bootstrap.sh's format, which the agent validates too (pkg/config.BootstrapTokenPattern).
BOOTSTRAP_TOKEN = re.compile(r"[a-z0-9]{6}\.[a-z0-9]{16}")

# Must match the Butane config, and the units the agent installs (pkg/daemon/assets).
AGENT_UNIT = "aks-flex-node-agent.service"
BOOTSTRAP_UNIT = "aks-flex-node-bootstrap.service"
RESET_UNIT = "aks-flex-node-reset.service"
CTR_NAMESPACE = "aks-flex"
# The agent's container, which its unit runs.
AGENT_CONTAINER = "aks-flex-node"
SETTINGS = "/etc/sysconfig/aks-flex-node-container"
CONFIG_DIR = "/etc/aks-flex-node"
BOOTSTRAP_INPUTS = f"{CONFIG_DIR}/bootstrap.env"
# The Butane config's commented-out entry names this path, and the bootstrap unit removes it with
# bootstrap.env once bootstrap succeeds.
BASE_CONFIG = f"{CONFIG_DIR}/base-config.json"
# What the bootstrap unit's ExecStartPost removes.
BOOTSTRAP_INPUT_PATHS = (BOOTSTRAP_INPUTS, BASE_CONFIG, f"{CONFIG_DIR}/agent.kubeconfig",
                         f"{CONFIG_DIR}/kubelet.kubeconfig")
AGENT_IMAGE_SELECTION = f"{CONFIG_DIR}/agent-image.env"
# pkg/daemon/state.go: config.ConfigDir's daemon-state.json, whose activeMachine is kube1 or kube2.
DAEMON_STATE = f"{CONFIG_DIR}/daemon-state.json"
ACTIVE_MACHINE_FIELD = "activeMachine"
MACHINES = ("kube1", "kube2")
MACHINES_DIR = "/var/lib/machines"
# Inside the machine; kubelet points it, absolutely, at the certificate it rotated to last.
KUBELET_CLIENT_CERT = "/var/lib/kubelet/pki/kubelet-client-current.pem"
# The path hack/acl-abupdate/trident-acl-agent-override.conf gives the agent.
TRIDENT_KUBECONFIG = f"{CONFIG_DIR}/trident-acl-agent.kubeconfig"
TRIDENT_UNIT = "trident-acl-agent.service"

# An AVC denial that concerns the agent names its files, its binary, or the agent library's.
AGENT_SUBJECTS = ("aks-flex-node", "/opt/unbounded", "unbounded-agent", "aks-flex")
AVC_DENIED = re.compile(r"avc:\s+denied")
# Denials that are known and tracked, with the issue that tracks each, as hack/acl/acl.py's U10 has them.
KNOWN_DENIALS = (
    # ACL's policy denies kernel_t, which every host process is, `systemctl reset-failed`. The
    # binary install's recovery script runs it on the host. The container install runs it from the
    # agent's containers, as spc_t, which may, so this shows a container that isn't spc_t.
    (re.compile(r'function="bus_unit_method_reset_failed"'),
     "https://github.com/Azure/AKSFlexNode/issues/385"),
)

SYSTEMD_UNIT_DIR = "/etc/systemd/system"
_UNIT_NAME = re.compile(r"[A-Za-z0-9:_.\\@-]+\.(service|socket|target|timer|path|mount|automount|swap|slice|scope|device)")
_DROPIN_NAME = re.compile(r"[A-Za-z0-9:_.@-]+\.conf")
_DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
_REPOSITORY = re.compile(r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*")
_TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}")
_PRIVATE_KEY = re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|\Z)", re.S)


# ---------------------------------------------------------------------------------------------
# Helpers


def duration(seconds: float) -> str:
    minutes, seconds = divmod(round(seconds), 60)
    return f"{minutes}m{seconds:02d}s" if minutes else f"{seconds}s"


def last_lines(text: str | bytes | None, count: int = 12) -> str:
    if isinstance(text, bytes):
        text = text.decode(errors="replace")
    return "\n".join((text or "").strip().splitlines()[-count:])


def redact(text: str) -> str:
    """Hide what looks like a bootstrap token or a private key in text from the VM, such as a journal,
    before it goes into an error: the agent shouldn't log either, but an error mustn't depend on that."""
    return _PRIVATE_KEY.sub("[private key redacted]", BOOTSTRAP_TOKEN.sub("[token redacted]", text))


def parse_sections(text: str) -> dict[str, str]:
    """Split a probe script's output into its `### name` sections."""
    sections: dict[str, list[str]] = {}
    current: list[str] | None = None
    for line in text.splitlines():
        if line.startswith("### "):
            current = sections.setdefault(line[4:].strip(), [])
        elif current is not None:
            current.append(line)
    return {name: "\n".join(lines).strip("\n") for name, lines in sections.items()}


def parse_properties(text: str) -> dict[str, str]:
    """KEY=value lines, as `systemctl show` prints them."""
    return dict(line.split("=", 1) for line in text.splitlines() if "=" in line)


def _probe(ssh: Ssh, script: str, first_section: str, what: str) -> dict[str, str]:
    result = ssh(script, check=False, timeout=120)
    sections = parse_sections(result.stdout)
    if first_section not in sections:
        raise StepError(f"couldn't read {what} on the VM (ssh exited {result.returncode}): "
                        f"{redact(last_lines(result.stderr, 4))}")
    return sections


def _journal(ssh: Ssh, unit: str, count: int = 40) -> str:
    out = ssh(f"sudo journalctl -u {shlex.quote(unit)} -b --no-pager -o cat -n {count}", check=False).stdout
    return redact(last_lines(out, count))


# ---------------------------------------------------------------------------------------------
# The node's settings


@dataclasses.dataclass(frozen=True)
class FlexNode:
    """What the VM needs to join the cluster as a Flex node. agent_image is the reference the VM pulls,
    by digest. The bootstrap token is a secret, so it's left out of the repr."""

    node_name: str
    node_ip: str
    api_server: str
    ca_pem: str
    kubernetes_version: str
    bootstrap_token: str = dataclasses.field(repr=False)
    agent_image: str
    pull_args: str = "--plain-http"
    machine_proxy_path: str = PROXY_PATH
    # Not in the lead's contract; defaults that suit a kind cluster.
    dns_service_ip: str = KIND_DNS_SERVICE_IP
    agent_pool: str = "acl-abupdate"


def env_file_value(value: str) -> str:
    """Quote a value for a systemd environment file, as getting-started says to: single quotes keep
    it as written, and a value with a single quote in it goes in double quotes, with " and \\
    escaped."""
    if "'" not in value:
        return f"'{value}'"
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def _check_node(node: FlexNode) -> None:
    """Fail before rendering anything with settings bootstrap would reject. The token's value never
    goes into the message."""
    if not BOOTSTRAP_TOKEN.fullmatch(node.bootstrap_token):
        raise StepError("the bootstrap token isn't <6 of [a-z0-9]>.<16 of [a-z0-9]>")
    for name in ("node_name", "node_ip", "api_server", "ca_pem", "kubernetes_version", "agent_image"):
        if not getattr(node, name).strip():
            raise StepError(f"the Flex node's {name} is empty")
    if "-----BEGIN CERTIFICATE-----" not in node.ca_pem:
        raise StepError("the cluster CA isn't a PEM certificate")
    for name in ("agent_image", "pull_args"):
        if any(ch in getattr(node, name) for ch in "\n\r\0"):
            raise StepError(f"the Flex node's {name} has a line break in it")


def base_config(node: FlexNode) -> dict:
    """The agent's base config, which bootstrap.sh renders its config from: a bootstrap-token node that
    reads its machine from the in-cluster controller, as hack/acl-container's. The kubelet advertises
    the VM's address on the cluster's bridge, rather than the user-mode NIC's 10.0.2.15, which the
    API server can't reach, and the node is named explicitly rather than after the hostname."""
    _check_node(node)
    return {
        "azure": {
            "subscriptionId": ZERO_ID,
            "tenantId": ZERO_ID,
            "resourceManagerEndpoint": "https://management.azure.com",
            "targetAgentPoolName": node.agent_pool,
            "bootstrapToken": {"token": node.bootstrap_token},
            "arc": {"enabled": False},
            "targetCluster": {"resourceId": FAKE_CLUSTER_ID, "location": "local"},
        },
        "agent": {
            "logLevel": "info",
            "logDir": "/var/log/aks-flex-node",
            "nodeName": node.node_name,
            "machineClient": {"mode": "in-cluster", "endpointUrl": node.machine_proxy_path},
            "requireMachineRegistration": True,
        },
        "components": {"kubernetes": node.kubernetes_version},
        "networking": {"dnsServiceIP": node.dns_service_ip},
        "node": {"kubelet": {
            "clusterFQDN": node.api_server,
            # Base64 of the PEM, as AKS's bootstrap data gives it.
            "caCertData": base64.b64encode(node.ca_pem.encode()).decode(),
            "nodeIP": node.node_ip,
        }},
    }


def container_env(node: FlexNode) -> str:
    """agent-container.env, the install's settings, which the units read as an EnvironmentFile: the
    image, and the flags to pull it with. The flags are on ctr's command line, so never credentials."""
    _check_node(node)
    lines = [f"AKS_FLEX_NODE_IMAGE={env_file_value(node.agent_image)}"]
    if node.pull_args.strip():
        lines.append(f"AKS_FLEX_NODE_PULL_ARGS={env_file_value(node.pull_args.strip())}")
    return "\n".join(lines) + "\n"


def bootstrap_env() -> str:
    """bootstrap.env, bootstrap's inputs: only the base config's path, since the token is in it. The
    image's aks-flex-node-bootstrap reads values as written, so they aren't quoted."""
    return f"AKS_FLEX_NODE_BASE_CONFIG_FILE={BASE_CONFIG}\n"


# ---------------------------------------------------------------------------------------------
# The agent image and its registry


def build_agent_image(repo: Path, tag: str, *, run: Run, log: Log) -> None:
    """Build the agent's image from this repository with rootless Podman, as a release builds it, with
    the commit it's from."""
    head = run(["git", "-C", str(repo), "rev-parse", "--short", "HEAD"])
    commit = head.stdout.strip() if head.returncode == 0 and head.stdout else "unknown"
    if commit != "unknown" and run(["git", "-C", str(repo), "diff", "--quiet", "HEAD"]).returncode != 0:
        commit += "-dirty"
    log(f"building {tag} from {DOCKERFILE} with rootless Podman, at {commit}")
    started = time.monotonic()
    result = run(["podman", "build", "-f", str(repo / DOCKERFILE), "--build-arg", f"GIT_COMMIT={commit}",
                  "-t", tag, str(repo)], timeout=3600, stream=True)
    if result.returncode != 0:
        raise StepError(f"podman build of {tag} exited {result.returncode}; its output is above")
    log(f"built {tag} in {duration(time.monotonic() - started)}")


class Registry:
    """A plain-HTTP registry in a rootless Podman container, published on the host's loopback only:
    Podman pushes to it there, and the VM pulls from it as guest_host, which QEMU's user-mode NIC
    maps to the host's loopback. Nothing else can reach it."""

    def __init__(self, name: str, port: int, *, guest_host: str = GUEST_HOST, run: Run, log: Log,
                 image: str = REGISTRY_IMAGE) -> None:
        self.name = name
        self.port = port
        self.guest_host = guest_host
        self.image = image
        self.run = run
        self.log = log

    @property
    def address(self) -> str:
        """Where the host reaches it."""
        return f"127.0.0.1:{self.port}"

    def running(self) -> bool:
        result = self.run(["podman", "container", "inspect", "--format", "{{.State.Running}}", self.name])
        return result.returncode == 0 and result.stdout.strip() == "true"

    def answers(self) -> bool:
        try:
            with urllib.request.urlopen(f"http://{self.address}/v2/", timeout=5) as response:
                return response.status == 200
        except (OSError, ValueError, http.client.HTTPException):
            return False

    def start(self, timeout: float = 60) -> None:
        if self.running():
            return
        # One that exited, from an earlier run, keeps the name.
        self.run(["podman", "rm", "--force", "--ignore", self.name])
        self.log(f"starting registry {self.name} on {self.address}, which the VM reaches as "
                 f"{self.guest_host}:{self.port}")
        result = self.run(["podman", "run", "--detach", "--name", self.name,
                           "--publish", f"{self.address}:5000", self.image], timeout=600)
        if result.returncode != 0:
            raise StepError(f"starting registry {self.name} failed: {last_lines(result.stderr, 6)}")
        deadline = time.monotonic() + timeout
        while not self.answers():
            if time.monotonic() >= deadline:
                raise StepError(f"registry {self.name} doesn't answer on http://{self.address}/v2/ "
                                f"after {duration(timeout)}")
            time.sleep(0.5)

    def stop(self) -> None:
        """Remove the registry's container, and with it what was pushed. Best effort."""
        self.run(["podman", "rm", "--force", "--ignore", self.name])

    def push(self, local_tag: str, repository: str, tag: str) -> str:
        """Push a local image as repository:tag, and return the reference the VM pulls it by, pinned
        by the digest the registry has it as."""
        if not _REPOSITORY.fullmatch(repository) or not _TAG.fullmatch(tag):
            raise StepError(f"{repository}:{tag} isn't a repository and tag")
        self.start()
        destination = f"{self.address}/{repository}:{tag}"
        with tempfile.TemporaryDirectory(prefix="flexinstall-push-") as directory:
            digest_file = Path(directory) / "digest"
            result = self.run(["podman", "push", "--quiet", "--tls-verify=false", "--digestfile",
                               str(digest_file), local_tag, destination], timeout=1800)
            if result.returncode != 0:
                raise StepError(f"pushing {local_tag} to {destination} failed: {last_lines(result.stderr, 6)}")
            try:
                digest = digest_file.read_text().strip()
            except OSError as error:
                raise StepError(f"podman push wrote no digest for {destination}: {error}") from error
        if not _DIGEST.fullmatch(digest):
            raise StepError(f"podman push gave {digest!r} as {destination}'s digest")
        reference = f"{self.guest_host}:{self.port}/{repository}@{digest}"
        self.log(f"pushed {local_tag} as {destination}; the VM pulls {reference}")
        return reference


# ---------------------------------------------------------------------------------------------
# Butane and Ignition


def render_butane(template: Path, files: dict[str, str | bytes], *, run: Run,
                  butane_image: str = BUTANE_IMAGE, workdir: Path | None = None) -> dict:
    """Render a Butane config, as an operator does, from the files it names, and return the Ignition
    config. Butane runs from PATH if it's installed, otherwise from its image in rootless Podman.

    The files can hold secrets, so they're written to a private directory, 0700 with files 0600,
    which is removed whatever happens. The Ignition config is returned, never written."""
    for name in files:
        if not name or "/" in name or name in (".", ".."):
            raise StepError(f"{name!r} isn't a file name for Butane's files directory")
    try:
        source = template.read_text()
    except OSError as error:
        raise StepError(f"can't read the Butane config {template}: {error}") from error
    directory = Path(tempfile.mkdtemp(prefix="flexinstall-butane-", dir=workdir))
    try:
        directory.chmod(0o700)
        for name, content in files.items():
            data = content.encode() if isinstance(content, str) else content
            fd = os.open(directory / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "wb") as file:
                file.write(data)
        butane = shutil.which("butane")
        if butane:
            cmd = [butane, "--strict", "--files-dir", str(directory)]
        else:
            # z: on a host with SELinux, the container may read the directory.
            cmd = ["podman", "run", "--rm", "-i", "-v", f"{directory}:/files:ro,z", butane_image,
                   "--strict", "--files-dir", "/files"]
        result = run(cmd, input=source, timeout=600)
        if result.returncode != 0:
            raise StepError(f"Butane couldn't render {template.name} (exit {result.returncode}): "
                            f"{redact(last_lines(result.stderr, 8))}")
        try:
            config = json.loads(result.stdout)
        except json.JSONDecodeError as error:
            raise StepError(f"Butane's output for {template.name} isn't JSON: {error}") from None
    finally:
        shutil.rmtree(directory, ignore_errors=True)
    if not isinstance(config, dict):
        raise StepError(f"Butane's output for {template.name} isn't an Ignition config")
    return config


@dataclasses.dataclass
class IgnitionPayload:
    """An Ignition config as what applies it to a running host: a tar archive of its files,
    directories, and units, to extract at /, and the units to enable and mask. paths are what the
    archive writes, and the directories it may create, for restorecon. disable and create_only, which
    the contract doesn't name, are units Ignition would disable, and files it mustn't overwrite."""

    tar: bytes
    enable: list[str]
    mask: list[str]
    paths: list[str]
    disable: list[str] = dataclasses.field(default_factory=list)
    create_only: list[str] = dataclasses.field(default_factory=list)


@dataclasses.dataclass(frozen=True)
class _Node:
    path: str
    mode: int
    data: bytes | None  # None for a directory


def _only(obj: object, where: str, allowed: set[str]) -> dict:
    """obj as a mapping with only the keys this module applies: anything else Ignition would do, it
    can't, so it refuses rather than apply part of a config."""
    if not isinstance(obj, dict):
        raise StepError(f"{where} isn't an object")
    unsupported = sorted(key for key, value in obj.items() if key not in allowed and value not in (None, [], {}))
    if unsupported:
        raise StepError(f"{where} has {', '.join(unsupported)}, which applying Ignition here doesn't support")
    return obj


def _list(obj: dict, key: str, where: str) -> list:
    value = obj.get(key)
    if value is None:
        return []
    if not isinstance(value, list):
        raise StepError(f"{where}.{key} isn't a list")
    return value


def _absolute(path: object, where: str) -> str:
    if (not isinstance(path, str) or not path.startswith("/") or path.startswith("//") or path == "/"
            or posixpath.normpath(path) != path or any(ch in path for ch in "\n\r\0")):
        raise StepError(f"{where}'s path {path!r} isn't a clean absolute path")
    return path


def _mode(value: object, default: int, where: str) -> int:
    if value is None:
        return default
    if isinstance(value, bool) or not isinstance(value, int) or not 0 <= value <= 0o7777:
        raise StepError(f"{where}'s mode {value!r} isn't a file mode")
    return value


def data_url_contents(source: str, where: str) -> bytes:
    """The bytes of a data: URL, base64 or percent-encoded. Errors never quote it: it may hold a secret."""
    if not source.startswith("data:"):
        scheme = source.split(":", 1)[0] if ":" in source else "a relative"
        raise StepError(f"{where}'s contents come from {scheme} URL; only data: URLs are supported")
    header, separator, payload = source[len("data:"):].partition(",")
    if not separator:
        raise StepError(f"{where}'s data: URL has no comma")
    if header.split(";")[-1].strip().lower() == "base64":
        try:
            return base64.b64decode(urllib.parse.unquote_to_bytes(payload), validate=True)
        except ValueError:
            raise StepError(f"{where}'s data: URL isn't valid base64") from None
    return urllib.parse.unquote_to_bytes(payload)


def _file_contents(contents: object, where: str) -> bytes:
    if contents is None:
        return b""
    contents = _only(contents, f"{where}.contents", {"source", "compression"})
    source = contents.get("source")
    data = b"" if source is None else data_url_contents(str(source), where)
    compression = contents.get("compression") or ""
    if compression == "gzip":
        try:
            return gzip.decompress(data)
        except (OSError, EOFError):
            raise StepError(f"{where}'s gzip contents don't decompress") from None
    if compression:
        raise StepError(f"{where}'s compression {compression!r} isn't supported")
    return data


def _ancestors(path: str) -> list[str]:
    """path's directories below the top level, which extracting it may create: /etc/a/b gives /etc/a."""
    parts = path.strip("/").split("/")
    return ["/" + "/".join(parts[:depth]) for depth in range(2, len(parts))]


def _tar(nodes: list[_Node]) -> bytes:
    buffer = io.BytesIO()
    mtime = int(time.time())
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.PAX_FORMAT) as tar:
        for node in nodes:
            info = tarfile.TarInfo(node.path.lstrip("/"))
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            info.mtime = mtime
            info.mode = node.mode
            if node.data is None:
                info.type = tarfile.DIRTYPE
                tar.addfile(info)
            else:
                info.size = len(node.data)
                tar.addfile(info, io.BytesIO(node.data))
    return buffer.getvalue()


def ignition_payload(config: dict, *, extra_files: dict[str, tuple[int, bytes]] | None = None) -> IgnitionPayload:
    """What applying an Ignition config to a running host takes, for the part of Ignition that Butane
    configs like the agent's use: storage.directories, storage.files from data: URLs, and
    systemd.units with drop-ins. Anything else, it refuses. extra_files are files to write too, by
    path, as (mode, contents), such as a base config the Butane config only names.

    Ignition's defaults apply: files 0644, directories 0755, units and drop-ins 0644, all root's.
    Units go to /etc/systemd/system, and drop-ins to its NAME.d. Directories the archive doesn't
    hold, it creates 0755."""
    config = _only(config, "the Ignition config", {"ignition", "storage", "systemd"})
    ignition = _only(config.get("ignition") or {}, "ignition", {"version"})
    version = str(ignition.get("version", ""))
    if not version.startswith("3."):
        raise StepError(f"the Ignition config's version is {version or 'missing'}, not 3.x")

    nodes: dict[str, _Node] = {}
    create_only: list[str] = []

    def add(node: _Node, where: str) -> None:
        if node.path in nodes:
            raise StepError(f"{where} writes {node.path}, which the config writes already")
        nodes[node.path] = node

    storage = _only(config.get("storage") or {}, "storage", {"directories", "files"})
    for index, entry in enumerate(_list(storage, "directories", "storage")):
        where = f"storage.directories[{index}]"
        entry = _only(entry, where, {"path", "mode"})
        add(_Node(_absolute(entry.get("path"), where), _mode(entry.get("mode"), 0o755, where), None), where)
    for index, entry in enumerate(_list(storage, "files", "storage")):
        where = f"storage.files[{index}]"
        entry = _only(entry, where, {"path", "mode", "overwrite", "contents"})
        path = _absolute(entry.get("path"), where)
        where = f"{where} ({path})"
        overwrite = entry.get("overwrite")
        if overwrite not in (None, True, False):
            raise StepError(f"{where}'s overwrite isn't a boolean")
        if not overwrite:
            create_only.append(path)
        add(_Node(path, _mode(entry.get("mode"), 0o644, where), _file_contents(entry.get("contents"), where)), where)
    for path, (mode, data) in sorted((extra_files or {}).items()):
        where = f"extra file {path}"
        add(_Node(_absolute(path, where), _mode(mode, 0o644, where), bytes(data)), where)

    enable: list[str] = []
    disable: list[str] = []
    mask: list[str] = []
    systemd = _only(config.get("systemd") or {}, "systemd", {"units"})
    for index, unit in enumerate(_list(systemd, "units", "systemd")):
        where = f"systemd.units[{index}]"
        unit = _only(unit, where, {"name", "enabled", "mask", "contents", "dropins"})
        name = unit.get("name")
        if not isinstance(name, str) or not _UNIT_NAME.fullmatch(name):
            raise StepError(f"{where}'s name {name!r} isn't a unit name")
        where = f"unit {name}"
        contents = unit.get("contents")
        if unit.get("mask"):
            if contents is not None:
                raise StepError(f"{where} is masked and has contents")
            mask.append(name)
        if contents is not None:
            if not isinstance(contents, str):
                raise StepError(f"{where}'s contents aren't text")
            add(_Node(f"{SYSTEMD_UNIT_DIR}/{name}", 0o644, contents.encode()), where)
        for dropin_index, dropin in enumerate(_list(unit, "dropins", where)):
            dropin_where = f"{where}'s dropins[{dropin_index}]"
            dropin = _only(dropin, dropin_where, {"name", "contents"})
            dropin_name = dropin.get("name")
            if not isinstance(dropin_name, str) or not _DROPIN_NAME.fullmatch(dropin_name):
                raise StepError(f"{dropin_where}'s name {dropin_name!r} isn't a drop-in name")
            if not isinstance(dropin.get("contents"), str):
                raise StepError(f"{dropin_where} has no contents")
            add(_Node(f"{SYSTEMD_UNIT_DIR}/{name}.d/{dropin_name}", 0o644, dropin["contents"].encode()), dropin_where)
        enabled = unit.get("enabled")
        if enabled is True:
            enable.append(name)
        elif enabled is False:
            disable.append(name)
        elif enabled is not None:
            raise StepError(f"{where}'s enabled isn't a boolean")

    # Directories first, each before what's in it.
    ordered = sorted(nodes.values(), key=lambda node: (node.data is not None, node.path))
    paths = sorted({path for node in ordered for path in (node.path, *_ancestors(node.path))})
    return IgnitionPayload(tar=_tar(ordered), enable=enable, mask=mask, paths=paths, disable=disable,
                           create_only=sorted(create_only))


def apply_command(payload: IgnitionPayload) -> str:
    """The command that applies a payload on the VM, with payload.tar on its stdin, as Ignition would
    have: files Ignition wouldn't overwrite must not exist; the archive is extracted as root's, with
    its modes, leaving the modes of directories that exist alone; what it wrote is relabelled, since
    SELinux is enforcing and tar writes no labels; and systemd loads the units and enables and masks
    them. Only tar reads stdin."""
    quote = shlex.quote
    lines = ["set -eu"]
    for path in payload.create_only:
        lines.append(f"if sudo test -e {quote(path)} || sudo test -L {quote(path)}; then "
                     f"printf 'flexinstall: %s exists, and the config does not overwrite it\\n' {quote(path)} >&2; "
                     "exit 1; fi")
    lines.append("sudo tar --extract --file=- --directory=/ --preserve-permissions --numeric-owner "
                 "--no-overwrite-dir")
    if payload.paths:
        lines.append("sudo restorecon -F -- " + " ".join(quote(path) for path in payload.paths))
    lines.append("sudo systemctl daemon-reload")
    for verb, units in (("enable", payload.enable), ("disable", payload.disable), ("mask", payload.mask)):
        if units:
            lines.append(f"sudo systemctl {verb} -- " + " ".join(quote(unit) for unit in units))
    return "\n".join(lines) + "\n"


def _require_install(payload: IgnitionPayload) -> None:
    """The Butane config still installs what provision relies on."""
    written = set(payload.paths)
    missing = [path for path in (SETTINGS, BOOTSTRAP_INPUTS, BASE_CONFIG, f"{SYSTEMD_UNIT_DIR}/{BOOTSTRAP_UNIT}",
                                 f"{SYSTEMD_UNIT_DIR}/{RESET_UNIT}") if path not in written]
    if missing or BOOTSTRAP_UNIT not in payload.enable:
        raise StepError("the agent's Butane config no longer installs what this harness expects: "
                        + ", ".join(missing or [f"{BOOTSTRAP_UNIT} enabled"]))


def agent_installed(ssh: Ssh) -> bool:
    """Whether bootstrap has installed the agent's unit, which keeps the bootstrap unit from running."""
    return ssh(f"test -e {SYSTEMD_UNIT_DIR}/{AGENT_UNIT} || test -L {SYSTEMD_UNIT_DIR}/multi-user.target.wants/{AGENT_UNIT}",
               check=False).returncode == 0


def provision(node: FlexNode, *, repo: Path, ssh: Ssh, ssh_bytes: SshBytes, run: Run, log: Log,
              butane_image: str = BUTANE_IMAGE) -> None:
    """Install the agent on the VM from the container Butane config, and start its bootstrap, as its
    first boot would have: render the config with the install's settings and bootstrap's inputs, add
    the base config bootstrap.env names, 0600, as the config's commented-out entry would, apply it,
    and start the bootstrap unit, which pulls the image and joins the node. It doesn't wait for that;
    see wait_for_bootstrap.

    A VM with the agent installed is refused: the bootstrap unit wouldn't run, and would leave the new
    inputs, with the token, in place. Reset the node first."""
    if agent_installed(ssh):
        raise StepError(f"the agent is installed on the VM already ({AGENT_UNIT} exists), so its bootstrap "
                        f"unit wouldn't run; reset the node first, with {RESET_UNIT}")
    template = repo / BUTANE_TEMPLATE
    log(f"rendering {BUTANE_TEMPLATE} for node {node.node_name} at {node.node_ip}, with {node.agent_image}")
    config = render_butane(template, {"agent-container.env": container_env(node), "bootstrap.env": bootstrap_env()},
                           run=run, butane_image=butane_image)
    base = json.dumps(base_config(node), indent=2) + "\n"
    payload = ignition_payload(config, extra_files={BASE_CONFIG: (0o600, base.encode())})
    _require_install(payload)
    log(f"applying the install to the VM: {len(payload.paths)} paths, enabling {', '.join(payload.enable)}")
    result = ssh_bytes(apply_command(payload), payload.tar, timeout=300)
    if result.returncode != 0:
        raise StepError(f"applying the agent's install on the VM exited {result.returncode}: "
                        f"{redact(last_lines(result.stderr, 8))}")
    ssh(f"sudo systemctl start --no-block {BOOTSTRAP_UNIT}")
    log(f"started {BOOTSTRAP_UNIT}")


# ---------------------------------------------------------------------------------------------
# Bootstrap and the agent


_BOOTSTRAP_PROBE = textwrap.dedent(f"""\
    echo '### bootstrap'
    systemctl show {BOOTSTRAP_UNIT} -p ActiveState -p SubState -p Result -p ExecMainStatus \\
      -p ExecMainExitTimestampMonotonic -p ConditionResult -p NRestarts
    echo '### agent'
    echo "is-enabled=$(systemctl is-enabled {AGENT_UNIT} 2>/dev/null)"
    echo "is-active=$(systemctl is-active {AGENT_UNIT} 2>/dev/null)"
    echo "installed=$(test -e {SYSTEMD_UNIT_DIR}/{AGENT_UNIT} && echo yes || echo no)"
    echo '### inputs'
    for path in {' '.join(BOOTSTRAP_INPUT_PATHS)}; do
      if sudo test -e "$path"; then echo "$path"; fi
    done
    true
    """)


def _bootstrap_state(bootstrap: dict[str, str]) -> str:
    return (f"it's {bootstrap.get('ActiveState', '?')}/{bootstrap.get('SubState', '?')}, result "
            f"{bootstrap.get('Result', '?')}, exit status {bootstrap.get('ExecMainStatus', '?')}, after "
            f"{bootstrap.get('NRestarts', '?')} restart(s)")


def bootstrap_findings_from(sections: dict[str, str]) -> list[tuple[bool, str, str]]:
    """The findings of bootstrap_findings, from its probe's sections."""
    bootstrap = parse_properties(sections.get("bootstrap", ""))
    agent = parse_properties(sections.get("agent", ""))
    ran = bootstrap.get("ExecMainExitTimestampMonotonic", "0") not in ("", "0")
    if ran:
        ok = (bootstrap.get("ActiveState") == "inactive" and bootstrap.get("Result") == "success"
              and bootstrap.get("ExecMainStatus") == "0")
        detail = _bootstrap_state(bootstrap)
    else:
        # A boot after the one it bootstrapped skips it, as the agent's unit is enabled.
        ok = bootstrap.get("ConditionResult") == "no" and agent.get("is-enabled") == "enabled"
        detail = "it hasn't run this boot" if not ok else ""
        if not ok and bootstrap.get("ActiveState") not in (None, "inactive"):
            detail = _bootstrap_state(bootstrap)
    findings = [(ok, f"{BOOTSTRAP_UNIT} succeeded", detail)]
    enabled, active = agent.get("is-enabled", ""), agent.get("is-active", "")
    findings.append((enabled == "enabled" and active == "active", f"{AGENT_UNIT} is enabled and active",
                     f"it's {enabled or 'not installed'} and {active or 'unknown'}"))
    left = sections.get("inputs", "").split()
    findings.append((not left, "bootstrap removed its inputs, with the bootstrap token", ", ".join(left)))
    return findings


def bootstrap_findings(ssh: Ssh) -> list[tuple[bool, str, str]]:
    """Whether bootstrap did what it's for: its unit succeeded, it installed, enabled, and started the
    agent's unit, and its inputs, with the token, are gone. Each finding is (ok, description, detail)."""
    return bootstrap_findings_from(_probe(ssh, _BOOTSTRAP_PROBE, "bootstrap", "the bootstrap's state"))


def wait_for_bootstrap(ssh: Ssh, *, timeout: float, log: Log, interval: float = 10) -> None:
    """Wait for the bootstrap unit to install the agent: it pulls the image and runs bootstrap.sh, whose
    start installs and starts the agent's unit. A failure it retries is logged; one it doesn't, exit
    status 78, fails at once."""
    log(f"waiting for {BOOTSTRAP_UNIT} to bootstrap the node")
    started = time.monotonic()
    restarts = 0
    bootstrap: dict[str, str] = {}
    while True:
        sections = _probe(ssh, _BOOTSTRAP_PROBE, "bootstrap", "the bootstrap's state")
        bootstrap = parse_properties(sections.get("bootstrap", ""))
        agent = parse_properties(sections.get("agent", ""))
        state = bootstrap.get("ActiveState", "")
        if state == "failed":
            raise StepError(f"{BOOTSTRAP_UNIT} failed: {_bootstrap_state(bootstrap)}\n{_journal(ssh, BOOTSTRAP_UNIT)}")
        if state == "inactive" and bootstrap.get("Result") == "success" and agent.get("installed") == "yes":
            log(f"bootstrap installed {AGENT_UNIT} in {duration(time.monotonic() - started)}")
            return
        count = int(bootstrap.get("NRestarts", "0") or 0)
        if count > restarts:
            restarts = count
            log(f"bootstrap failed, and {BOOTSTRAP_UNIT} retries it ({count} restart(s) so far)")
        if time.monotonic() - started >= timeout:
            break
        time.sleep(interval)
    skipped = (state == "inactive" and bootstrap.get("ConditionResult") == "no"
               and bootstrap.get("ExecMainExitTimestampMonotonic", "0") in ("", "0"))
    why = f"; it was skipped: is {BOOTSTRAP_INPUTS} there?" if skipped else ""
    raise StepError(f"timed out after {duration(timeout)} waiting for {BOOTSTRAP_UNIT}: "
                    f"{_bootstrap_state(bootstrap)}{why}\n{_journal(ssh, BOOTSTRAP_UNIT)}")


def _agent_ready_probe(since: str = "") -> str:
    denied = ""
    if since:
        denied = (f"echo \"denied=$(sudo journalctl -u {AGENT_UNIT} --since @{shlex.quote(since)} --no-pager -o cat "
                  "| grep -m1 'machinectl failed')\"\n")
    return textwrap.dedent(f"""\
        echo '### agent'
        echo "status=$(sudo ctr --namespace {CTR_NAMESPACE} task ls 2>/dev/null | awk '$1 == "{AGENT_CONTAINER}" {{print $3}}')"
        echo "state=$(systemctl show {AGENT_UNIT} -p ActiveState --value)"
        id=$(systemctl show {AGENT_UNIT} -p InvocationID --value)
        if [ -n "$id" ] && sudo journalctl -b --no-pager -o cat "_SYSTEMD_INVOCATION_ID=$id" \\
            | grep -q 'agent container ready'; then
          echo ready=yes
        else
          echo ready=no
        fi
        """) + denied


def _agent_readiness(ssh: Ssh, since: str = "") -> dict[str, str]:
    return parse_properties(_probe(ssh, _agent_ready_probe(since), "agent", "the agent's state")["agent"])


def agent_ready(ssh: Ssh) -> bool:
    """Whether the agent's container runs, and its entrypoint has logged its checks, as harness.py's
    agent_ready: only this start of the unit counts, since the journal keeps the ones before it."""
    readiness = _agent_readiness(ssh)
    return readiness.get("status") == "RUNNING" and readiness.get("ready") == "yes"


def wait_for_agent(ssh: Ssh, *, timeout: float, log: Log, interval: float = 5) -> None:
    """Wait for the agent's container to run and its entrypoint to log that it's ready. The entrypoint
    exits when the host's machinectl fails, and systemd restarts it, so an SELinux denial would only
    show as a timeout; it fails at once instead."""
    log(f"waiting for the agent's container ({AGENT_UNIT})")
    since = ssh("date +%s", check=False).stdout.strip()
    since = since if since.isdigit() else ""
    started = time.monotonic()
    while True:
        readiness = _agent_readiness(ssh, since)
        if readiness.get("status") == "RUNNING" and readiness.get("ready") == "yes":
            log(f"the agent's container is running, after {duration(time.monotonic() - started)}")
            return
        if "Access denied" in readiness.get("denied", ""):
            raise StepError(f"the agent's container can't reach the host's systemd over D-Bus: "
                            f"{redact(readiness['denied'])}\nACL's SELinux policy needs `allow spc_t kernel_t:dbus send_msg;`, "
                            "and containerd's snapshots labelled container_ro_file_t, from ACL 3.0.20261026; "
                            "see hack/acl-container/DESIGN.md#selinux")
        if readiness.get("state") == "failed":
            raise StepError(f"{AGENT_UNIT} failed\n{_journal(ssh, AGENT_UNIT)}")
        if time.monotonic() - started >= timeout:
            break
        time.sleep(interval)
    raise StepError(f"timed out after {duration(timeout)} waiting for the agent's container: its task is "
                    f"{readiness.get('status') or 'absent'}, its unit {readiness.get('state') or 'unknown'}\n"
                    f"{_journal(ssh, AGENT_UNIT)}")


_AGENT_PROBE = textwrap.dedent(f"""\
    echo '### tasks'
    sudo ctr --namespace {CTR_NAMESPACE} task ls 2>&1
    echo '### context'
    pid=$(sudo ctr --namespace {CTR_NAMESPACE} task ls 2>/dev/null | awk '$1 == "{AGENT_CONTAINER}" {{print $2}}')
    if [ -n "$pid" ]; then sudo cat "/proc/$pid/attr/current" 2>&1 | tr -d '\\0'; echo; fi
    echo '### unit'
    echo "is-active=$(systemctl is-active {AGENT_UNIT} 2>/dev/null)"
    echo '### state'
    sudo jq -c . {DAEMON_STATE} 2>/dev/null
    echo '### machines'
    for machine in {' '.join(MACHINES)}; do
      echo "$machine $(machinectl show "$machine" -p State --value 2>/dev/null)"
    done
    """)


def task_status(tasks: str, container: str = AGENT_CONTAINER) -> tuple[str, str]:
    """A container's task's PID and status, from `ctr task ls`, or ("", "") if it has none."""
    for line in tasks.splitlines()[1:]:
        fields = line.split()
        if len(fields) >= 3 and fields[0] == container:
            return fields[1], fields[-1]
    return "", ""


def _daemon_state_json(text: str) -> dict:
    try:
        state = json.loads(text)
    except json.JSONDecodeError:
        return {}
    return state if isinstance(state, dict) else {}


def agent_findings_from(sections: dict[str, str]) -> list[tuple[bool, str, str]]:
    """The findings of agent_findings, from its probe's sections."""
    _, status = task_status(sections.get("tasks", ""))
    findings = [(status == "RUNNING", "the agent's container task is RUNNING", f"it's {status or 'absent'}")]
    context = sections.get("context", "").strip()
    fields = context.split(":")
    findings.append((len(fields) >= 3 and fields[2] == "spc_t", "the agent's container runs as SELinux type spc_t",
                     f"its context is {context or 'unknown'}"))
    active = parse_properties(sections.get("unit", "")).get("is-active", "")
    findings.append((active == "active", f"{AGENT_UNIT} is active", f"it's {active or 'unknown'}"))
    machine = str(_daemon_state_json(sections.get("state", "")).get(ACTIVE_MACHINE_FIELD, ""))
    states = dict((line.split(" ", 1) + [""])[:2] for line in sections.get("machines", "").splitlines() if line.strip())
    state = states.get(machine, "").strip()
    findings.append((bool(machine) and state == "running",
                     f"the agent's active machine, {machine or 'none'}, is running",
                     f"machined has it {state}" if state else f"machined has no {machine}" if machine
                     else f"{DAEMON_STATE} names none"))
    return findings


def agent_findings(ssh: Ssh) -> list[tuple[bool, str, str]]:
    """Whether the agent runs as the container install runs it: its task RUNNING, as spc_t, which ACL's
    container rules apply to, its unit active, and its active machine running in machined."""
    return agent_findings_from(_probe(ssh, _AGENT_PROBE, "tasks", "the agent's state"))


def daemon_state(ssh: Ssh) -> dict:
    """The agent's daemon state, with its active machine as activeMachine, or {} if there's none."""
    result = ssh(f"sudo cat {DAEMON_STATE}", check=False)
    return _daemon_state_json(result.stdout) if result.returncode == 0 else {}


# ---------------------------------------------------------------------------------------------
# The agent's state, before and after an OS update

# Run on the VM, as core or root, it prints the agent's state that an OS update has to keep, sorted:
# "<sha256>  <path>" for each file, a symlink's being of its target, never a file's contents; then
# "image <ref> <digest>" for each image in the agent's containerd namespace; "active <machine>", from
# the daemon state; and "machine <name> <state>" for each machine, as machined has it, or stopped.
# Where the paths come from:
#   /etc/aks-flex-node, /etc/sysconfig/aks-flex-node-container: the install and the agent's config,
#     state, credentials, and image selection (pkg/config, pkg/daemon)
#   /etc/unbounded: the agent library's applied configs and nftables rules (goalstates.ConfigDir,
#     AgentConfigDir)
#   /etc/systemd/system: the agent's units (pkg/daemon/assets), the bootstrap and reset units, the
#     machines' config regeneration units and nspawn drop-ins, nftables-flush, LocalDNS's network
#     unit, the host routing units, and the links enabling them (goalstates, pkg/hostrouting)
#   /etc/systemd/nspawn/NAME.nspawn: the machines' settings (goalstates.SystemdNSpawnDir)
#   /etc/sysctl.d/99-kubernetes.conf: the host's sysctls (phases/host/os.go). The agent library
#     writes its modules-load file in the machine, not the host, but one on the host would count.
STATE_MANIFEST_SCRIPT = textwrap.dedent(f"""\
    manifest() {{
      export LC_ALL=C
      {{
        find {CONFIG_DIR} /etc/unbounded -xdev \\( -type f -o -type l \\) -print 2>/dev/null
        find {SYSTEMD_UNIT_DIR} /etc/systemd/nspawn \\( -type f -o -type l \\) \\( \\
          -name 'aks-flex-node-*' -o -path '*/aks-flex-node-*.d/*' -o -name 'unbounded-*' \\
          -o -name 'systemd-nspawn@*' -o -path '*/systemd-nspawn@*.service.d/*' -o -name '*.nspawn' \\
          -o -name nftables-flush.service -o -name static-routes.service -o -name check-route-overlap.service \\
          \\) -print 2>/dev/null
        find /etc/sysctl.d /etc/modules-load.d \\( -type f -o -type l \\) \\( \\
          -name '*kubernetes*' -o -name '*aks-flex*' -o -name '*unbounded*' \\) -print 2>/dev/null
        if [ -e {SETTINGS} ]; then echo {SETTINGS}; fi
      }} | sort -u | while IFS= read -r path; do
        if [ -L "$path" ]; then
          printf '%s  %s\\n' "$(printf 'link %s' "$(readlink -- "$path")" | sha256sum | cut -d' ' -f1)" "$path"
        else
          sha256sum -- "$path"
        fi
      done
      ctr --namespace {CTR_NAMESPACE} images ls 2>/dev/null | awk 'NR > 1 {{print "image", $1, $3}}' | sort
      active=$(jq -r '.{ACTIVE_MACHINE_FIELD} // empty' {DAEMON_STATE} 2>/dev/null)
      if [ -n "$active" ]; then echo "active $active"; fi
      for machine in $({{ printf '%s\\n' {' '.join(MACHINES)}; ls {MACHINES_DIR} 2>/dev/null; }} | sort -u); do
        state=$(machinectl show "$machine" -p State --value 2>/dev/null)
        if [ -z "$state" ]; then
          if [ -e "{MACHINES_DIR}/$machine" ]; then state=stopped; else continue; fi
        fi
        echo "machine $machine $state"
      done
    }}
    if [ "$(id -u)" = 0 ]; then
      manifest
    else
      sudo bash -c "$(declare -f manifest); manifest"
    fi
    """)

_HASH_LINE = re.compile(r"([0-9a-f]{64})  (/.*)")


def parse_state_manifest(text: str) -> dict[str, str]:
    """The manifest STATE_MANIFEST_SCRIPT prints, as a mapping: a file's path to its SHA-256,
    "image REF" to its digest, "machine NAME" to its state, and "active" to the active machine.
    Anything else is skipped."""
    manifest: dict[str, str] = {}
    for line in text.splitlines():
        line = line.rstrip("\r")
        match = _HASH_LINE.fullmatch(line)
        if match:
            manifest[match.group(2)] = match.group(1)
            continue
        fields = line.split()
        if len(fields) == 3 and fields[0] in ("image", "machine"):
            manifest[f"{fields[0]} {fields[1]}"] = fields[2]
        elif len(fields) == 2 and fields[0] == "active":
            manifest["active"] = fields[1]
    return manifest


def state_manifest(ssh: Ssh) -> dict[str, str]:
    """Run STATE_MANIFEST_SCRIPT on the VM, and parse what it prints."""
    result = ssh(STATE_MANIFEST_SCRIPT, check=False, timeout=180)
    if result.returncode != 0:
        raise StepError(f"reading the agent's state on the VM exited {result.returncode}: "
                        f"{redact(last_lines(result.stderr, 4))}")
    return parse_state_manifest(result.stdout)


def _differences(before: dict[str, str], after: dict[str, str]) -> str:
    changed = sorted(key for key in before.keys() & after.keys() if before[key] != after[key])
    parts = [(label, keys) for label, keys in (("changed", changed), ("removed", sorted(before.keys() - after.keys())),
                                               ("added", sorted(after.keys() - before.keys()))) if keys]
    return "; ".join(f"{label}: {', '.join(keys)}" for label, keys in parts)


def manifest_findings(before: dict[str, str], after: dict[str, str]) -> list[tuple[bool, str, str]]:
    """Whether the agent's state survived, from manifests taken before and after: its files, by
    content; its images, by digest; and its active machine, running. Details name paths, never
    contents."""
    def part(manifest: dict[str, str], prefix: str) -> dict[str, str]:
        return {key.removeprefix(prefix) if prefix != "/" else key: value
                for key, value in manifest.items() if key.startswith(prefix)}

    files_before, files_after = part(before, "/"), part(after, "/")
    difference = _differences(files_before, files_after)
    findings = [(bool(files_before) and not difference, f"the agent's {len(files_before)} state files are unchanged",
                 difference or "the manifest before had none")]

    images_before, images_after = part(before, "image "), part(after, "image ")
    difference = _differences(images_before, images_after)
    findings.append((bool(images_before) and not difference, "the agent's images are unchanged",
                     difference or "the manifest before had none"))

    active_before, active_after = before.get("active", ""), after.get("active", "")
    state_before = before.get(f"machine {active_before}", "")
    state_after = after.get(f"machine {active_after}", "")
    ok = bool(active_before) and active_after == active_before and state_before == state_after == "running"
    findings.append((ok, f"the active machine, {active_before or 'none'}, is the same and running",
                     f"before: {active_before or 'none'} {state_before or 'absent'}; "
                     f"after: {active_after or 'none'} {state_after or 'absent'}"))
    return findings


# ---------------------------------------------------------------------------------------------
# trident-acl-agent's kubeconfig

# printf's format for it, on one line: the server, the CA, and the kubelet's certificate and key,
# each as base64.
_KUBECONFIG_FORMAT = "\\n".join((
    "apiVersion: v1", "kind: Config",
    "clusters:", "- name: flex", "  cluster:", "    server: %s", "    certificate-authority-data: %s",
    "users:", "- name: kubelet", "  user:", "    client-certificate-data: %s", "    client-key-data: %s",
    "contexts:", "- name: flex", "  context:", "    cluster: flex", "    user: kubelet",
    "current-context: flex",
)) + "\\n"


def trident_kubeconfig_script(*, server: str, ca_pem: str, path: str = TRIDENT_KUBECONFIG,
                              unit: str = TRIDENT_UNIT) -> str:
    """A script to run on the VM that gives trident-acl-agent the kubelet's credentials: it reads the
    kubelet's client certificate and key from the agent's active machine, writes a kubeconfig at path,
    0600, with them, the server, and the CA embedded, relabels it, and restarts unit. On a Flex node
    the kubelet runs in a machine, so the host has no kubeconfig of its own for the agent's
    /var/lib/kubelet/kubeconfig contract. The key goes from file to file on the VM only, through a
    shell builtin rather than any command line, and nothing prints it."""
    if "://" not in server:
        server = f"https://{server}"
    if "-----BEGIN CERTIFICATE-----" not in ca_pem:
        raise StepError("the cluster CA isn't a PEM certificate")
    ca = base64.b64encode((ca_pem.strip() + "\n").encode()).decode()
    quote = shlex.quote
    return textwrap.dedent(f"""\
        set -eu
        state={quote(DAEMON_STATE)}
        kubeconfig={quote(path)}
        unit={quote(unit)}
        server={quote(server)}
        ca={quote(ca)}
        machine=$(sudo jq -r '.{ACTIVE_MACHINE_FIELD} // empty' "$state" 2>/dev/null || true)
        case $machine in
          {'|'.join(MACHINES)}) ;;
          '') echo "trident kubeconfig: $state names no active machine; has the agent started one?" >&2; exit 1 ;;
          *) echo "trident kubeconfig: $state names $machine, which isn't a machine of the agent's" >&2; exit 1 ;;
        esac
        root={MACHINES_DIR}/$machine
        pem={KUBELET_CLIENT_CERT}
        # kubelet's link is absolute within the machine, so it's resolved within its root.
        for _ in 1 2 3 4 5 6 7 8; do
          target=$(sudo readlink -- "$root$pem") || break
          case $target in
            /*) pem=$target ;;
            *) pem=${{pem%/*}}/$target ;;
          esac
        done
        if ! sudo test -f "$root$pem"; then
          echo "trident kubeconfig: $machine has no kubelet client certificate at {KUBELET_CLIENT_CERT}" >&2
          exit 1
        fi
        cert=$(sudo sed -n '/^-----BEGIN CERTIFICATE-----$/,/^-----END CERTIFICATE-----$/p' "$root$pem" | base64 -w0)
        key=$(sudo sed -n '/^-----BEGIN [A-Z ]*PRIVATE KEY-----$/,/^-----END [A-Z ]*PRIVATE KEY-----$/p' "$root$pem" | base64 -w0)
        if [ -z "$cert" ] || [ -z "$key" ]; then
          echo "trident kubeconfig: $machine's $pem lacks a certificate or a key" >&2
          exit 1
        fi
        printf {quote(_KUBECONFIG_FORMAT)} \\
          "$server" "$ca" "$cert" "$key" | sudo install -D -m 0600 -o root -g root /dev/stdin "$kubeconfig"
        unset key
        sudo restorecon -F "$kubeconfig"
        sudo systemctl restart "$unit"
        echo "wrote $kubeconfig from $machine's kubelet client certificate, and restarted $unit"
        """)


# ---------------------------------------------------------------------------------------------
# SELinux


def avc_denials(ssh: Ssh, *, patterns: tuple[str, ...] = AGENT_SUBJECTS) -> list[str]:
    """This boot's AVC denials that name any of patterns, or all of them for no patterns. journald
    keeps audit records under the audit transport when it reads them; otherwise the kernel logs them."""
    script = textwrap.dedent("""\
        if [ -n "$(sudo journalctl -b _TRANSPORT=audit --no-pager -q -n 1 2>/dev/null)" ]; then
          sudo journalctl -b _TRANSPORT=audit --no-pager -q
        else
          sudo journalctl -b -k --no-pager -q
        fi 2>/dev/null | grep -E 'avc:[[:space:]]+denied' || true
        """)
    lines = ssh(script, check=False, timeout=180).stdout.splitlines()
    return [line for line in lines if AVC_DENIED.search(line) and (not patterns or any(p in line for p in patterns))]


def denial_issue(line: str) -> str:
    """The issue that tracks a known denial, or "" for one that isn't known."""
    return next((issue for pattern, issue in KNOWN_DENIALS if pattern.search(line)), "")
