"""A local kind cluster for the ACL VM to join as an AKS Flex Node, on a bridge the harness owns.

The VM keeps its QEMU user-mode NIC for SSH, the internet, and the host's services, and gets a
second NIC on a TAP device in the harness's bridge. The kind node container joins that bridge too,
by a veth, at subnet.2: the VM reaches the API server there, and the API server reaches the VM's
kubelet across it. Nothing on the bridge has to leave it, so there's no NAT and no forwarding
sysctl, and one firewall rule: kind loads br_netfilter, so frames bridged between the TAP and the
veth traverse FORWARD, whose policy is DROP on a host running docker.

kind runs on rootful Podman, since attaching its node to the bridge needs root anyway. Every host
command goes through an injected run, root through `sudo -n` in the command, so the module can be
tested with a fake run (test_flexcluster.py). It adapts hack/acl-container/harness.py's cluster,
whose README.md and DESIGN.md explain the setup: the kind node advertises its bridge address, and
kindnet and kube-proxy reach the API server there rather than at the node's Podman hostname, which
the VM can't resolve.
"""

from __future__ import annotations

import base64
import copy
import dataclasses
import datetime
import hashlib
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import textwrap
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any, Protocol

from errors import StepError

# On everything the harness creates in the cluster. reset_to deletes the pods and MachineOperations
# that carry it, so the controller's pods don't: only its Deployment's own metadata does.
OWNED_LABEL = "acl-abupdate.io/owned"
OWNED_SELECTOR = f"{OWNED_LABEL}=true"

PROBE_NAME = "acl-abupdate-probe"
PROBE_LABEL = "app.kubernetes.io/name"
PROBE_IMAGE = "mcr.microsoft.com/azurelinux/busybox:1.36"
PROBE_OUTPUT = "acl-abupdate-probe-ok"
PROBE_EXEC_OUTPUT = "acl-abupdate-exec-ok"

CONTROLLER = "aks-flex-controller"
CONTROLLER_NAMESPACE = "kube-system"
CONTROLLER_ARGS = [
    "--listen-address=:8080",
    "--machine-configmap-namespace=kube-system",
    "--machine-configmap-name=aks-flex-machines",
    # Without the approver the daemon's client certificate is never issued.
    "--enable-csr-approver=true",
]
# A rebuilt controller is loaded under the same tag, so its digest in the pod template is what
# rolls the Deployment out.
CONTROLLER_DIGEST_ANNOTATION = "acl-abupdate.io/controller-sha256"
CONTROLLER_CONTAINERFILE = """\
FROM scratch
COPY aks-flex-controller /usr/local/bin/aks-flex-controller
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/aks-flex-controller"]
"""
MACHINE_CONFIGMAP = "aks-flex-machines"
MACHINE_CRDS = ("unbounded-cloud.io_machineoperations.yaml",
                "unbounded-cloud.io_machineoperationcredentials.yaml")
# The in-cluster machine endpoint, through the API server's service proxy: the agent config's
# agent.machineClient.endpointUrl.
PROXY_PATH = f"/api/v1/namespaces/kube-system/services/http:{CONTROLLER}:80/proxy"
BOOTSTRAP_GROUP = "system:bootstrappers:aks-flex-node"
# The machine the controller serves names these; nothing checks them against Azure.
AGENT_POOL = "aclab"
CLUSTER_RESOURCE_ID = ("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/aclab"
                       "/providers/Microsoft.ContainerService/managedClusters/aclab")

# The node lifecycle controller owns these taints, so reset_to leaves them as they are.
SYSTEM_TAINT_PREFIX = "node.kubernetes.io/"
INOTIFY_INSTANCES = Path("/proc/sys/fs/inotify/max_user_instances")
INOTIFY_MINIMUM = 512

_INTERFACE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,14}")
_CLUSTER_NAME = re.compile(r"[a-z0-9]([a-z0-9-]{0,47}[a-z0-9])?")
_USER = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}")
_DURATION = re.compile(r"(?:\d+[hms])+")
_VERSION = re.compile(r"v?(\d+)\.(\d+)\.(\d+)")
_BOOTSTRAP_TOKEN = re.compile(r"\b[a-z0-9]{6}\.[a-z0-9]{16}\b")
_PRIVATE_KEY = re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S)


class Runner(Protocol):
    def __call__(self, cmd: list[str], *, input: str | None = None, timeout: float | None = None,
                 stream: bool = False) -> subprocess.CompletedProcess[str]: ...


Finding = tuple[bool, str, str]


# ---------------------------------------------------------------------------------------------
# The cluster's settings


@dataclasses.dataclass(frozen=True)
class ClusterSpec:
    """Names and addresses of the cluster and its network. They default to the harness's own, apart
    from the other harnesses in hack, so they can run alongside it."""

    name: str = "aclab"
    subnet: str = "192.168.113"
    bridge: str = "virbr-aclab"
    tap: str = "tap-aclab"
    veth_host: str = "veth-aclab-kind"
    veth_kind: str = "eth-aclab"
    node_image: str = ""
    user: str = ""

    @property
    def gateway(self) -> str:
        return f"{self.subnet}.1"

    @property
    def kind_ip(self) -> str:
        return f"{self.subnet}.2"

    @property
    def vm_ip(self) -> str:
        return f"{self.subnet}.10"

    @property
    def api_server(self) -> str:
        return f"{self.kind_ip}:6443"

    @property
    def node_container(self) -> str:
        return f"{self.name}-control-plane"

    def to_dict(self) -> dict[str, str]:
        return dataclasses.asdict(self)

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> ClusterSpec:
        known = {field.name for field in dataclasses.fields(cls)}
        spec = cls(**{key: value for key, value in data.items() if key in known})
        spec.validate()
        return spec

    def validate(self) -> None:
        for field in dataclasses.fields(self):
            if not isinstance(getattr(self, field.name), str):
                raise StepError(f"the cluster's {field.name} must be a string, not {getattr(self, field.name)!r}")
        if not _CLUSTER_NAME.fullmatch(self.name):
            raise StepError(f"{self.name!r} isn't a kind cluster name: use 1 to 49 lowercase letters, digits, "
                            "and '-', starting and ending with a letter or digit")
        octets = self.subnet.split(".")
        if len(octets) != 3 or not all(o.isdigit() and str(int(o)) == o and int(o) <= 255 for o in octets):
            raise StepError(f"{self.subnet!r} isn't the first three octets of a /24, such as 192.168.113")
        interfaces = {"bridge": self.bridge, "tap": self.tap, "veth_host": self.veth_host,
                      "veth_kind": self.veth_kind}
        for what, value in interfaces.items():
            # The kernel's names are at most 15 characters (IFNAMSIZ).
            if not _INTERFACE.fullmatch(value):
                raise StepError(f"the {what} name {value!r} isn't an interface name: use 1 to 15 letters, "
                                "digits, '_', '.', and '-'")
        if len(set(interfaces.values())) != len(interfaces):
            raise StepError(f"the interface names must differ: {', '.join(interfaces.values())}")
        if self.node_image and not re.fullmatch(r"[^\s\"'\\]+", self.node_image):
            raise StepError(f"{self.node_image!r} isn't an image reference")
        if self.user and not _USER.fullmatch(self.user):
            raise StepError(f"{self.user!r} isn't a user name or ID")


# ---------------------------------------------------------------------------------------------
# Host network


def forward_rule(spec: ClusterSpec) -> list[str]:
    """The FORWARD rule's match and target, for the frames bridged between the VM and kind."""
    return ["-i", spec.bridge, "-o", spec.bridge, "-j", "ACCEPT"]


def _sudo(*cmd: str) -> list[str]:
    return ["sudo", "-n", *cmd]


def _tap_add(spec: ClusterSpec) -> list[str]:
    if not spec.user:
        raise StepError("the TAP device needs an owner, for QEMU to open it as that user: set the cluster's user")
    return _sudo("ip", "tuntap", "add", "dev", spec.tap, "mode", "tap", "user", spec.user)


def network_up_commands(spec: ClusterSpec) -> list[list[str]]:
    """The bridge, with the host's address on it, the VM's TAP device in it, owned by the user QEMU
    runs as, and the FORWARD rule for the bridge."""
    return [
        _sudo("ip", "link", "add", spec.bridge, "type", "bridge"),
        _sudo("ip", "addr", "add", f"{spec.gateway}/24", "dev", spec.bridge),
        _sudo("ip", "link", "set", spec.bridge, "up"),
        _tap_add(spec),
        _sudo("ip", "link", "set", spec.tap, "master", spec.bridge),
        _sudo("ip", "link", "set", spec.tap, "up"),
        _sudo("iptables", "-I", "FORWARD", *forward_rule(spec)),
    ]


def network_down_commands(spec: ClusterSpec) -> list[list[str]]:
    """Undo network_up_commands, in reverse, and remove the kind node's veth, which attaching it
    made. Each may fail, as what it removes may be gone; the rule's is repeated until it fails."""
    return [
        _sudo("iptables", "-D", "FORWARD", *forward_rule(spec)),
        _sudo("ip", "link", "delete", spec.veth_host),
        _sudo("ip", "link", "delete", spec.tap),
        _sudo("ip", "link", "delete", spec.bridge),
    ]


def _is_rule_delete(cmd: list[str]) -> bool:
    return "iptables" in cmd and "-D" in cmd


def links_by_name(links: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """`ip -j addr show`'s links by name."""
    return {link.get("ifname", ""): link for link in links if isinstance(link, dict)}


def link_addresses(link: dict[str, Any]) -> list[str]:
    return [f"{a.get('local')}/{a.get('prefixlen')}" for a in link.get("addr_info", []) or []
            if a.get("family") == "inet"]


def _link_up(link: dict[str, Any] | None) -> bool:
    # A TAP no QEMU holds is NO-CARRIER and operstate DOWN while set up, so the flag is what counts.
    return link is not None and "UP" in (link.get("flags") or [])


def network_repair_commands(spec: ClusterSpec, links: list[dict[str, Any]], rule_present: bool) -> list[list[str]]:
    """What network_up_commands would make that's missing, from `ip -j addr show`'s links: a host
    restart drops the bridge, the TAP, and the rule, and deleting the bridge alone detaches the TAP.
    An existing TAP is kept, since a running QEMU may hold it."""
    by_name = links_by_name(links)
    bridge, tap = by_name.get(spec.bridge), by_name.get(spec.tap)
    commands = []
    if bridge is None:
        commands.append(_sudo("ip", "link", "add", spec.bridge, "type", "bridge"))
    if bridge is None or f"{spec.gateway}/24" not in link_addresses(bridge):
        commands.append(_sudo("ip", "addr", "add", f"{spec.gateway}/24", "dev", spec.bridge))
    if not _link_up(bridge):
        commands.append(_sudo("ip", "link", "set", spec.bridge, "up"))
    if tap is None:
        commands.append(_tap_add(spec))
    if tap is None or tap.get("master") != spec.bridge:
        commands.append(_sudo("ip", "link", "set", spec.tap, "master", spec.bridge))
    if not _link_up(tap):
        commands.append(_sudo("ip", "link", "set", spec.tap, "up"))
    if not rule_present:
        commands.append(_sudo("iptables", "-I", "FORWARD", *forward_rule(spec)))
    return commands


def forward_rule_count(spec: ClusterSpec, listing: str) -> int:
    """How many times `iptables -S FORWARD` lists the bridge's rule."""
    wanted = ["-A", "FORWARD", *forward_rule(spec)]
    return sum(1 for line in listing.splitlines() if line.split() == wanted)


def attach_commands(spec: ClusterSpec, pid: str) -> list[list[str]]:
    """Give the kind node, whose container's PID is pid, an address on the bridge, by a veth."""
    return [
        _sudo("ip", "link", "add", spec.veth_host, "type", "veth", "peer", "name", spec.veth_kind),
        _sudo("ip", "link", "set", spec.veth_host, "master", spec.bridge),
        _sudo("ip", "link", "set", spec.veth_host, "up"),
        _sudo("ip", "link", "set", spec.veth_kind, "netns", pid),
        _sudo("nsenter", "-t", pid, "-n", "ip", "addr", "add", f"{spec.kind_ip}/24", "dev", spec.veth_kind),
        _sudo("nsenter", "-t", pid, "-n", "ip", "link", "set", spec.veth_kind, "up"),
    ]


def inotify_warning(value: str) -> str | None:
    """A warning when the host's inotify instances are too few for kind's pods."""
    try:
        instances = int(value.strip())
    except ValueError:
        return None
    if instances >= INOTIFY_MINIMUM:
        return None
    return (f"fs.inotify.max_user_instances is {instances}, and kind needs {INOTIFY_MINIMUM} or more, "
            "or kube-proxy and the controller crash-loop with 'too many open files'; raise it with "
            f"`sudo sysctl fs.inotify.max_user_instances={INOTIFY_MINIMUM}`")


# ---------------------------------------------------------------------------------------------
# kind and the cluster's configuration


def kind_config(spec: ClusterSpec) -> str:
    """kind's cluster config. The API server's certificate also names the bridge address, where
    the VM reaches it; patches replace lists, so it keeps kind's own names too."""
    node = "  - role: control-plane\n"
    if spec.node_image:
        node += f"    image: {json.dumps(spec.node_image)}\n"
    sans = ", ".join(json.dumps(name) for name in ("localhost", "127.0.0.1", spec.kind_ip))
    return ("kind: Cluster\n"
            "apiVersion: kind.x-k8s.io/v1alpha4\n"
            "nodes:\n" + node +
            "kubeadmConfigPatches:\n"
            "  - |\n"
            "    kind: ClusterConfiguration\n"
            "    apiServer:\n"
            f"      certSANs: [{sans}]\n")


# Rewrites the kubelet's flags so it runs with --node-ip on the bridge, and restarts it if they
# changed, or if it isn't running, as after a host restart, when it fails until the veth is back.
ADVERTISE_SCRIPT = textwrap.dedent("""\
    set -eu
    flags=/var/lib/kubelet/kubeadm-flags.env
    . "$flags"
    set -- $KUBELET_KUBEADM_ARGS
    args=""
    for arg do
      case "$arg" in
        --node-ip=*) ;;
        *) args="$args $arg" ;;
      esac
    done
    want="KUBELET_KUBEADM_ARGS=\\"${args# } --node-ip=@NODE_IP@\\""
    if [ "$(cat "$flags")" != "$want" ]; then
      printf '%s\\n' "$want" >"$flags"
      systemctl restart kubelet
    elif ! systemctl is-active --quiet kubelet; then
      systemctl restart kubelet
    fi
    """)


def advertise_script(spec: ClusterSpec) -> str:
    return ADVERTISE_SCRIPT.replace("@NODE_IP@", spec.kind_ip)


def kube_proxy_kubeconfig(text: str, api_server: str) -> str:
    """kube-proxy's kubeconfig, with its server at the bridge address."""
    lines = []
    for line in text.splitlines():
        if line.strip().startswith("server:"):
            line = f"{line[:len(line) - len(line.lstrip())]}server: https://{api_server}"
        lines.append(line)
    return "\n".join(lines) + "\n"


def kindnet_patch(daemonset: dict[str, Any], api_server: str) -> dict[str, Any] | None:
    """A strategic merge patch that points kindnet's CONTROL_PLANE_ENDPOINT at the bridge address,
    or None if it does already."""
    containers = daemonset.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
    for container in containers:
        env = {item.get("name"): item.get("value") for item in container.get("env", []) or []}
        if "CONTROL_PLANE_ENDPOINT" not in env:
            continue
        if env["CONTROL_PLANE_ENDPOINT"] == api_server:
            return None
        return {"spec": {"template": {"spec": {"containers": [
            {"name": container["name"], "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": api_server}]}]}}}}
    raise StepError("kindnet's DaemonSet has no container with CONTROL_PLANE_ENDPOINT; this kind version "
                    "configures it differently")


def owned(obj: dict[str, Any]) -> dict[str, Any]:
    """obj, labelled as the harness's."""
    metadata = obj.setdefault("metadata", {})
    metadata["labels"] = {**(metadata.get("labels") or {}), OWNED_LABEL: "true"}
    return obj


def node_rbac() -> list[dict[str, Any]]:
    """The bindings scripts/aks-flex-config applies for bootstrap-token nodes."""
    bindings = {
        "aks-flex-node-bootstrapper": "system:node-bootstrapper",
        "aks-flex-node-auto-approve-csr": "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
    }
    return [owned({
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "ClusterRoleBinding",
        "metadata": {"name": name},
        "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": role},
        "subjects": [{"apiGroup": "rbac.authorization.k8s.io", "kind": "Group", "name": BOOTSTRAP_GROUP}],
    }) for name, role in bindings.items()]


def kubectl_items(text: str) -> list[dict[str, Any]]:
    """The objects of kubectl's JSON output: a List's items, or the objects it prints one after
    another, as create --dry-run does for several manifests."""
    decoder = json.JSONDecoder()
    items: list[dict[str, Any]] = []
    position = 0
    while True:
        while position < len(text) and text[position].isspace():
            position += 1
        if position >= len(text):
            return items
        try:
            document, position = decoder.raw_decode(text, position)
        except json.JSONDecodeError as error:
            raise StepError(f"kubectl printed no JSON objects: {error}") from error
        if document.get("kind", "").endswith("List"):
            items.extend(document.get("items", []))
        else:
            items.append(document)


def controller_manifest(objects: list[dict[str, Any]], image: str, binary_sha256: str) -> dict[str, Any]:
    """hack/controller-deployment's objects, the CRDs, and the node RBAC as one List to apply, all
    labelled as the harness's, with the controller running the image loaded into kind, its CSR
    approver on. The Deployment's pods aren't labelled, so reset_to leaves them."""
    items = []
    deployment = False
    for obj in copy.deepcopy(objects):
        owned(obj)
        if obj.get("kind") == "Deployment" and obj["metadata"].get("name") == CONTROLLER:
            template = obj["spec"]["template"]
            container = template["spec"]["containers"][0]
            container.update(image=image, imagePullPolicy="Never", args=list(CONTROLLER_ARGS))
            metadata = template.setdefault("metadata", {})
            metadata["annotations"] = {**(metadata.get("annotations") or {}),
                                       CONTROLLER_DIGEST_ANNOTATION: binary_sha256}
            deployment = True
        items.append(obj)
    if not deployment:
        raise StepError(f"hack/controller-deployment has no Deployment {CONTROLLER}")
    return {"apiVersion": "v1", "kind": "List", "items": items}


def machine_json(node_name: str, kubernetes_version: str, settings_version: str) -> dict[str, Any]:
    """The node's machine, as ARM returns it, for the controller's store. Its Kubernetes version
    has to match the agent config's. Its eTag is the goal's settings version, which a repave
    changes."""
    return {
        "id": f"{CLUSTER_RESOURCE_ID}/agentPools/{AGENT_POOL}/machines/{node_name}",
        "name": node_name,
        "type": "Microsoft.ContainerService/managedClusters/agentPools/machines",
        "properties": {
            "eTag": settings_version,
            "provisioningState": "Succeeded",
            "kubernetes": {"orchestratorVersion": kubernetes_version, "nodeLabels": {}, "nodeTaints": []},
        },
    }


def parse_duration(value: str) -> datetime.timedelta:
    """A Go-style duration of hours, minutes, and seconds, such as 24h or 1h30m."""
    if not _DURATION.fullmatch(value):
        raise StepError(f"{value!r} isn't a duration such as 24h, 90m, or 1h30m")
    units = {"h": 3600, "m": 60, "s": 1}
    seconds = sum(int(n) * units[unit] for n, unit in re.findall(r"(\d+)([hms])", value))
    if seconds <= 0:
        raise StepError(f"{value!r} isn't a positive duration")
    return datetime.timedelta(seconds=seconds)


def bootstrap_token_secret(token_id: str, token_secret: str, expiration: datetime.datetime) -> dict[str, Any]:
    return owned({
        "apiVersion": "v1",
        "kind": "Secret",
        "type": "bootstrap.kubernetes.io/token",
        "metadata": {"name": f"bootstrap-token-{token_id}", "namespace": "kube-system"},
        "stringData": {
            "description": "an AKS Flex Node joining from hack/acl-abupdate",
            "token-id": token_id,
            "token-secret": token_secret,
            "expiration": expiration.astimezone(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "usage-bootstrap-authentication": "true",
            "usage-bootstrap-signing": "true",
            "auth-extra-groups": BOOTSTRAP_GROUP,
        },
    })


def probe_manifest(node_name: str, namespace: str) -> dict[str, Any]:
    """The probe's namespace and Deployment: one pod, pinned to the node, tolerating its taints.
    A Deployment rather than a pod, so the pod comes back after a drain or a recreated Node."""
    labels = {PROBE_LABEL: PROBE_NAME, OWNED_LABEL: "true"}
    deployment = {
        "apiVersion": "apps/v1",
        "kind": "Deployment",
        "metadata": {"name": PROBE_NAME, "namespace": namespace, "labels": dict(labels)},
        "spec": {
            "replicas": 1,
            # One pod at a time, so the pod on the node is the one to check.
            "strategy": {"type": "Recreate"},
            "selector": {"matchLabels": {PROBE_LABEL: PROBE_NAME}},
            "template": {
                "metadata": {"labels": dict(labels)},
                "spec": {
                    "nodeSelector": {"kubernetes.io/hostname": node_name},
                    "tolerations": [{"operator": "Exists"}],
                    "terminationGracePeriodSeconds": 10,
                    # sh, as PID 1, ignores SIGTERM without a trap, so the pod would take its whole
                    # grace period to stop, and hold up a drain or the node's shutdown.
                    "containers": [{
                        "name": "probe",
                        "image": PROBE_IMAGE,
                        "command": ["sh", "-c", f"trap 'exit 0' TERM; echo {PROBE_OUTPUT}; "
                                                "while :; do sleep 3600 & wait $!; done"],
                    }],
                },
            },
        },
    }
    namespace_object = owned({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": namespace}})
    return {"apiVersion": "v1", "kind": "List", "items": [namespace_object, deployment]}


# ---------------------------------------------------------------------------------------------
# Nodes


def condition(obj: dict[str, Any] | None, kind: str) -> dict[str, Any]:
    for item in (obj or {}).get("status", {}).get("conditions", []) or []:
        if item.get("type") == kind:
            return item
    return {}


def describe_conditions(node: dict[str, Any] | None) -> str:
    if node is None:
        return "the Node doesn't exist"
    parts = []
    for item in node.get("status", {}).get("conditions", []) or []:
        reason = ": ".join(part for part in (item.get("reason"), item.get("message")) if part)
        parts.append(f"{item.get('type')}={item.get('status')}" + (f" ({reason})" if reason else ""))
    return "; ".join(parts) or "the Node has no conditions"


def internal_ips(node: dict[str, Any]) -> list[str]:
    return [address.get("address", "") for address in node.get("status", {}).get("addresses", []) or []
            if address.get("type") == "InternalIP"]


def release_version(version: str) -> str:
    """major.minor.patch of a Kubernetes version, without its v or any suffix: v1.37.0+k3s1 is 1.37.0."""
    match = _VERSION.match(version.strip())
    return ".".join(match.groups()) if match else version.strip()


def parse_server_version(text: str) -> str:
    """The server's version from `kubectl version -o json`, such as 1.37.0."""
    try:
        version = json.loads(text)["serverVersion"]["gitVersion"]
    except (ValueError, KeyError, TypeError) as error:
        raise StepError(f"kubectl version printed no server version: {error}") from error
    return release_version(version)


def parse_time(value: str) -> datetime.datetime | None:
    """A Kubernetes timestamp or MicroTime, as an aware UTC datetime."""
    if not value:
        return None
    try:
        parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=datetime.timezone.utc)


def parse_lease_renew_time(lease: dict[str, Any] | None) -> datetime.datetime | None:
    return parse_time(((lease or {}).get("spec") or {}).get("renewTime") or "")


def _taint(taint: dict[str, Any]) -> dict[str, str]:
    """A taint without timeAdded, which the API server sets."""
    normal = {"key": taint.get("key", ""), "effect": taint.get("effect", "")}
    if taint.get("value"):
        normal["value"] = taint["value"]
    return normal


def _taint_key(taint: dict[str, str]) -> tuple[str, str, str]:
    return taint.get("key", ""), taint.get("value", ""), taint.get("effect", "")


def node_record(node: dict[str, Any], prefix: str) -> dict[str, Any]:
    """What reset_to restores: the node's UID, its annotations under prefix/, whether it's
    cordoned, and its taints, apart from the node lifecycle controller's."""
    metadata, spec = node.get("metadata", {}), node.get("spec", {})
    annotations = metadata.get("annotations") or {}
    taints = [_taint(t) for t in spec.get("taints") or [] if not t.get("key", "").startswith(SYSTEM_TAINT_PREFIX)]
    return {
        "node": metadata.get("name", ""),
        "uid": metadata.get("uid", ""),
        "annotations": {key: value for key, value in sorted(annotations.items()) if key.startswith(f"{prefix}/")},
        "unschedulable": bool(spec.get("unschedulable")),
        "taints": sorted(taints, key=_taint_key),
    }


def reset_patch(node: dict[str, Any], record: dict[str, Any], prefix: str) -> dict[str, Any] | None:
    """A JSON merge patch that makes the node's annotations under prefix/, unschedulable, and
    taints other than node.kubernetes.io/ ones as record has them, or None if they are. It names
    the node's resourceVersion, so it fails rather than overwrite a change made meanwhile."""
    if node.get("metadata", {}).get("uid") != record.get("uid"):
        raise StepError(f"node {node.get('metadata', {}).get('name', '')} was recreated: its UID is "
                        f"{node.get('metadata', {}).get('uid')}, not {record.get('uid')}; rejoin it")
    current = node_record(node, prefix)
    wanted_annotations = record.get("annotations") or {}
    annotations: dict[str, str | None] = {key: None for key in current["annotations"] if key not in wanted_annotations}
    annotations.update({key: value for key, value in wanted_annotations.items()
                        if current["annotations"].get(key) != value})
    spec: dict[str, Any] = {}
    if current["unschedulable"] != bool(record.get("unschedulable")):
        spec["unschedulable"] = True if record.get("unschedulable") else None
    wanted_taints = sorted((_taint(t) for t in record.get("taints") or []), key=_taint_key)
    if [_taint_key(t) for t in current["taints"]] != [_taint_key(t) for t in wanted_taints]:
        system = [t for t in node.get("spec", {}).get("taints") or []
                  if t.get("key", "").startswith(SYSTEM_TAINT_PREFIX)]
        spec["taints"] = (system + wanted_taints) or None
    if not annotations and not spec:
        return None
    patch: dict[str, Any] = {"metadata": {"resourceVersion": node.get("metadata", {}).get("resourceVersion", "")}}
    if annotations:
        patch["metadata"]["annotations"] = annotations
    if spec:
        patch["spec"] = spec
    return patch


def node_findings_from(node: dict[str, Any] | None, *, name: str, expected_ip: str, server_version: str,
                       renew_time: datetime.datetime | None, since: datetime.datetime | None) -> list[Finding]:
    """node_findings, from what it read: the Node, the server's version, and the Lease's renewal."""
    descriptions = [f"node {name} is Ready", f"node {name}'s InternalIP is {expected_ip}",
                    f"node {name}'s kubelet is the server's version, {server_version}"]
    if since is not None:
        descriptions.append(f"node {name}'s Lease was renewed after {since.isoformat(timespec='seconds')}")
    if node is None:
        return [(False, description, "the Node doesn't exist") for description in descriptions]
    findings: list[Finding] = []
    ready = condition(node, "Ready").get("status") == "True"
    findings.append((ready, descriptions[0], describe_conditions(node)))
    addresses = internal_ips(node)
    findings.append((addresses == [expected_ip], descriptions[1], f"InternalIP {', '.join(addresses) or 'none'}"))
    kubelet = node.get("status", {}).get("nodeInfo", {}).get("kubeletVersion", "")
    findings.append((bool(kubelet) and release_version(kubelet) == server_version, descriptions[2],
                     f"the kubelet is {kubelet or 'unknown'}"))
    if since is not None:
        if since.tzinfo is None:
            since = since.replace(tzinfo=datetime.timezone.utc)
        renewed = renew_time is not None and renew_time > since
        findings.append((renewed, descriptions[3],
                         f"renewed at {renew_time.isoformat()}" if renew_time else "it has no Lease, or no renewTime"))
    return findings


def _waiting_reasons(pod: dict[str, Any]) -> list[str]:
    reasons = []
    for status in pod.get("status", {}).get("containerStatuses", []) or []:
        waiting = (status.get("state") or {}).get("waiting") or {}
        if waiting.get("reason"):
            reasons.append(waiting["reason"])
    return reasons


def probe_pod(pods: dict[str, Any], node_name: str) -> tuple[dict[str, Any] | None, str]:
    """The probe's pod that runs on node_name and is Ready, if any, and what the probe's pods are
    doing, for a failure's detail."""
    states = []
    for pod in pods.get("items", []) or []:
        metadata, spec, status = pod.get("metadata", {}), pod.get("spec", {}), pod.get("status", {})
        ready = condition(pod, "Ready").get("status") == "True"
        state = (f"{metadata.get('name', '?')} on {spec.get('nodeName') or 'no node'}: "
                 f"{status.get('phase', 'unknown')}, Ready={'True' if ready else 'False'}")
        reasons = _waiting_reasons(pod)
        scheduled = condition(pod, "PodScheduled")
        if scheduled.get("status") == "False" and scheduled.get("message"):
            reasons.append(scheduled["message"])
        if reasons:
            state += f" ({'; '.join(reasons)})"
        if metadata.get("deletionTimestamp"):
            states.append(state + ", terminating")
            continue
        if spec.get("nodeName") == node_name and status.get("phase") == "Running" and ready:
            return pod, state
        states.append(state)
    return None, "; ".join(states) or "there's no probe pod"


# ---------------------------------------------------------------------------------------------
# Helpers


def last_lines(text: str | None, count: int = 12) -> str:
    return "\n".join((text or "").strip().splitlines()[-count:])


def redact(text: str) -> str:
    """Drop bootstrap tokens and private keys from text the harness writes for diagnostics."""
    return _PRIVATE_KEY.sub("<private key redacted>", _BOOTSTRAP_TOKEN.sub("<bootstrap token redacted>", text))


def write_private(path: Path, text: str) -> None:
    """Write a file only its owner can read, in place of any there, all at once."""
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    partial = path.with_name(f".{path.name}.partial")
    partial.unlink(missing_ok=True)
    with os.fdopen(os.open(partial, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
        out.write(text)
    os.replace(partial, path)


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _command_error(cmd: list[str], result: subprocess.CompletedProcess[str]) -> StepError:
    if result.stderr is None and result.stdout is None:
        detail = " (its output is above)"
    else:
        tail = last_lines(result.stderr) or last_lines(result.stdout)
        detail = f": {tail}" if tail else ""
    return StepError(f"{shlex.join(cmd)} exited {result.returncode}{detail}")


# ---------------------------------------------------------------------------------------------
# The cluster


class Cluster:
    """The kind cluster and its network: made by create, restored by ensure after a host restart,
    and removed by delete. The VM joins it with a bootstrap token, and reads its machine from the
    in-cluster controller, so no Azure resources are involved."""

    def __init__(self, spec: ClusterSpec, *, kubeconfig: Path, repo: Path, work_dir: Path,
                 run: Runner, log: Callable[[str], None]) -> None:
        spec.validate()
        self.spec = spec
        self.kubeconfig = kubeconfig
        self.repo = repo
        self.work_dir = work_dir
        self.run = run
        self.log = log
        # Seams for the tests, which don't wait, and don't need kind installed.
        self.sleep: Callable[[float], None] = time.sleep
        self.monotonic: Callable[[], float] = time.monotonic
        self.which: Callable[[str], str | None] = shutil.which
        self.inotify_path = INOTIFY_INSTANCES

    @property
    def controller_image(self) -> str:
        return f"localhost/{CONTROLLER}:{self.spec.name}"

    @property
    def _root_kubeconfig(self) -> Path:
        """Where kind, run as root, writes and edits its kubeconfig, rather than root's own. The
        user's kubeconfig is written from `kind get kubeconfig` instead, so it's the user's."""
        return self.work_dir / "kind-root.kubeconfig"

    # Commands

    def _run(self, cmd: list[str], *, input: str | None = None, timeout: float | None = 120,
             stream: bool = False, check: bool = True) -> subprocess.CompletedProcess[str]:
        try:
            result = self.run(cmd, input=input, timeout=timeout, stream=stream)
        except subprocess.TimeoutExpired:
            result = subprocess.CompletedProcess(cmd, 124, "", f"timed out after {int(timeout or 0)}s")
        if check and result.returncode != 0:
            raise _command_error(cmd, result)
        return result

    def _sudo(self, *cmd: str, check: bool = True, timeout: float | None = 120) -> subprocess.CompletedProcess[str]:
        return self._run(_sudo(*cmd), check=check, timeout=timeout)

    def _kind_binary(self) -> str:
        # Under sudo, PATH is secure_path, which a user's kind, from go install, isn't on.
        binary = self.which("kind")
        if binary is None:
            raise StepError("kind is needed for the cluster the VM joins; install it, such as with "
                            "`go install sigs.k8s.io/kind@latest`")
        return binary

    def _kind(self, *args: str, check: bool = True, timeout: float | None = 300,
              stream: bool = False) -> subprocess.CompletedProcess[str]:
        cmd = _sudo("env", "KIND_EXPERIMENTAL_PROVIDER=podman", self._kind_binary(), *args)
        return self._run(cmd, check=check, timeout=timeout, stream=stream)

    def _kubectl(self, *args: str, input: str | None = None, check: bool = True,
                 timeout: float | None = 120) -> subprocess.CompletedProcess[str]:
        return self._run(["kubectl", "--kubeconfig", str(self.kubeconfig), *args], input=input, check=check,
                         timeout=timeout)

    def kubectl(self, *args: str, input: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
        return self._kubectl(*args, input=input, check=check, timeout=600)

    def _kubectl_json(self, *args: str) -> Any:
        result = self._kubectl(*args, "-o", "json")
        text = (result.stdout or "").strip()
        try:
            return json.loads(text) if text else None
        except json.JSONDecodeError as error:
            raise StepError(f"kubectl {' '.join(args)} printed no JSON: {error}") from error

    def _poll(self, timeout: float, probe: Callable[[], bool], interval: float = 5) -> bool:
        deadline = self.monotonic() + timeout
        while True:
            if probe():
                return True
            if self.monotonic() >= deadline:
                return False
            self.sleep(interval)

    def _private_dir(self, *parts: str) -> Path:
        self.work_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.work_dir.chmod(0o700)
        directory = self.work_dir.joinpath(*parts)
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        return directory

    # Host network

    def _links(self) -> list[dict[str, Any]]:
        result = self._run(["ip", "-j", "addr", "show"])
        try:
            return json.loads(result.stdout or "[]")
        except json.JSONDecodeError as error:
            raise StepError(f"ip -j addr show printed no JSON: {error}") from error

    def _forward_rules(self) -> int:
        return forward_rule_count(self.spec, self._sudo("iptables", "-S", "FORWARD").stdout or "")

    def _network_down(self) -> list[str]:
        failures = []
        for cmd in network_down_commands(self.spec):
            if _is_rule_delete(cmd):
                # Each run deletes one copy of the rule, and an earlier run may have left several.
                for _ in range(100):
                    if self._run(cmd, check=False).returncode != 0:
                        break
                continue
            result = self._run(cmd, check=False)
            if result.returncode != 0 and "Cannot find device" not in (result.stderr or ""):
                failures.append(str(_command_error(cmd, result)))
        return failures

    def _network_up(self) -> None:
        self.log(f"creating bridge {self.spec.bridge} ({self.spec.gateway}/24) and {self.spec.tap}, "
                 f"owned by {self.spec.user or '(no one)'}")
        for cmd in network_up_commands(self.spec):
            self._run(cmd)

    def _ensure_network(self) -> None:
        commands = network_repair_commands(self.spec, self._links(), self._forward_rules() > 0)
        if commands:
            self.log("restoring the network: " + "; ".join(shlex.join(cmd[2:]) for cmd in commands))
        for cmd in commands:
            self._run(cmd)

    # The kind node

    def _container(self, field: str) -> str:
        result = self._sudo("podman", "inspect", "--format", f"{{{{.State.{field}}}}}", self.spec.node_container)
        return (result.stdout or "").strip()

    def _container_pid(self) -> str:
        pid = self._container("Pid")
        if not pid.isdigit() or pid == "0":
            raise StepError(f"{self.spec.node_container} isn't running (its PID is {pid or 'unknown'})")
        return pid

    def _veth_attached(self, pid: str) -> bool:
        host = links_by_name(self._links()).get(self.spec.veth_host)
        if host is None or host.get("master") != self.spec.bridge or not _link_up(host):
            return False
        inside = self._sudo("nsenter", "-t", pid, "-n", "ip", "-j", "addr", "show", "dev", self.spec.veth_kind,
                            check=False)
        if inside.returncode != 0:
            return False
        try:
            links = json.loads(inside.stdout or "[]")
        except json.JSONDecodeError:
            return False
        return any(f"{self.spec.kind_ip}/24" in link_addresses(link) and _link_up(link) for link in links)

    def _attach_to_bridge(self) -> None:
        """Give the kind node its address on the bridge, unless it has it, where the VM reaches the
        API server and the API server reaches the VM's kubelet."""
        pid = self._container_pid()
        if self._veth_attached(pid):
            return
        self.log(f"attaching {self.spec.node_container} to {self.spec.bridge} as {self.spec.kind_ip}")
        self._sudo("ip", "link", "delete", self.spec.veth_host, check=False)
        self._sudo("nsenter", "-t", pid, "-n", "ip", "link", "delete", self.spec.veth_kind, check=False)
        for cmd in attach_commands(self.spec, pid):
            self._run(cmd)

    def _advertise_bridge_address(self, timeout: float = 180) -> None:
        """Make the kind node's kubelet advertise its bridge address. kindnet on the VM routes the
        kind node's pod CIDR through it, and the VM can't reach the node's Podman address."""
        cmd = _sudo("podman", "exec", self.spec.node_container, "sh", "-c", advertise_script(self.spec))
        attempts: list[subprocess.CompletedProcess[str]] = []

        def advertised_flags() -> bool:
            # Right after a start, the node's systemd may not take a restart yet.
            attempts.append(self._run(cmd, check=False))
            return attempts[-1].returncode == 0

        if not self._poll(60, advertised_flags, 3):
            raise _command_error(cmd, attempts[-1])

        def advertised() -> bool:
            result = self._kubectl("get", "node", self.spec.node_container, "-o",
                                   "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}", check=False,
                                   timeout=30)
            return result.returncode == 0 and (result.stdout or "").split() == [self.spec.kind_ip]

        if not self._poll(timeout, advertised, 3):
            raise StepError(f"{self.spec.node_container} didn't advertise {self.spec.kind_ip} within {int(timeout)}s")

    def _point_cluster_at_bridge(self) -> None:
        """kindnet and kube-proxy on the VM would otherwise reach the API server at the kind node's
        container hostname, which the VM can't resolve."""
        daemonset = self._kubectl_json("-n", "kube-system", "get", "daemonset", "kindnet")
        patch = kindnet_patch(daemonset or {}, self.spec.api_server)
        if patch is not None:
            self._kubectl("-n", "kube-system", "patch", "daemonset", "kindnet", "--type=strategic",
                          "-p", json.dumps(patch))
            self._kubectl("-n", "kube-system", "delete", "pod", "-l", "app=kindnet", "--wait=false")
        configmap = self._kubectl_json("-n", "kube-system", "get", "configmap", "kube-proxy") or {}
        current = configmap.get("data", {}).get("kubeconfig.conf", "")
        wanted = kube_proxy_kubeconfig(current, self.spec.api_server)
        if wanted != current:
            self._kubectl("-n", "kube-system", "patch", "configmap", "kube-proxy", "--type=merge",
                          "-p", json.dumps({"data": {"kubeconfig.conf": wanted}}))
            self._kubectl("-n", "kube-system", "rollout", "restart", "daemonset/kube-proxy")
        for daemonset_name in ("kindnet", "kube-proxy"):
            self._kubectl("-n", "kube-system", "rollout", "status", f"daemonset/{daemonset_name}",
                          "--timeout=180s", timeout=240)

    def _write_kubeconfig(self) -> None:
        """The user's kubeconfig, private, from kind's: its server is the API server's port on the
        host's loopback, which needs no bridge."""
        text = self._kind("get", "kubeconfig", "--name", self.spec.name).stdout or ""
        if "server:" not in text:
            raise StepError(f"kind get kubeconfig printed no kubeconfig for {self.spec.name}")
        current = self.kubeconfig.read_text() if self.kubeconfig.is_file() else None
        if current != text:
            write_private(self.kubeconfig, text)

    def _wait_for_api_server(self, timeout: float) -> None:
        def ready() -> bool:
            return self._kubectl("get", "--raw", "/readyz", check=False, timeout=30).returncode == 0

        if not self._poll(timeout, ready, 3):
            raise StepError(f"the API server of {self.spec.name} wasn't ready within {int(timeout)}s")

    def _warn_about_inotify(self) -> None:
        try:
            warning = inotify_warning(self.inotify_path.read_text())
        except OSError:
            return
        if warning:
            self.log(f"warning: {warning}")

    # Lifecycle

    def exists(self) -> bool:
        return self.spec.name in (self._kind("get", "clusters").stdout or "").split()

    def create(self) -> None:
        """Create the network and the cluster, replacing any left from an earlier run, and write
        the user's kubeconfig."""
        self._warn_about_inotify()
        if self.exists():
            self.log(f"deleting kind cluster {self.spec.name}, left from an earlier run")
            self._kind("delete", "cluster", "--name", self.spec.name, "--kubeconfig", str(self._root_kubeconfig))
        self._network_down()
        self._network_up()
        config = self._private_dir() / "kind.yaml"
        config.write_text(kind_config(self.spec))
        self.log(f"creating kind cluster {self.spec.name} on rootful Podman"
                 + (f" with {self.spec.node_image}" if self.spec.node_image else ""))
        self._kind("create", "cluster", "--name", self.spec.name, "--config", str(config),
                   "--kubeconfig", str(self._root_kubeconfig), "--wait", "180s", timeout=900, stream=True)
        self._write_kubeconfig()
        self._sudo("rm", "-f", str(self._root_kubeconfig), f"{self._root_kubeconfig}.lock")
        self._attach_to_bridge()
        self._advertise_bridge_address()
        self._point_cluster_at_bridge()
        self.log(f"kind cluster {self.spec.name} is up; the VM reaches its API server at {self.spec.api_server}")

    def ensure(self) -> None:
        """Bring the cluster back after a host restart, or check it's whole: start the kind node,
        restore the bridge, the TAP, and the rule, reattach the node, and have it advertise its
        bridge address again. Each part is left alone if it's as it should be."""
        if not self.exists():
            raise StepError(f"there's no kind cluster {self.spec.name}; create it")
        self._ensure_network()
        if self._container("Running") != "true":
            self.log(f"starting {self.spec.node_container}")
            self._sudo("podman", "start", self.spec.node_container)
        self._write_kubeconfig()
        self._attach_to_bridge()
        self._advertise_bridge_address()
        self._wait_for_api_server(180)
        self.wait_ready(self.spec.node_container, 300)
        if self._kubectl("-n", CONTROLLER_NAMESPACE, "get", "deployment", CONTROLLER, check=False).returncode == 0:
            self._kubectl("-n", CONTROLLER_NAMESPACE, "rollout", "status", f"deployment/{CONTROLLER}",
                          "--timeout=180s", timeout=240)
        self.log(f"kind cluster {self.spec.name} is up at {self.spec.api_server}")

    def delete(self) -> list[str]:
        """Remove the cluster, then the network, as far as possible, and return what failed or is
        left. It never raises, so down can go on."""
        failures: list[str] = []

        def attempt(what: str, action: Callable[[], None]) -> None:
            try:
                action()
            except Exception as error:  # best effort: report it, and go on
                failures.append(f"{what}: {error}")

        def delete_cluster() -> None:
            result = self._kind("delete", "cluster", "--name", self.spec.name,
                                "--kubeconfig", str(self._root_kubeconfig), check=False)
            self._sudo("rm", "-f", str(self._root_kubeconfig), f"{self._root_kubeconfig}.lock", check=False)
            if result.returncode != 0:
                raise _command_error(result.args, result)
            if self.exists():
                raise StepError(f"kind cluster {self.spec.name} is still there")

        def delete_network() -> None:
            failures.extend(self._network_down())

        def network_gone() -> None:
            names = links_by_name(self._links())
            left = [name for name in (self.spec.bridge, self.spec.tap, self.spec.veth_host) if name in names]
            if left:
                failures.append(f"these interfaces are still there: {', '.join(left)}")
            if self._forward_rules():
                failures.append(f"the FORWARD rule for {self.spec.bridge} is still there")

        self.log(f"deleting kind cluster {self.spec.name} and its network")
        attempt("deleting the kind cluster", delete_cluster)
        attempt("deleting the network", delete_network)
        attempt("checking the network is gone", network_gone)
        attempt("removing the kubeconfig", lambda: self.kubeconfig.unlink(missing_ok=True))
        return failures

    # The controller

    def _manifest_objects(self, *source: str) -> list[dict[str, Any]]:
        """The objects of manifests, as kubectl reads them, so they can be labelled and edited
        before they're applied, once."""
        return kubectl_items(self._kubectl("create", "--dry-run=client", "-o", "json", *source).stdout or "")

    def _unbounded_dir(self) -> Path:
        """The unbounded module this repository builds with, which has the MachineOperation CRDs."""
        result = self._run(["go", "-C", str(self.repo), "mod", "download", "-json", "github.com/Azure/unbounded"],
                           timeout=600)
        try:
            return Path(json.loads(result.stdout)["Dir"])
        except (ValueError, KeyError, TypeError) as error:
            raise StepError(f"go mod download printed no directory for github.com/Azure/unbounded: {error}") from error

    def build_and_deploy_controller(self) -> None:
        """Build aks-flex-controller from the repository, load its image into kind, and deploy it
        from hack/controller-deployment, with its CSR approver, the MachineOperation CRDs, and the
        bootstrap-token RBAC scripts/aks-flex-config applies."""
        context = self._private_dir("controller")
        binary = context / CONTROLLER
        self.log(f"building {CONTROLLER}")
        self._run(["env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "go", "-C", str(self.repo), "build",
                   "-o", str(binary), f"./cmd/{CONTROLLER}"], timeout=1800)
        digest = file_sha256(binary)
        (context / "Containerfile").write_text(CONTROLLER_CONTAINERFILE)
        image = self.controller_image
        previous = self._run(["podman", "image", "inspect", "--format", "{{.Id}}", image], check=False)
        built = self._run(["podman", "build", "-q", "-t", image, str(context)], timeout=600)
        old_id, new_id = (previous.stdout or "").strip(), last_lines(built.stdout, 1).strip()
        if previous.returncode == 0 and old_id and not old_id.startswith(new_id):
            # The build moved the tag; what it left is untagged, and nothing else uses it.
            self._run(["podman", "rmi", old_id], check=False)
        archive = self.work_dir / f"{CONTROLLER}.tar"
        archive.unlink(missing_ok=True)
        self._run(["podman", "save", "-q", "-o", str(archive), image], timeout=600)
        self.log(f"loading {image} into kind cluster {self.spec.name}")
        try:
            self._kind("load", "image-archive", str(archive), "--name", self.spec.name, timeout=600)
        finally:
            archive.unlink(missing_ok=True)

        crds = self._unbounded_dir() / "deploy" / "machina" / "crd"
        objects = self._manifest_objects(*(arg for crd in MACHINE_CRDS for arg in ("-f", str(crds / crd))))
        objects += node_rbac()
        objects += self._manifest_objects("-k", str(self.repo / "hack" / "controller-deployment"))
        self.log(f"deploying {CONTROLLER} ({digest[:12]})")
        self._kubectl("apply", "-f", "-", input=json.dumps(controller_manifest(objects, image, digest)))
        self._kubectl("-n", CONTROLLER_NAMESPACE, "rollout", "status", f"deployment/{CONTROLLER}",
                      "--timeout=180s", timeout=240)
        if not self._poll(60, lambda: self._kubectl("get", "--raw", f"{PROXY_PATH}/healthz", check=False,
                                                    timeout=30).returncode == 0, 3):
            raise StepError(f"{CONTROLLER}'s machine endpoint, {PROXY_PATH}, didn't answer within 60s")

    # What the VM's agent needs

    def kubernetes_version(self) -> str:
        return parse_server_version(self._kubectl("version", "-o", "json").stdout or "")

    def ca_pem(self) -> str:
        data = self._kubectl("config", "view", "--minify", "--raw", "-o",
                             "jsonpath={.clusters[0].cluster.certificate-authority-data}").stdout or ""
        try:
            pem = base64.b64decode(data.strip(), validate=True).decode()
        except ValueError as error:
            raise StepError(f"{self.kubeconfig} has no certificate authority data: {error}") from error
        if "BEGIN CERTIFICATE" not in pem:
            raise StepError(f"{self.kubeconfig}'s certificate authority data isn't a PEM certificate")
        return pem

    def dns_service_ip(self) -> str:
        """The cluster DNS service's address: the agent config's networking.dnsServiceIP."""
        return (self._kubectl("-n", "kube-system", "get", "service", "kube-dns", "-o",
                              "jsonpath={.spec.clusterIP}").stdout or "").strip()

    def identity(self) -> dict[str, str]:
        """What tells this cluster from another made under the same name."""
        uid = (self._kubectl("get", "namespace", "kube-system", "-o", "jsonpath={.metadata.uid}").stdout or "").strip()
        return {"uid": uid, "caSha256": hashlib.sha256(self.ca_pem().encode()).hexdigest()}

    def create_bootstrap_token(self, ttl: str = "24h") -> str:
        """A bootstrap token for the node, in the group whose RBAC lets it join. It's created
        rather than applied, so it isn't copied into a last-applied annotation."""
        expiration = datetime.datetime.now(datetime.timezone.utc) + parse_duration(ttl)
        token_id, token_secret = secrets.token_hex(3), secrets.token_hex(8)
        secret = bootstrap_token_secret(token_id, token_secret, expiration)
        self._kubectl("create", "-f", "-", input=json.dumps(secret))
        self.log(f"created bootstrap token {token_id}, which expires in {ttl}")
        return f"{token_id}.{token_secret}"

    def register_machine(self, node_name: str, settings_version: str = "1") -> None:
        """Add or update the node in the controller's machine store, a ConfigMap of ARM machine JSON
        by name."""
        machine = machine_json(node_name, self.kubernetes_version(), settings_version)
        self._kubectl("-n", CONTROLLER_NAMESPACE, "patch", "configmap", MACHINE_CONFIGMAP, "--type=merge",
                      "-p", json.dumps({"data": {f"{node_name}.json": json.dumps(machine)}}))
        self.log(f"registered machine {node_name} (settings version {settings_version})")

    # Nodes

    def node(self, name: str) -> dict[str, Any] | None:
        return self._kubectl_json("get", "node", name, "--ignore-not-found")

    def node_ready(self, name: str) -> bool:
        try:
            node = self.node(name)
        except StepError:
            return False
        return condition(node, "Ready").get("status") == "True"

    def wait_ready(self, name: str, timeout: float) -> None:
        self.log(f"waiting for node {name} to be Ready")
        if self._poll(timeout, lambda: self.node_ready(name), 5):
            self.log(f"node {name} is Ready")
            return
        try:
            detail = describe_conditions(self.node(name))
        except StepError as error:
            detail = str(error)
        raise StepError(f"node {name} wasn't Ready within {int(timeout)}s: {detail}")

    def lease_renew_time(self, name: str) -> datetime.datetime | None:
        return parse_lease_renew_time(self._kubectl_json("-n", "kube-node-lease", "get", "lease", name,
                                                         "--ignore-not-found"))

    def record(self, name: str, prefix: str) -> dict[str, Any]:
        """What reset_to returns the node to, and the cluster it's in."""
        node = self.node(name)
        if node is None:
            raise StepError(f"there's no node {name} to record")
        return {**node_record(node, prefix), "cluster": self.identity()}

    def reset_to(self, name: str, record: dict[str, Any], prefix: str) -> None:
        """Return the node to record, and delete the pods and MachineOperations the harness made,
        apart from the probe's pod."""
        wanted_cluster = record.get("cluster")
        if wanted_cluster and self.identity() != wanted_cluster:
            raise StepError(f"kind cluster {self.spec.name} was recreated since node {name} was recorded; rejoin it")
        for attempt in range(5):
            node = self.node(name)
            if node is None:
                raise StepError(f"node {name} is gone; rejoin it")
            patch = reset_patch(node, record, prefix)
            if patch is None:
                break
            result = self._kubectl("patch", "node", name, "--type=merge", "-p", json.dumps(patch), check=False)
            if result.returncode == 0:
                self.log(f"reset node {name}'s {prefix}/ annotations, cordon, and taints")
                break
            # A Node changes under the kubelet and the node controller; a conflict is read again.
            if "the object has been modified" not in (result.stderr or "") or attempt == 4:
                raise _command_error(result.args, result)
        selector = f"{OWNED_SELECTOR},{PROBE_LABEL}!={PROBE_NAME}"
        deleted = self._kubectl("delete", "pods", "--all-namespaces", "-l", selector, "--ignore-not-found",
                                "--wait=true", "--timeout=90s", check=False, timeout=150)
        if deleted.returncode != 0:
            # A pod on a node that's down never finishes terminating.
            self._kubectl("delete", "pods", "--all-namespaces", "-l", selector, "--ignore-not-found",
                          "--grace-period=0", "--force")
        operations = self._kubectl("delete", "machineoperations", "-l", OWNED_SELECTOR, "--ignore-not-found",
                                   "--wait=true", "--timeout=60s", check=False, timeout=120)
        if operations.returncode != 0 and "the server doesn't have a resource type" not in (operations.stderr or ""):
            raise _command_error(operations.args, operations)

    def cordon(self, name: str) -> None:
        self._kubectl("cordon", name)

    def drain(self, name: str, timeout: float = 300) -> None:
        self.log(f"draining node {name}")
        self._kubectl("drain", name, "--ignore-daemonsets", "--delete-emptydir-data", "--force",
                      f"--timeout={int(timeout)}s", timeout=timeout + 60)

    def uncordon(self, name: str) -> None:
        self._kubectl("uncordon", name)

    # Checks

    def ensure_probe(self, node_name: str, namespace: str = "acl-abupdate") -> None:
        self._kubectl("apply", "-f", "-", input=json.dumps(probe_manifest(node_name, namespace)))

    def probe_findings(self, node_name: str, namespace: str = "acl-abupdate",
                       timeout: float = 300) -> list[Finding]:
        """The probe's pod runs on the node, and the API server reaches the node's kubelet for its
        logs and for exec. Each finding is (ok, description, detail)."""
        found: dict[str, Any] = {"pod": None, "detail": ""}

        def running() -> bool:
            result = self._kubectl("-n", namespace, "get", "pods", "-l", f"{PROBE_LABEL}={PROBE_NAME}",
                                   "-o", "json", check=False, timeout=30)
            if result.returncode != 0:
                found["detail"] = last_lines(result.stderr, 3)
                return False
            try:
                found["pod"], found["detail"] = probe_pod(json.loads(result.stdout or "{}"), node_name)
            except json.JSONDecodeError as error:
                found["detail"] = f"kubectl printed no JSON: {error}"
            return found["pod"] is not None

        self._poll(timeout, running, 5)
        pod = found["pod"]
        findings: list[Finding] = [(pod is not None, f"the probe pod is Running and Ready on {node_name}",
                                    found["detail"])]
        logs_description = f"kubectl logs reaches {node_name}'s kubelet"
        exec_description = f"kubectl exec reaches {node_name}'s kubelet"
        if pod is None:
            findings += [(False, logs_description, "there's no probe pod to reach it through"),
                         (False, exec_description, "there's no probe pod to reach it through")]
            return findings
        name = pod["metadata"]["name"]
        logs = self._kubectl("-n", namespace, "logs", name, check=False, timeout=60)
        findings.append((logs.returncode == 0 and PROBE_OUTPUT in (logs.stdout or ""), logs_description,
                         last_lines(logs.stderr, 3) or f"its log has no {PROBE_OUTPUT}"))
        executed = self._kubectl("-n", namespace, "exec", name, "--", "echo", PROBE_EXEC_OUTPUT, check=False,
                                 timeout=60)
        findings.append((executed.returncode == 0 and PROBE_EXEC_OUTPUT in (executed.stdout or ""),
                         exec_description, last_lines(executed.stderr, 3) or f"it printed no {PROBE_EXEC_OUTPUT}"))
        return findings

    def node_findings(self, name: str, *, expected_ip: str,
                      since: datetime.datetime | None = None) -> list[Finding]:
        """The node is Ready, at expected_ip, its kubelet is the server's version, and, with since,
        it renewed its Lease after then. Each finding is (ok, description, detail)."""
        try:
            node = self.node(name)
            server_version = self.kubernetes_version()
            renew_time = self.lease_renew_time(name) if since is not None else None
        except StepError as error:
            return [(False, f"node {name} can be read", str(error))]
        return node_findings_from(node, name=name, expected_ip=expected_ip, server_version=server_version,
                                  renew_time=renew_time, since=since)

    def diagnostics(self, node_name: str, directory: Path) -> None:
        """Write what tells why the node or the cluster misbehaved to directory, as far as it can be
        read. Bootstrap tokens and private keys are redacted; Secrets aren't read."""
        collect = {
            "nodes.txt": ["get", "nodes", "-o", "wide"],
            "node.yaml": ["get", "node", node_name, "-o", "yaml"],
            "node-describe.txt": ["describe", "node", node_name],
            "lease.yaml": ["-n", "kube-node-lease", "get", "lease", node_name, "-o", "yaml"],
            "events.txt": ["get", "events", "--all-namespaces", "--sort-by=.lastTimestamp"],
            "pods-on-node.txt": ["get", "pods", "--all-namespaces", "-o", "wide",
                                 "--field-selector", f"spec.nodeName={node_name}"],
            "controller.log": ["-n", CONTROLLER_NAMESPACE, "logs", f"deployment/{CONTROLLER}", "--all-containers",
                               "--tail=1000"],
            "machineoperations.yaml": ["get", "machineoperations", "-o", "yaml"],
        }
        try:
            directory.mkdir(parents=True, exist_ok=True)
        except OSError as error:
            self.log(f"warning: can't write the cluster's diagnostics to {directory}: {error}")
            return
        for filename, args in collect.items():
            try:
                result = self._kubectl(*args, check=False, timeout=60)
                text = result.stdout or ""
                if filename == "events.txt":
                    lines = text.splitlines()
                    text = "\n".join(lines[:1] + lines[1:][-200:]) + "\n"
                if result.returncode != 0:
                    text += f"\n# kubectl exited {result.returncode}\n{result.stderr or ''}"
                (directory / filename).write_text(redact(text))
            except Exception as error:  # best effort: one file's failure doesn't stop the others
                self.log(f"warning: couldn't write {directory / filename}: {error}")
