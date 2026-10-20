#!/usr/bin/env python3
"""Prototype: run the AKS Flex Node agent as a privileged containerd container on Azure Container Linux.

    hack/acl-container/harness.py up [--image ACL.qcow2]   build and push the agent image, boot the VM
    hack/acl-container/harness.py push                     rebuild and push the image, restart the container
    hack/acl-container/harness.py check                    run the feasibility checks
    hack/acl-container/harness.py status                   show the VM, agent container, and machines
    hack/acl-container/harness.py ssh [COMMAND]            shell on the VM, or run a command there
    hack/acl-container/harness.py agent [COMMAND]          shell in the agent container, or run a command there
    hack/acl-container/harness.py console                  follow the VM's serial console
    hack/acl-container/harness.py down                     stop the VM, remove the network, registry, and state

The VM is provisioned only by the Ignition config that agent-container.bu renders, plus harness
access: an SSH user, a hostname, and a static address. The agent image is built from this
repository with image/Containerfile and pushed to a registry on the bridge, which the VM's
containerd pulls from. See README.md for usage and DESIGN.md for what this proves.
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import http.server
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import textwrap
import threading
import time
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import aclimage  # noqa: E402
import ukiboot  # noqa: E402

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
DEFAULT_STATE = REPO / ".vm" / "acl-container"
STATE = Path(os.environ.get("ACLC_STATE_DIR", str(DEFAULT_STATE)))
# up marks the state directory it makes, which down removes only if it's the default or marked:
# ACLC_STATE_DIR could name any directory.
STATE_MARKER = STATE / ".aclc-state"
IMAGE_DIR = Path(os.environ.get("ACL_IMAGE_DIR", str(REPO / ".vm" / "acl-images")))

# The subnet and interface names are this harness's own, so other local VM harnesses can run
# alongside it.
SUBNET = os.environ.get("ACLC_SUBNET", "192.168.112")
GATEWAY = f"{SUBNET}.1"
VM_IP = f"{SUBNET}.10"
BRIDGE = "virbr-aclc"
TAP = "tap-aclc"
DNS = os.environ.get("ACLC_DNS", "8.8.8.8 8.8.4.4").split()
SERVE_PORT = int(os.environ.get("ACLC_SERVE_PORT", "8298"))
VM_MEMORY = os.environ.get("ACLC_VM_MEMORY", "4096")
VM_CPUS = os.environ.get("ACLC_VM_CPUS", "4")
VM_MIN_DISK = 20 * 1024**3
HOSTNAME = "acl-flex-proto"
SSH_USER = "core"

ENGINE = os.environ.get("ACLC_CONTAINER_ENGINE") or ("podman" if shutil.which("podman") else "docker")
REGISTRY_NAME = "aclc-registry"
REGISTRY_IMAGE = os.environ.get("ACLC_REGISTRY_IMAGE", "docker.io/library/registry:2")
REGISTRY_PORT = int(os.environ.get("ACLC_REGISTRY_PORT", "5099"))
AGENT_REPOSITORY = "aks-flex-node-agent"
AGENT_TAG = os.environ.get("ACLC_AGENT_TAG", "proto")
LOCAL_AGENT_IMAGE = f"localhost/{AGENT_REPOSITORY}:{AGENT_TAG}"
# The reference the VM pulls. The registry is plain HTTP and only listens on the bridge.
VM_AGENT_IMAGE = f"{GATEWAY}:{REGISTRY_PORT}/{AGENT_REPOSITORY}:{AGENT_TAG}"
BUTANE_TEMPLATE = HERE / "agent-container.bu"
BUTANE_IMAGE = os.environ.get("ACLC_BUTANE_IMAGE", "quay.io/coreos/butane:v0.29.0")

# Must match agent-container.bu, and the agent's unit, which start installs.
AGENT_UNIT = "aks-flex-node-agent.service"
BOOTSTRAP_UNIT = "aks-flex-node-bootstrap.service"
RESET_UNIT = "aks-flex-node-reset.service"
CTR_NAMESPACE = "aks-flex"
CONTAINER_ID = "aks-flex-node"
CTR = f"ctr --namespace {CTR_NAMESPACE}"

IGNITION_NAME = "config.ign"
# bootstrap's inputs, which the bootstrap unit waits for: the variables bootstrap.sh takes, and
# the base config they name, which holds the bootstrap token.
BOOTSTRAP_INPUTS = "/etc/aks-flex-node/bootstrap.env"
BASE_CONFIG = "/etc/aks-flex-node/base-config.json"
# The image the agent's units run, when not the install's: AgentUpgrade selects it, and bootstrap
# can be given one.
AGENT_IMAGE_SELECTION = "/etc/aks-flex-node/agent-image.env"
# An AgentUpgrade's durable record, from its request until the restarted daemon publishes it.
AGENT_UPGRADE_SIGNAL = "/etc/aks-flex-node/agent-upgrade-signal.json"
SETTINGS = "/etc/sysconfig/aks-flex-node-container"

# The kind cluster up creates for the agent to join, on root Podman. Its node joins the VM's
# bridge at KIND_BRIDGE_IP, where the VM reaches the API server.
CLUSTER = os.environ.get("ACLC_KIND_CLUSTER", "aclc")
KIND_NODE = f"{CLUSTER}-control-plane"
KIND_NODE_IMAGE = os.environ.get("ACLC_KIND_NODE_IMAGE", "")
KIND_BRIDGE_IP = f"{SUBNET}.2"
API_SERVER = f"{KIND_BRIDGE_IP}:6443"
VETH_HOST = "veth-aclc-kind"
VETH_KIND = "eth-aclc"
KUBECONFIG = STATE / "kubeconfig"
CLUSTER_MARKER = STATE / "cluster"
CONTROLLER_IMAGE = "localhost/aks-flex-controller:aclc"
# main's in-cluster machine endpoint, through the API server's service proxy.
PROXY_PATH = "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy"
BOOTSTRAP_GROUP = "system:bootstrappers:aks-flex-node"
AGENT_POOL = "aclc"
FAKE_CLUSTER_ID = ("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/aclc"
                   "/providers/Microsoft.ContainerService/managedClusters/aclc")

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
    print(f"[aclc] {message}", flush=True)


def die(message: str) -> None:
    print(f"[aclc] error: {message}", file=sys.stderr, flush=True)
    sys.exit(1)


def run(cmd: list[str], *, check: bool = True, capture: bool = False, input: str | None = None,
        cwd: Path | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(cmd, check=False, text=True, input=input, cwd=cwd,
                            env={**os.environ, **env} if env else None,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if check and result.returncode != 0:
        detail = f": {result.stderr.strip()}" if capture and result.stderr else ""
        die(f"{' '.join(cmd)} exited {result.returncode}{detail}")
    return result


def capture(cmd: list[str], **kwargs) -> str:
    return run(cmd, capture=True, **kwargs).stdout.strip()


def sudo(*cmd: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    return run(["sudo", *cmd], check=check, capture=not check)


def eventually(probe, timeout: float, interval: float = 5) -> bool:
    """Poll probe until it holds or timeout seconds pass, and return whether it held."""
    deadline = time.monotonic() + timeout
    while not probe():
        if time.monotonic() >= deadline:
            return False
        time.sleep(interval)
    return True


def wait_until(description: str, timeout: float, probe, interval: float = 5) -> None:
    if not eventually(probe, timeout, interval):
        die(f"timed out after {int(timeout)}s waiting for {description}")


# ---------------------------------------------------------------------------------------------
# Agent image and registry


def build_agent_image() -> None:
    commit = capture(["git", "rev-parse", "--short", "HEAD"], cwd=REPO) if (REPO / ".git").exists() else "unknown"
    dirty = run(["git", "diff", "--quiet", "HEAD"], check=False, cwd=REPO).returncode != 0
    log(f"building {LOCAL_AGENT_IMAGE} with {ENGINE}")
    run([ENGINE, "build", "-f", str(HERE / "image" / "Containerfile"),
         "--build-arg", f"GIT_COMMIT={commit}{'-dirty' if dirty else ''}",
         "-t", LOCAL_AGENT_IMAGE, str(REPO)])


def registry_running() -> bool:
    result = run([ENGINE, "inspect", "-f", "{{.State.Running}}", REGISTRY_NAME], check=False, capture=True)
    return result.returncode == 0 and result.stdout.strip() == "true"


def start_registry() -> None:
    """Run a plain-HTTP registry on the bridge, and on loopback, where docker pushes to it
    without an insecure-registries entry."""
    if registry_running():
        return
    run([ENGINE, "rm", "-f", REGISTRY_NAME], check=False, capture=True)
    log(f"starting registry {REGISTRY_NAME} on {GATEWAY}:{REGISTRY_PORT}")
    run([ENGINE, "run", "-d", "--name", REGISTRY_NAME,
         "-p", f"127.0.0.1:{REGISTRY_PORT}:5000", "-p", f"{GATEWAY}:{REGISTRY_PORT}:5000",
         REGISTRY_IMAGE], capture=True)


def stop_registry() -> None:
    if shutil.which(ENGINE):
        run([ENGINE, "rm", "-f", REGISTRY_NAME], check=False, capture=True)


def push_agent_image() -> str:
    """Push the agent image and return its digest."""
    return push_image(LOCAL_AGENT_IMAGE, AGENT_TAG).rpartition("@")[2]


def push_image(local: str, tag: str) -> str:
    """Push a local image to the bridge's registry as AGENT_REPOSITORY:tag, and return the
    reference the VM pulls it by, pinned by digest."""
    start_registry()
    vm_image = f"{GATEWAY}:{REGISTRY_PORT}/{AGENT_REPOSITORY}:{tag}"
    digest_file = STATE / ("agent-image.digest" if tag == AGENT_TAG else f"{tag}.digest")
    if ENGINE == "podman":
        run(["podman", "push", "--quiet", "--tls-verify=false", "--digestfile", str(digest_file),
             local, vm_image])
        digest = digest_file.read_text().strip()
    else:
        loopback = f"127.0.0.1:{REGISTRY_PORT}/{AGENT_REPOSITORY}:{tag}"
        run(["docker", "tag", local, loopback])
        run(["docker", "push", "--quiet", loopback])
        digest = capture(["docker", "inspect", "-f", "{{index .RepoDigests 0}}", loopback]).split("@")[-1]
        digest_file.write_text(digest + "\n")
    log(f"pushed {vm_image}@{digest}")
    return f"{GATEWAY}:{REGISTRY_PORT}/{AGENT_REPOSITORY}@{digest}"


# ---------------------------------------------------------------------------------------------
# Host networking


def create_network() -> None:
    log(f"creating bridge {BRIDGE} ({GATEWAY}/24) and {TAP}")
    delete_network()
    sudo("ip", "link", "add", BRIDGE, "type", "bridge")
    sudo("ip", "addr", "add", f"{GATEWAY}/24", "dev", BRIDGE)
    sudo("ip", "link", "set", BRIDGE, "up")
    # Owned by this user, so QEMU opens it without CAP_NET_ADMIN.
    sudo("ip", "tuntap", "add", "dev", TAP, "mode", "tap", "user", str(os.getuid()))
    sudo("ip", "link", "set", TAP, "master", BRIDGE)
    sudo("ip", "link", "set", TAP, "up")
    if shutil.which("nmcli"):
        for iface in (BRIDGE, TAP):
            sudo("nmcli", "device", "set", iface, "managed", "no", check=False)

    # The VM reaches the internet for the agent's rootfs images. Docker turns forwarding on
    # itself; without it, it is usually off.
    forwarding = Path("/proc/sys/net/ipv4/ip_forward").read_text().strip()
    saved = STATE / "ip_forward.saved"
    if forwarding != "1":
        if not saved.exists():
            saved.write_text(forwarding + "\n")
        log("enabling IPv4 forwarding (down restores it)")
        sudo("sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
    sudo("iptables", "-I", "FORWARD", "-i", BRIDGE, "-j", "ACCEPT")
    sudo("iptables", "-I", "FORWARD", "-o", BRIDGE, "-j", "ACCEPT")
    sudo("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", f"{SUBNET}.0/24",
         "!", "-d", f"{SUBNET}.0/24", "-j", "MASQUERADE")
    if docker_uses_nft():
        sudo("nft", "insert", "rule", "ip", "filter", "DOCKER-USER", "iifname", BRIDGE, "accept")
        sudo("nft", "insert", "rule", "ip", "filter", "DOCKER-USER", "oifname", BRIDGE, "accept")


def docker_uses_nft() -> bool:
    return bool(shutil.which("nft")) and sudo("nft", "list", "chain", "ip", "filter", "DOCKER-USER",
                                              check=False).returncode == 0


def delete_network() -> None:
    for iface in (TAP, VETH_HOST, BRIDGE):
        sudo("ip", "link", "delete", iface, check=False)
    for chain_opt in ("-i", "-o"):
        while sudo("iptables", "-D", "FORWARD", chain_opt, BRIDGE, "-j", "ACCEPT",
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
                sudo("nft", "delete", "rule", "ip", "filter", "DOCKER-USER", "handle", handle, check=False)


def restore_forwarding() -> None:
    saved = STATE / "ip_forward.saved"
    if saved.exists():
        value = saved.read_text().strip()
        log(f"restoring IPv4 forwarding to {value}")
        sudo("sysctl", "-q", "-w", f"net.ipv4.ip_forward={value}", check=False)
        saved.unlink()


# ---------------------------------------------------------------------------------------------
# Cluster
#
# A kind cluster for the agent to join, with main's in-cluster controller as its machine
# endpoint and a bootstrap token for the node, so no Azure resources are involved. kind runs on
# root Podman, since attaching its node to the VM's bridge needs root anyway.


def kind(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    binary = shutil.which("kind")
    if binary is None:
        die("kind is needed for the cluster the agent joins")
        raise AssertionError
    return run(["sudo", "env", "KIND_EXPERIMENTAL_PROVIDER=podman", binary, *args],
               check=check, capture=not check)


def kubectl(*args: str, check: bool = True, input: str | None = None,
            quiet: bool = False) -> subprocess.CompletedProcess[str]:
    return run(["kubectl", "--kubeconfig", str(KUBECONFIG), *args], check=check, input=input,
               capture=quiet)


def kubectl_out(*args: str) -> str:
    return capture(["kubectl", "--kubeconfig", str(KUBECONFIG), *args])


def cluster_exists() -> bool:
    return CLUSTER in kind("get", "clusters", check=False).stdout.split()


def cluster_enabled() -> bool:
    return CLUSTER_MARKER.exists()


def create_cluster() -> None:
    if cluster_exists():
        log(f"deleting kind cluster {CLUSTER}, left from an earlier run")
        kind("delete", "cluster", "--name", CLUSTER)
    # Patches replace lists, so the certificate keeps kind's own names too.
    config = STATE / "kind.yaml"
    config.write_text(textwrap.dedent(f"""\
        kind: Cluster
        apiVersion: kind.x-k8s.io/v1alpha4
        kubeadmConfigPatches:
          - |
            kind: ClusterConfiguration
            apiServer:
              certSANs: ["localhost", "127.0.0.1", "{KIND_BRIDGE_IP}"]
    """))
    log(f"creating kind cluster {CLUSTER} on root Podman")
    cmd = ["create", "cluster", "--name", CLUSTER, "--config", str(config),
           "--kubeconfig", str(KUBECONFIG), "--wait", "180s"]
    if KIND_NODE_IMAGE:
        cmd += ["--image", KIND_NODE_IMAGE]
    kind(*cmd)
    sudo("chown", f"{os.getuid()}:{os.getgid()}", str(KUBECONFIG))
    attach_kind_to_bridge()
    advertise_bridge_address()
    point_cluster_at_bridge()


def attach_kind_to_bridge() -> None:
    """Give the kind node an address on the VM's bridge, where the VM reaches the API server and
    the API server reaches the node's kubelet."""
    log(f"attaching {KIND_NODE} to {BRIDGE} as {KIND_BRIDGE_IP}")
    pid = capture(["sudo", "podman", "inspect", KIND_NODE, "--format", "{{.State.Pid}}"])
    sudo("ip", "link", "delete", VETH_HOST, check=False)
    sudo("ip", "link", "add", VETH_HOST, "type", "veth", "peer", "name", VETH_KIND)
    sudo("ip", "link", "set", VETH_HOST, "master", BRIDGE)
    sudo("ip", "link", "set", VETH_HOST, "up")
    sudo("ip", "link", "set", VETH_KIND, "netns", pid)
    sudo("nsenter", "-t", pid, "-n", "ip", "addr", "add", f"{KIND_BRIDGE_IP}/24", "dev", VETH_KIND)
    sudo("nsenter", "-t", pid, "-n", "ip", "link", "set", VETH_KIND, "up")


def advertise_bridge_address() -> None:
    """Make the control plane's node advertise its bridge address. kindnet on the agent's node
    routes the control plane's pod CIDR through it, and the VM can't reach the node's Podman
    address."""
    script = textwrap.dedent(f"""\
        set -eu
        . /var/lib/kubelet/kubeadm-flags.env
        set -- $KUBELET_KUBEADM_ARGS
        args=""
        for arg do
          case "$arg" in
            --node-ip=*) ;;
            *) args="$args $arg" ;;
          esac
        done
        printf 'KUBELET_KUBEADM_ARGS="%s --node-ip={KIND_BRIDGE_IP}"\\n' "${{args# }}" \\
          >/var/lib/kubelet/kubeadm-flags.env
        systemctl restart kubelet
    """)
    run(["sudo", "podman", "exec", KIND_NODE, "sh", "-c", script])

    def advertised() -> bool:
        result = kubectl("get", "node", KIND_NODE, "-o",
                         "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}",
                         check=False, quiet=True)
        return result.returncode == 0 and result.stdout.split() == [KIND_BRIDGE_IP]

    wait_until(f"{KIND_NODE} to advertise {KIND_BRIDGE_IP}", 120, advertised, 3)


def point_cluster_at_bridge() -> None:
    """kindnet and kube-proxy on the agent's node would otherwise reach the API server through the
    control plane's container hostname, which the VM can't resolve."""
    patch = {"spec": {"template": {"spec": {"containers": [
        {"name": "kindnet-cni", "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": API_SERVER}]}]}}}}
    kubectl("-n", "kube-system", "patch", "daemonset", "kindnet", "--type=strategic",
            "-p", json.dumps(patch))
    kubeconfig = kubectl_out("-n", "kube-system", "get", "configmap", "kube-proxy", "-o",
                             "jsonpath={.data.kubeconfig\\.conf}")
    lines = []
    for line in kubeconfig.splitlines():
        if line.strip().startswith("server:"):
            line = f"{line[:len(line) - len(line.lstrip())]}server: https://{API_SERVER}"
        lines.append(line)
    kubectl("-n", "kube-system", "patch", "configmap", "kube-proxy", "--type=merge",
            "-p", json.dumps({"data": {"kubeconfig.conf": "\n".join(lines) + "\n"}}))
    kubectl("-n", "kube-system", "rollout", "restart", "daemonset/kube-proxy")
    kubectl("-n", "kube-system", "delete", "pod", "-l", "app=kindnet", "--wait=false")
    kubectl("-n", "kube-system", "rollout", "status", "daemonset/kindnet", "--timeout=180s")
    kubectl("-n", "kube-system", "rollout", "status", "daemonset/kube-proxy", "--timeout=180s")


# The bindings scripts/aks-flex-config applies for bootstrap-token nodes.
NODE_RBAC = f"""
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


def install_controller() -> None:
    """Deploy main's aks-flex-controller from hack/controller-deployment, with a locally built
    image, and the MachineOperation CRDs the daemon watches."""
    log("deploying aks-flex-controller")
    context = STATE / "controller"
    context.mkdir(exist_ok=True)
    run(["go", "build", "-o", str(context / "aks-flex-controller"), "./cmd/aks-flex-controller"],
        cwd=REPO, env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"})
    (context / "Containerfile").write_text(textwrap.dedent("""\
        FROM scratch
        COPY aks-flex-controller /usr/local/bin/aks-flex-controller
        USER 65532:65532
        ENTRYPOINT ["/usr/local/bin/aks-flex-controller"]
    """))
    run([ENGINE, "build", "-q", "-t", CONTROLLER_IMAGE, str(context)], capture=True)
    archive = context / "aks-flex-controller.tar"
    archive.unlink(missing_ok=True)
    run([ENGINE, "save", "-o", str(archive), CONTROLLER_IMAGE], capture=True)
    kind("load", "image-archive", str(archive), "--name", CLUSTER)

    unbounded = capture(["go", "list", "-m", "-f", "{{.Dir}}", "github.com/Azure/unbounded"], cwd=REPO)
    for crd in ("unbounded-cloud.io_machineoperations.yaml",
                "unbounded-cloud.io_machineoperationcredentials.yaml"):
        kubectl("apply", "-f", f"{unbounded}/deploy/machina/crd/{crd}")
    kubectl("apply", "-f", "-", input=NODE_RBAC)
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
    kubectl("get", "--raw", f"{PROXY_PATH}/healthz", quiet=True)


def kubernetes_version() -> str:
    version = json.loads(kubectl_out("version", "-o", "json"))["serverVersion"]["gitVersion"]
    return version.removeprefix("v").split("+", 1)[0]


def register_machine(version: str, settings_version: str = "1") -> None:
    """Add or update the node in the controller's machine store, a ConfigMap of ARM machine JSON
    by name. Its Kubernetes version has to match the agent config's. Its eTag is the goal's
    settings version, which a repave changes."""
    machine = {
        "id": f"{FAKE_CLUSTER_ID}/agentPools/{AGENT_POOL}/machines/{HOSTNAME}",
        "name": HOSTNAME,
        "type": "Microsoft.ContainerService/managedClusters/agentPools/machines",
        "properties": {
            "eTag": settings_version,
            "provisioningState": "Succeeded",
            "kubernetes": {"orchestratorVersion": version, "nodeLabels": {}, "nodeTaints": []},
        },
    }
    kubectl("-n", "kube-system", "patch", "configmap", "aks-flex-machines", "--type=merge",
            "-p", json.dumps({"data": {f"{HOSTNAME}.json": json.dumps(machine)}}))


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


def agent_config(token: str, version: str) -> dict:
    """The agent's config, in main's format, for a bootstrap-token node that reads its machine from
    the in-cluster controller. bootstrap.sh starts from it."""
    ca = kubectl_out("config", "view", "--minify", "--raw", "-o",
                     "jsonpath={.clusters[0].cluster.certificate-authority-data}")
    dns_ip = kubectl_out("-n", "kube-system", "get", "service", "kube-dns", "-o",
                         "jsonpath={.spec.clusterIP}")
    return {
        "azure": {
            "subscriptionId": "00000000-0000-0000-0000-000000000000",
            "tenantId": "00000000-0000-0000-0000-000000000000",
            "resourceManagerEndpoint": "https://management.azure.com",
            "targetAgentPoolName": AGENT_POOL,
            "bootstrapToken": {"token": token},
            "arc": {"enabled": False},
            "targetCluster": {"resourceId": FAKE_CLUSTER_ID, "location": "local"},
        },
        "agent": {
            "logLevel": "info",
            "logDir": "/var/log/aks-flex-node",
            "nodeName": HOSTNAME,
            "machineClient": {"mode": "in-cluster", "endpointUrl": PROXY_PATH},
            "requireMachineRegistration": True,
        },
        "components": {"kubernetes": version},
        "networking": {"dnsServiceIP": dns_ip},
        "node": {"kubelet": {"clusterFQDN": API_SERVER, "caCertData": ca}},
    }


def bootstrap_inputs() -> dict[str, str]:
    """bootstrap's inputs, by path on the VM, for a bootstrap-token node: a base config with a new
    token, from which bootstrap.sh renders the agent's config, and the variable naming it."""
    config = agent_config(create_bootstrap_token(), kubernetes_version())
    return {
        BASE_CONFIG: json.dumps(config, indent=2) + "\n",
        BOOTSTRAP_INPUTS: f"AKS_FLEX_NODE_BASE_CONFIG_FILE={BASE_CONFIG}\n",
    }


def node_ready() -> bool:
    result = kubectl("get", "node", HOSTNAME, "-o",
                     "jsonpath={.status.conditions[?(@.type=='Ready')].status}",
                     check=False, quiet=True)
    return result.returncode == 0 and result.stdout.strip() == "True"


def wait_for_node_ready(timeout: float = 1200) -> None:
    log(f"waiting for node {HOSTNAME} to be Ready")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if node_ready():
            log(f"node {HOSTNAME} is Ready")
            return
        if ssh(f"systemctl show {AGENT_UNIT} -p ActiveState --value", check=False).stdout.strip() == "failed":
            break
        time.sleep(10)
    print(ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -n 60", check=False).stdout)
    die(f"node {HOSTNAME} did not become Ready")


def delete_cluster() -> None:
    if shutil.which("kind") and cluster_exists():
        kind("delete", "cluster", "--name", CLUSTER)


# ---------------------------------------------------------------------------------------------
# Ignition


def butane(template: Path, files: Path) -> dict:
    """Render a Butane config, as an operator does, from a directory of the files it names."""
    rendered = capture([ENGINE, "run", "--rm", "-i", "-v", f"{files}:/files:ro,z", BUTANE_IMAGE,
                        "--strict", "--files-dir", "/files"], input=template.read_text())
    return json.loads(rendered)


def render_ignition(with_agent: bool) -> dict:
    """Render agent-container.bu, from the one file it names. Without the agent, the VM gets only
    the harness's access."""
    if not with_agent:
        return {"ignition": {"version": "3.4.0"}}
    files = STATE / "butane-files"
    shutil.rmtree(files, ignore_errors=True)
    files.mkdir(mode=0o700)
    (files / "agent-container.env").write_text(textwrap.dedent(f"""\
        AKS_FLEX_NODE_IMAGE={VM_AGENT_IMAGE}
        AKS_FLEX_NODE_PULL_ARGS=--plain-http
    """))
    return butane(BUTANE_TEMPLATE, files)


def data_url(content: str) -> str:
    import base64
    return "data:;base64," + base64.b64encode(content.encode()).decode()


def mac_address() -> str:
    octets = [int(part) for part in VM_IP.split(".")]
    return f"52:54:00:{octets[1]:02x}:{octets[2]:02x}:{octets[3]:02x}"


def add_harness_access(doc: dict, ssh_public_key: str) -> dict:
    """Add what the harness needs to reach the VM. Nothing here affects the agent.

    A user Ignition config displaces the image's own systemd section, so the waagent mask it
    carries is restated. The core user is given in full, because a bare name leaves it on
    /sbin/nologin. The network unit matches on MAC, since interface names depend on the machine
    type. The hostname is written because the image leaves it as "localhost".
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
    """) + "".join(f"DNS={server}\n" for server in DNS)
    files = doc.setdefault("storage", {}).setdefault("files", [])
    files.append({"path": "/etc/hostname", "mode": 0o644, "overwrite": True,
                  "contents": {"source": data_url(f"{HOSTNAME}\n")}})
    files.append({"path": "/etc/systemd/network/10-acl-harness.network", "mode": 0o644,
                  "overwrite": True, "contents": {"source": data_url(network_unit)}})
    return doc


def add_containerd_labels(doc: dict) -> dict:
    """Label /var/lib/containerd as ACL's file_contexts has it, before containerd starts, as an
    ACL build that labels it would. ACL 3.0.20261019 builds it into the image unlabeled, so
    every container containerd starts stays kernel_t; labeled, the agent's run as spc_t, where
    ACL's SELinux rule for containers applies. Labeled once, the files containerd adds inherit
    the labels, so later boots change nothing."""
    unit = textwrap.dedent("""\
        [Unit]
        Description=Label /var/lib/containerd as file_contexts has it (AKS Flex Node harness)
        After=local-fs.target
        Before=containerd.service
        ConditionPathExists=/var/lib/containerd

        [Service]
        Type=oneshot
        RemainAfterExit=yes
        ExecStart=/usr/sbin/restorecon -R /var/lib/containerd

        [Install]
        WantedBy=containerd.service
    """)
    doc.setdefault("systemd", {}).setdefault("units", []).append(
        {"name": "aclc-label-containerd.service", "enabled": True, "contents": unit})
    return doc


def first_boot_kargs(config_url: str) -> str:
    """What the first boot's command line needs: where Ignition fetches its config, the
    initramfs address it fetches it over (named, since an empty device matches lo first), and a
    mask for the Azure build's metadata hostname service, which nothing answers under QEMU."""
    dns = ":".join(DNS[:2])
    return (f"ignition.config.url={config_url} "
            f"ip={VM_IP}::{GATEWAY}:24:{HOSTNAME}:eth0:none:{dns} "
            "rd.systemd.mask=flatcar-metadata-hostname.service")


class FileServer:
    """Serve an explicit set of files on the bridge, while the VM provisions."""

    def __init__(self, files: dict[str, Path]):
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

    # Ignition runs only while the image's first-boot addon exists, and ignition-quench deletes
    # it afterwards. Adding the harness's arguments to that addon, rather than booting the kernel
    # directly, keeps later boots from running Ignition again; see ukiboot.py.
    patched = ukiboot.patch_firstboot_cmdline(disk, first_boot_kargs(config_url))
    log(f"patched {patched.addon} ({patched.used}/{patched.capacity} bytes)")

    code, variables_template = ovmf_firmware()
    variables = STATE / "OVMF_VARS.fd"
    shutil.copyfile(variables_template, variables)

    pid_file, serial_log = STATE / "qemu.pid", STATE / "serial.log"
    serial_log.unlink(missing_ok=True)
    # cache=unsafe lets the host cache the guest's writes and ignores its flushes. The guest writes
    # whole images, as containerd pulls and unpacks the agent's and machines' images. It
    # risks only a disk QEMU dies holding, and this one is created for this VM and dropped with
    # it, by up again or down.
    run(["setsid", "qemu-system-x86_64",
         "-machine", "q35", "-cpu", "host", "-accel", "kvm",
         "-m", VM_MEMORY, "-smp", VM_CPUS,
         "-drive", f"if=pflash,format=raw,readonly=on,file={code}",
         "-drive", f"if=pflash,format=raw,file={variables}",
         "-drive", f"file={disk},format=qcow2,if=virtio,cache=unsafe",
         "-netdev", f"tap,id=net0,ifname={TAP},script=no,downscript=no",
         "-device", f"virtio-net-pci,netdev=net0,mac={mac_address()}",
         "-daemonize", "-pidfile", str(pid_file),
         "-serial", f"file:{serial_log}", "-display", "none"])
    log(f"VM started (pid {pid_file.read_text().strip()}, serial log {serial_log})")


def vm_pid() -> int | None:
    """The VM's QEMU process, from its PID file, while it runs. A PID can be reused, so the
    process has to be QEMU's."""
    try:
        pid = int((STATE / "qemu.pid").read_text().strip())
        cmdline = Path(f"/proc/{pid}/cmdline").read_bytes()
    except (OSError, ValueError):
        return None
    return pid if b"qemu-system" in cmdline else None


def vm_running() -> bool:
    return vm_pid() is not None


def stop_vm() -> None:
    pid_file = STATE / "qemu.pid"
    pid = vm_pid()
    if pid is None:
        pid_file.unlink(missing_ok=True)
        return
    log(f"stopping VM (pid {pid})")
    os.kill(pid, 15)
    for _ in range(10):
        time.sleep(1)
        if vm_pid() is None:
            break
    else:
        os.kill(pid, 9)
    pid_file.unlink(missing_ok=True)


def ssh(command: str, *, check: bool = True, timeout: float = 120,
        input: str | None = None) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run(["ssh", *SSH_OPTS, f"{SSH_USER}@{VM_IP}", command],
                                capture_output=True, text=True, timeout=timeout, input=input)
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


def require_vm() -> None:
    if not vm_running():
        die("the VM is not running; run `up` first")


# ---------------------------------------------------------------------------------------------
# Agent container


def agent_task_status() -> str:
    """Return the agent task's status, RUNNING once it runs, or "" if there is none."""
    out = ssh(f"sudo {CTR} task ls", check=False).stdout
    for line in out.splitlines()[1:]:
        fields = line.split()
        if fields and fields[0] == CONTAINER_ID:
            return fields[-1]
    return ""


def agent_task_pid() -> str:
    out = ssh(f"sudo {CTR} task ls", check=False).stdout
    for line in out.splitlines()[1:]:
        fields = line.split()
        if fields and fields[0] == CONTAINER_ID:
            return fields[1]
    return ""


def wait_for_agent(timeout: float = 600) -> None:
    log(f"waiting for the agent container ({AGENT_UNIT})")

    def running() -> bool:
        if agent_task_status() == "RUNNING":
            # The entrypoint logs its checks, then idles. Only this start of the unit counts: the
            # journal keeps the lines of the ones before it.
            return "agent container ready" in ssh(
                f"id=$(systemctl show {AGENT_UNIT} -p InvocationID --value) && [ -n \"$id\" ] && "
                f"journalctl -b --no-pager -o cat _SYSTEMD_INVOCATION_ID=$id", check=False).stdout
        state = ssh(f"systemctl show {AGENT_UNIT} -p ActiveState --value", check=False).stdout.strip()
        if state == "failed":
            print(ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -n 40", check=False).stdout)
        # The entrypoint exits when the host's machinectl fails, and systemd restarts it, so a
        # denial would otherwise only show as a timeout.
        failure = ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -o cat | grep -m1 'machinectl failed'",
                      check=False).stdout.strip()
        if "Access denied" in failure:
            die(f"the agent container cannot reach the host's systemd over D-Bus: {failure}\n"
                "ACL's SELinux policy needs `allow spc_t kernel_t:dbus send_msg;`, which ACL "
                "3.0.20261019 and later have (see SELINUX.md). Look for denials with "
                "`sudo journalctl -b | grep 'avc:  denied'` on the VM.")
        return False

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if running():
            log("the agent container is running")
            return
        time.sleep(5)
    print(ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -n 60", check=False).stdout)
    die(f"timed out after {int(timeout)}s waiting for the agent container")


def unit_properties(unit: str, *names: str) -> dict[str, str]:
    out = ssh(f"systemctl show {unit} " + " ".join(f"-p {name}" for name in names), check=False).stdout
    return dict(line.split("=", 1) for line in out.splitlines() if "=" in line)


def agent_installed() -> bool:
    return ssh(f"test -e /etc/systemd/system/{AGENT_UNIT}", check=False).returncode == 0


def wait_for_bootstrap(timeout: float = 1800) -> None:
    """Wait for the bootstrap unit to install the agent: it pulls the image and runs bootstrap.sh,
    whose start installs and starts the agent's unit."""
    log(f"waiting for {BOOTSTRAP_UNIT} to bootstrap the node")

    def done() -> bool:
        state = unit_properties(BOOTSTRAP_UNIT, "ActiveState", "Result")
        if state.get("ActiveState") == "failed":
            print(ssh(f"journalctl -u {BOOTSTRAP_UNIT} -b --no-pager -n 60", check=False).stdout)
            die(f"{BOOTSTRAP_UNIT} failed")
        return state.get("ActiveState") == "inactive" and state.get("Result") == "success" and agent_installed()

    if not eventually(done, timeout, 10):
        print(ssh(f"journalctl -u {BOOTSTRAP_UNIT} -b --no-pager -n 60", check=False).stdout)
        die(f"timed out after {int(timeout)}s waiting for {BOOTSTRAP_UNIT}")
    log(f"bootstrap installed {AGENT_UNIT}")


def bootstrap_again(image: str = "") -> None:
    """Join the VM again, as bootstrapping a host again does: new inputs, with a new token, and a
    start of the bootstrap unit. With an image, the inputs select it, as bootstrap.sh's inputs
    name the agent it downloads; without one, bootstrap runs the install's."""
    log(f"bootstrapping {HOSTNAME} again" + (f" with {image}" if image else ""))
    inputs = bootstrap_inputs()
    if image:
        inputs[AGENT_IMAGE_SELECTION] = f"AKS_FLEX_NODE_IMAGE={image}\n"
    for path, content in inputs.items():
        ssh(f"sudo install -D -m 0600 /dev/stdin {path}", input=content)
    ssh(f"sudo systemctl start --no-block {BOOTSTRAP_UNIT}")
    wait_for_bootstrap()
    wait_for_agent()


def agent_command(args: list[str], tty: bool) -> str:
    """Return the VM command that runs args in the agent container. ctr exec does not pass the
    caller's environment, so TERM is set for interactive shells."""
    term = os.environ.get("TERM", "xterm-256color") if tty else "dumb"
    exec_id = f"aclc-{secrets.token_hex(4)}"
    inner = ["/usr/bin/env", f"TERM={term}", *args]
    return shlex.join(["sudo", "ctr", "--namespace", CTR_NAMESPACE, "task", "exec",
                       *(["--tty"] if tty else []), "--exec-id", exec_id, CONTAINER_ID, *inner])


def agent(script: str, *, check: bool = True, timeout: float = 300) -> subprocess.CompletedProcess[str]:
    """Run a bash script in the agent container."""
    return ssh(agent_command(["/bin/bash", "-c", script], tty=False), check=check, timeout=timeout)


def agent_out(script: str) -> str:
    return agent(script).stdout.strip()


# ---------------------------------------------------------------------------------------------
# Checks


class Checks:
    def __init__(self) -> None:
        self.failures: list[str] = []
        self.started = time.monotonic()
        self.durations: list[tuple[str, float]] = []

    def expect(self, ok: bool, description: str, detail: str = "") -> None:
        if ok:
            log(f"ok: {description}")
        else:
            self.failures.append(description)
            log(f"FAIL: {description}{': ' + detail if detail else ''}")

    @contextlib.contextmanager
    def group(self, name: str):
        """Time a group of checks, for done's summary of where check spends its time."""
        log(f"checking {name}")
        started = time.monotonic()
        try:
            yield
        finally:
            self.durations.append((name, time.monotonic() - started))

    def done(self) -> None:
        if self.durations:
            log(f"took {duration(time.monotonic() - self.started)}: "
                + ", ".join(f"{name} {duration(seconds)}" for name, seconds in self.durations))
        if self.failures:
            die(f"{len(self.failures)} check(s) failed: " + "; ".join(self.failures))
        log("all checks passed")


def duration(seconds: float) -> str:
    minutes, seconds = divmod(round(seconds), 60)
    return f"{minutes}m{seconds:02d}s" if minutes else f"{seconds}s"


def machine_state(machine: str) -> str:
    """Return the machine's machined state, such as running, or "" if machined has no such
    machine."""
    return ssh(f"machinectl show {shlex.quote(machine)} -p State --value", check=False).stdout.strip()


def machine_leader(machine: str) -> str:
    return ssh(f"machinectl show {shlex.quote(machine)} -p Leader --value", check=False).stdout.strip()


def wait_machine(machine: str, want: str, timeout: float = 180) -> None:
    wait_until(f"machine {machine} to be {want or 'gone'}", timeout,
               lambda: machine_state(machine) == want, 3)


def check_container(checks: Checks) -> None:
    checks.expect(ssh(f"systemctl is-active {AGENT_UNIT}", check=False).stdout.strip() == "active",
                  f"{AGENT_UNIT} is active")
    checks.expect(agent_task_status() == "RUNNING", "the agent task runs in containerd")
    pid = ssh_out(f"sudo {CTR} task ls | awk '$1 == \"{CONTAINER_ID}\" {{print $2}}'")
    cgroup = ssh_out(f"cat /proc/{pid}/cgroup") if pid else ""
    checks.expect(cgroup == "0::/system.slice/system-aksflex.slice/aks-flex-agent.scope",
                  "the agent's container runs in aks-flex-agent.scope, where systemd tracks it", cgroup)

    # The agent's containers run as spc_t, as executing a file of containerd's snapshots, labeled
    # for containers, moves them to it. Where containerd's files are unlabeled, as ACL 3.0.20261019
    # ships them, nothing moves them, and they stay kernel_t, as every host process is on ACL.
    snapshots = ssh_out("sudo stat -c %C /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots")
    snapshot_type = (snapshots.split(":") + ["", "", ""])[2]
    expected = "kernel_t" if snapshot_type == "unlabeled_t" else "spc_t"
    context = agent_out("tr -d '\\0' </proc/self/attr/current")
    checks.expect(f":{expected}:" in context,
                  f"the agent container runs as {expected}, as containerd's snapshots are "
                  f"{snapshot_type or 'unknown'}", context)

    host_root = ssh_out("sudo stat -c %d:%i /")
    checks.expect(agent_out("stat -c %d:%i /host") == host_root,
                  "/host in the container is the host's root directory")
    checks.expect(agent_out("cat /host/etc/machine-id") == ssh_out("cat /etc/machine-id"),
                  "/host/etc/machine-id is the host's")
    host_pidns = ssh_out("sudo readlink /proc/1/ns/pid")
    checks.expect(agent_out("readlink /proc/self/ns/pid") == host_pidns,
                  "the container shares the host's PID namespace")
    container_os = agent_out(". /etc/os-release; echo $ID")
    host_os = ssh_out(". /etc/os-release; echo ${VARIANT_ID:-$ID}")
    checks.expect(container_os != host_os, "the container has its own root filesystem",
                  f"container {container_os}, host {host_os}")

    version = ssh_out("machinectl --version | head -1")
    checks.expect(agent_out("machinectl --version | head -1") == version,
                  f"machinectl in the container is the host's ({version})")
    checks.expect(agent_out("command -v machinectl; readlink -f $(command -v machinectl)")
                  .splitlines()[-1] == "/opt/aks-flex-node/bin/host-exec",
                  "machinectl in the container is the host-exec shim, not a copy")
    checks.expect(agent_out("aks-flex-node version | head -1") == "AKS Flex Node Agent",
                  "the aks-flex-node binary is in the image")

    # The agent unmounts the bpffs of each machine it removes, which host systemd mounted.
    probe = "/run/aclc-umount-check"
    ssh(f"sudo mkdir -p {probe} && sudo mount -t bpf bpf {probe}")
    unmount = agent(f"umount {probe}", check=False)
    unmounted = ssh(f"findmnt -rn {probe}", check=False).returncode != 0
    ssh(f"sudo umount {probe} 2>/dev/null; sudo rmdir {probe}", check=False)
    checks.expect(unmounted, "umount in the container unmounts on the host",
                  (unmount.stdout + unmount.stderr).strip())

    # The units give the container only the host's root. Its entrypoint mounts the host paths the
    # agent uses at their own paths.
    paths = ("/etc/aks-flex-node", "/etc/systemd", "/var/lib/machines", "/run", "/usr/local")
    container = agent_out("stat -c %d:%i " + " ".join(paths)).split()
    host = ssh_out("sudo stat -c %d:%i " + " ".join(paths)).split()
    differ = [path for path, mine, theirs in zip(paths, container, host) if mine != theirs]
    checks.expect(len(container) == len(paths) and not differ,
                  "the entrypoint mounted the host's paths at their own paths", ", ".join(differ))
    exec_start = ssh_out(f"systemctl show {AGENT_UNIT} -p ExecStart --value")
    checks.expect("/usr/bin/ctr" in exec_start and "aks-flex-node agent --config" in exec_start,
                  f"{AGENT_UNIT}, which start installed, runs the agent's image", exec_start)
    installed = ssh("ls -d /usr/local/bin/aks-flex-node /usr/local/lib/aks-flex-node 2>/dev/null",
                    check=False).stdout.strip()
    checks.expect(installed == "", "the agent has no binary of its own on the host", installed)


def daemon_state() -> dict:
    """The agent's daemon state on the host: its active machine and applied settings."""
    result = ssh("sudo cat /etc/aks-flex-node/daemon-state.json", check=False)
    try:
        return json.loads(result.stdout) if result.returncode == 0 else {}
    except json.JSONDecodeError:
        return {}


def node_uid() -> str:
    result = kubectl("get", "node", HOSTNAME, "-o", "jsonpath={.metadata.uid}", check=False,
                     quiet=True)
    return result.stdout.strip() if result.returncode == 0 else ""


def check_cluster(checks: Checks, after: str = "") -> None:
    """The node the agent in the container joined: Ready, on the machine the agent made active,
    running a pod, and reachable from the API server for its logs."""
    checks.expect(node_ready(), f"node {HOSTNAME} is Ready{after}")
    active = daemon_state().get("activeMachine", "")
    checks.expect(bool(active) and machine_state(active) == "running",
                  f"the agent's active machine, {active or 'none'}, runs on the host")
    pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "aclc-probe", "namespace": "default"},
        "spec": {
            "nodeName": HOSTNAME,
            "restartPolicy": "Never",
            "tolerations": [{"operator": "Exists"}],
            # sh, as its container's PID 1, ignores SIGTERM without a trap, so the pod would take
            # its grace period to stop, and a machine stopped meanwhile, as NodeReboot stops it,
            # would wait for it until its own stop timed out.
            "containers": [{"name": "probe", "image": "mcr.microsoft.com/azurelinux/busybox:1.36",
                            "command": ["sh", "-c", "trap 'exit 0' TERM; echo aclc-probe-ok; "
                                        "sleep 3600 & wait"]}],
        },
    }
    kubectl("delete", "pod", "aclc-probe", "--ignore-not-found", "--wait=true", quiet=True)
    kubectl("apply", "-f", "-", input=json.dumps(pod), quiet=True)
    ready = kubectl("wait", "pod/aclc-probe", "--for=condition=Ready", "--timeout=300s",
                    check=False, quiet=True)
    checks.expect(ready.returncode == 0, f"a pod runs on the node{after}", ready.stderr.strip())
    logs = kubectl("logs", "aclc-probe", check=False, quiet=True)
    checks.expect("aclc-probe-ok" in logs.stdout, f"kubectl logs reaches the node's kubelet{after}",
                  logs.stderr.strip())
    # Gone before the next check, which may stop the machine.
    kubectl("delete", "pod", "aclc-probe", "--ignore-not-found", "--wait=true", "--timeout=60s",
            check=False, quiet=True)


def check_repave(checks: Checks) -> None:
    """Repave as main's e2e does: change the machine's settings version and delete the Node. The
    daemon then starts the other machine, kube1 or kube2, with the new settings, and removes the
    old one, its bpffs mount on the host included."""
    before = daemon_state().get("activeMachine", "")
    if not before:
        checks.expect(False, "the agent has an active machine to repave from")
        return
    old_uid = node_uid()
    settings_version = f"repave-{int(time.time())}"
    log(f"repaving {HOSTNAME}, on {before}, to settings version {settings_version}")
    register_machine(kubernetes_version(), settings_version)
    kubectl("delete", "node", HOSTNAME, "--wait=false", quiet=True)

    def repaved() -> bool:
        state = daemon_state()
        active = state.get("activeMachine", "")
        return (active not in ("", before) and state.get("appliedSettingsVersion") == settings_version
                and machine_state(active) == "running" and node_uid() not in ("", old_uid)
                and node_ready())

    done = eventually(repaved, 900, 10)
    after = daemon_state().get("activeMachine", "")
    checks.expect(done, f"the repave replaced {before} with {after}, and the new Node is Ready",
                  "" if done else agent_log_tail())
    if not done:
        return

    # The daemon removes the old machine once the new one has started.
    gone = eventually(lambda: ssh(f"machinectl show-image {shlex.quote(before)} || test -e /run/bpffs/{shlex.quote(before)}",
                                  check=False).returncode != 0, 120)
    left = ssh(f"machinectl list-images --no-legend; findmnt -rn -o TARGET /run/bpffs/{shlex.quote(before)}",
               check=False).stdout.strip()
    checks.expect(gone, f"the old machine, {before}, is removed, with its bpffs mount", left)
    check_cluster(checks, after=" after the repave")


def agent_log_tail() -> str:
    return ssh("sudo tail -n 20 /var/log/aks-flex-node/aks-flex-node.log", check=False).stdout


def run_machine_operation(kind: str, timeout: float = 600,
                          parameters: dict[str, str] | None = None) -> tuple[str, str]:
    """Request a MachineOperation of the node, as main's demos and e2e do, and return its final
    phase and message."""
    result = machine_operation(kind, timeout, parameters)
    return result["phase"], result["message"]


def machine_operation(kind: str, timeout: float = 600,
                      parameters: dict[str, str] | None = None) -> dict[str, str]:
    """Request a MachineOperation of the node, and return its final phase, its message, and the
    reason of its Completed condition, which main's e2e checks for a rejected request."""
    name = f"aclc-{kind.lower()}-{int(time.time())}"
    log(f"requesting {kind} MachineOperation {name}")
    spec = {"machineRef": HOSTNAME, "operationKind": kind, "ttlSecondsAfterFinished": 3600}
    if parameters:
        spec["parameters"] = parameters
    operation = {
        "apiVersion": "unbounded-cloud.io/v1alpha3",
        "kind": "MachineOperation",
        "metadata": {"name": name},
        "spec": spec,
    }
    kubectl("apply", "-f", "-", input=json.dumps(operation), quiet=True)
    status = {"phase": "", "reason": "", "message": ""}

    def finished() -> bool:
        result = kubectl("get", "machineoperation", name, "-o",
                         "jsonpath={.status.phase}|{.status.conditions[?(@.type==\"Completed\")].reason}"
                         "|{.status.message}", check=False, quiet=True)
        fields = result.stdout.split("|", 2)
        if len(fields) == 3:
            status.update(zip(("phase", "reason", "message"), fields))
        return status["phase"] in ("Complete", "Failed")

    eventually(finished, timeout, 1)
    return status


def check_node_reboot(checks: Checks) -> None:
    """NodeReboot restarts the active machine, as main's soft-reboot demo requests it."""
    active = daemon_state().get("activeMachine", "")
    leader = machine_leader(active)
    phase, message = run_machine_operation("NodeReboot")
    checks.expect(phase == "Complete", "a NodeReboot MachineOperation completes",
                  f"{phase or 'no phase'}: {message}")
    new_leader = machine_leader(active)
    checks.expect(new_leader not in ("", leader) and machine_state(active) == "running",
                  f"NodeReboot restarted {active}", f"leader {leader}, then {new_leader or 'none'}")
    checks.expect(eventually(node_ready, 300), f"node {HOSTNAME} is Ready after the NodeReboot")


# A broken candidate's agent, as main's e2e builds one: it answers version, so the upgrade
# switches to it, but its daemon fails.
BROKEN_AGENT = """\
#!/bin/bash
if [[ ${1:-} == version ]]; then
    exec /opt/aks-flex-node/bin/aks-flex-node-real "$@"
fi
echo "aks-flex-node: a broken candidate from harness.py check" >&2
exit 1
"""


def build_candidate_images() -> tuple[str, str]:
    """Build two candidates from the agent's image, as main's e2e builds its candidates from its
    binary: a good one, whose agent binary differs but runs, and a broken one. Push them, and
    return the references the VM pulls them by, pinned by digest. A candidate already pushed from
    the same agent image, by the same recipe, is reused while the registry has it."""
    candidates = {
        "upgrade-good": (["RUN printf '\\nAKS-FLEX-UPGRADE-CHECK\\n' >> /opt/aks-flex-node/bin/aks-flex-node"], {}),
        "upgrade-broken": (["RUN mv /opt/aks-flex-node/bin/aks-flex-node /opt/aks-flex-node/bin/aks-flex-node-real",
                            "COPY --chmod=0755 aks-flex-node /opt/aks-flex-node/bin/aks-flex-node"],
                           {"aks-flex-node": BROKEN_AGENT}),
    }
    base = capture([ENGINE, "image", "inspect", "-f", "{{.Id}}", LOCAL_AGENT_IMAGE]).strip()
    references = []
    for tag, (steps, files) in candidates.items():
        recipe = json.dumps({"base": base, "steps": steps, "files": files}, sort_keys=True)
        key = hashlib.sha256(recipe.encode()).hexdigest()
        record = STATE / f"{tag}.pushed.json"
        reference = pushed_candidate(record, key)
        if reference:
            log(f"reusing {tag}, pushed from the same agent image")
            references.append(reference)
            continue
        context = STATE / tag
        shutil.rmtree(context, ignore_errors=True)
        context.mkdir()
        for name, content in files.items():
            (context / name).write_text(content)
        (context / "Containerfile").write_text("\n".join([f"FROM {LOCAL_AGENT_IMAGE}", *steps]) + "\n")
        local = f"localhost/{AGENT_REPOSITORY}:{tag}"
        log(f"building {local}")
        run([ENGINE, "build", "--quiet", "-t", local, str(context)], capture=True)
        reference = push_image(local, tag)
        record.write_text(json.dumps({"key": key, "reference": reference}) + "\n")
        references.append(reference)
    return references[0], references[1]


def pushed_candidate(record: Path, key: str) -> str:
    """The reference a candidate was pushed by, if the record says it was built as key describes
    and the registry still has it, which up and down replace; or ""."""
    try:
        pushed = json.loads(record.read_text())
    except (OSError, ValueError):
        return ""
    reference = pushed.get("reference", "")
    if pushed.get("key") != key or "@" not in reference:
        return ""
    digest = reference.rpartition("@")[2]
    request = urllib.request.Request(
        f"http://127.0.0.1:{REGISTRY_PORT}/v2/{AGENT_REPOSITORY}/manifests/{digest}", method="HEAD",
        headers={"Accept": "application/vnd.oci.image.manifest.v1+json, "
                           "application/vnd.docker.distribution.manifest.v2+json"})
    try:
        with urllib.request.urlopen(request, timeout=10):
            return reference
    except OSError:
        return ""


def agent_image() -> str:
    """The image the agent's container runs, which its unit passes in."""
    return agent_out('printf "%s" "$AKS_FLEX_NODE_IMAGE"')


def agent_binary_digest() -> str:
    return agent_out("sha256sum /opt/aks-flex-node/bin/aks-flex-node").split()[0]


def machine_agent_digest() -> str:
    machine = daemon_state().get("activeMachine", "")
    out = ssh(f"sudo sha256sum /var/lib/machines/{shlex.quote(machine)}/usr/local/bin/aks-flex-node", check=False)
    return out.stdout.split()[0] if out.returncode == 0 else ""


SYSTEMD_TIMESPAN_UNITS = {"us": 1e-6, "ms": 1e-3, "s": 1, "min": 60, "h": 3600}


def make_room_for_agent_starts(needed: int) -> None:
    """Wait, if need be, until the agent's unit can start needed more times within its start
    limit. The limit counts deliberate restarts, as AgentUpgrade's, as well as failures, and a
    start past it fails the unit, which recovery takes for a failed upgrade. Host root, kernel_t on
    ACL, may not reset it, so a group of checks that restarts the agent waits for earlier starts
    to age out instead."""
    props = dict(line.split("=", 1) for line in ssh_out(
        f"systemctl show {AGENT_UNIT} -p StartLimitBurst -p StartLimitIntervalUSec").splitlines() if "=" in line)
    burst = int(props.get("StartLimitBurst", "0") or 0)
    interval = sum(int(n) * SYSTEMD_TIMESPAN_UNITS[unit] for n, unit in
                   re.findall(r"(\d+)(us|ms|min|s|h)\b", props.get("StartLimitIntervalUSec", "")))
    if not burst or not interval:
        return
    now, *starts = ssh_out(
        f"date +%s.%N; journalctl -b -u {AGENT_UNIT} --since -{int(interval)}s -o short-unix --no-pager "
        f"| awk '/Starting {AGENT_UNIT}/ {{print $1}}'").split()
    excess = len(starts) + needed - burst
    if excess <= 0:
        return
    if needed > burst:
        die(f"{needed} starts of {AGENT_UNIT} can't fit in its start limit, {burst} starts in {int(interval)}s")
    wait = float(sorted(starts, key=float)[excess - 1]) + interval - float(now) + 1
    if wait > 0:
        log(f"waiting {int(wait)}s for {AGENT_UNIT}'s start limit, {burst} starts in {int(interval)}s, "
            f"to allow {needed} more")
        time.sleep(wait)


def agent_image_by_digest() -> str:
    """The image the harness last pushed as the agent's, pinned by digest, as an AgentUpgrade has
    to name it."""
    digest = (STATE / "agent-image.digest").read_text().strip()
    return f"{GATEWAY}:{REGISTRY_PORT}/{AGENT_REPOSITORY}@{digest}"


def check_agent_upgrade(checks: Checks) -> str:
    """AgentUpgrade by image, as main's e2e upgrades by archive: to a candidate whose agent
    binary differs, then to one whose daemon fails, which rolls back to the last good image, then
    one naming an image by tag alone, which is rejected, as main rejects an archive without its
    SHA-256, then back to the agent's own image. Return the good candidate."""
    good, broken = build_candidate_images()
    # The good candidate, the broken one and its rollback, and the agent's own image.
    make_room_for_agent_starts(4)
    original = agent_image_by_digest()
    before = agent_binary_digest()

    phase, message = run_machine_operation("AgentUpgrade", parameters={"image": good})
    checks.expect(phase == "Complete", "an AgentUpgrade to a candidate image completes",
                  f"{phase or 'no phase'}: {message}")
    upgraded = agent_binary_digest()
    checks.expect(agent_image() == good and upgraded != before,
                  "the agent runs the candidate image's agent", agent_image())
    checks.expect(machine_agent_digest() == upgraded,
                  "the active machine has the candidate's agent binary, for kubelet")
    checks.expect(eventually(node_ready, 300), f"node {HOSTNAME} is Ready after the upgrade")

    log("the next candidate's daemon fails, and its recovery unit restores the last good image")
    phase, message = run_machine_operation("AgentUpgrade", timeout=1800, parameters={"image": broken})
    checks.expect(phase == "Failed", "an AgentUpgrade to an image whose daemon fails, fails",
                  f"{phase or 'no phase'}: {message}")
    wait_for_agent()
    checks.expect(agent_image() == good and agent_binary_digest() == upgraded,
                  "the agent is back on the last good image", agent_image())
    checks.expect(machine_agent_digest() == upgraded,
                  "the active machine has the last good agent binary again")
    checks.expect(eventually(node_ready, 300), f"node {HOSTNAME} is Ready after the rollback")

    selection = ssh_out(f"sudo cat {AGENT_IMAGE_SELECTION}")
    result = machine_operation("AgentUpgrade", parameters={"image": VM_AGENT_IMAGE})
    checks.expect(result["phase"] == "Failed" and result["reason"] == "InvalidParameters",
                  "an AgentUpgrade naming an image by tag alone fails as InvalidParameters",
                  f"{result['phase'] or 'no phase'} ({result['reason'] or 'no reason'}): {result['message']}")
    untouched = (agent_image() == good and agent_binary_digest() == upgraded
                 and ssh_out(f"sudo cat {AGENT_IMAGE_SELECTION}") == selection
                 and ssh(f"sudo test ! -e {AGENT_UPGRADE_SIGNAL}", check=False).returncode == 0)
    checks.expect(untouched, "the rejected AgentUpgrade left the agent, its image selection, and "
                  "the upgrade signal as they were")

    phase, message = run_machine_operation("AgentUpgrade", parameters={"image": original})
    checks.expect(phase == "Complete" and agent_image() == original and agent_binary_digest() == before,
                  "an AgentUpgrade back to the agent's own image completes",
                  f"{phase or 'no phase'}: {message}")
    images = ssh_out(f"sudo {CTR} images ls -q").split()
    checks.expect(broken not in images and good in images,
                  "the agent removed the broken candidate's image, and kept the last good one",
                  " ".join(images))
    return good


def check_reset(checks: Checks, how: str) -> None:
    """A reset ends as on main's hosts: the agent's unit is gone and nothing of the agent runs,
    nor any machine. What installed the agent stays, so the node can be bootstrapped again."""

    def stopped() -> bool:
        return not agent_installed() and ssh(f"sudo {CTR} task ls -q", check=False).stdout.strip() == ""

    checks.expect(eventually(stopped, 300, 5),
                  f"{how} removed {AGENT_UNIT}, and no container of the agent runs",
                  ssh(f"ls /etc/systemd/system/{AGENT_UNIT}; sudo {CTR} task ls", check=False).stdout)
    wants = f"/etc/systemd/system/multi-user.target.wants/{AGENT_UNIT}"
    checks.expect(ssh(f"test ! -L {wants}", check=False).returncode == 0,
                  f"{how} left no link enabling {AGENT_UNIT}")
    left = ssh("machinectl list-images --no-legend; ls /run/bpffs; sudo ls -A /etc/aks-flex-node",
               check=False).stdout.strip()
    checks.expect(left == "", f"{how} removed the machines, their bpffs mounts, and the agent's "
                  "config, state, bootstrap inputs, and image selection", left)
    install = ssh(f"test -f {SETTINGS} && systemctl cat {BOOTSTRAP_UNIT} {RESET_UNIT} >/dev/null",
                  check=False)
    checks.expect(install.returncode == 0, f"what installed the agent survives {how}", install.stderr)


def check_rejoin(checks: Checks, how: str, image: str = "") -> None:
    # As AKS does once the agent stops.
    kubectl("delete", "node", HOSTNAME, "--ignore-not-found", quiet=True)
    bootstrap_again(image)
    checks.expect(eventually(node_ready, 1200, 10),
                  f"node {HOSTNAME} joins again after {how}, once bootstrapped",
                  "" if node_ready() else agent_log_tail())
    running = agent_image()
    what = "the image bootstrap was given" if image else "the install's image"
    checks.expect(running == (image or VM_AGENT_IMAGE), f"the agent runs {what}", running)


def check_agent_reset(checks: Checks, rejoin_image: str) -> None:
    """AgentReset, which the daemon carries out: it resets the node, completes the operation, and
    stops the agent by removing its unit. The node joins again with another image."""
    phase, message = run_machine_operation("AgentReset")
    checks.expect(phase == "Complete", "an AgentReset MachineOperation completes",
                  f"{phase or 'no phase'}: {message}")
    check_reset(checks, "AgentReset")
    check_rejoin(checks, "AgentReset", rejoin_image)


def check_local_reset(checks: Checks) -> None:
    """aks-flex-node reset, which an operator runs through the reset unit, in a container of its
    own: it stops and removes the agent's unit, then resets the node."""
    log(f"resetting {HOSTNAME} through {RESET_UNIT}")
    ssh(f"sudo systemctl start {RESET_UNIT}", check=False, timeout=900)
    succeeded = unit_properties(RESET_UNIT, "Result").get("Result") == "success"
    checks.expect(succeeded, f"aks-flex-node reset, through {RESET_UNIT}, succeeds",
                  "" if succeeded else ssh(f"journalctl -u {RESET_UNIT} -b --no-pager -n 30",
                                           check=False).stdout)
    check_reset(checks, "reset")
    check_rejoin(checks, "reset")


def check_deletion_signal(checks: Checks) -> None:
    """AKS deleting the node, as main's e2e does: its Machine gone, then its Node tainted. The
    daemon resets the node and stops the agent, as for AgentReset."""
    log(f"deleting {HOSTNAME}'s machine and tainting its Node")
    kubectl("-n", "kube-system", "patch", "configmap", "aks-flex-machines", "--type=merge",
            "-p", json.dumps({"data": {f"{HOSTNAME}.json": None}}), quiet=True)
    kubectl("taint", "node", HOSTNAME, "kubernetes.azure.com/flex-node-deleting=true:NoSchedule",
            "--overwrite", quiet=True)
    check_reset(checks, "the deletion signal")
    register_machine(kubernetes_version())
    check_rejoin(checks, "the deletion signal")


def check_machine_lifecycle(checks: Checks, reboot: bool) -> None:
    """The agent's active machine belongs to the host, not to the agent's container: it runs in
    the host's machine.slice, the container reaches it with the host's systemd-run and
    machinectl, it keeps running while the container restarts, and it comes back after a reboot.
    The agent creates and removes machines itself, which repave and the resets check."""
    machine = daemon_state().get("activeMachine", "")
    if not machine or machine_state(machine) != "running":
        checks.expect(False, "the agent's active machine runs", machine or "no active machine")
        return

    unit_cgroup = ssh_out(f"systemctl show systemd-nspawn@{shlex.quote(machine)}.service -p ControlGroup --value")
    checks.expect(unit_cgroup.startswith("/machine.slice/"),
                  f"{machine} runs under the host's machine.slice, outside the agent's container",
                  unit_cgroup)
    os_release = agent(f"systemd-run --machine={shlex.quote(machine)} --pipe --wait --quiet cat /etc/os-release",
                       check=False)
    checks.expect(os_release.returncode == 0 and "ID=" in os_release.stdout,
                  f"systemd-run --machine from the agent's container runs a command in {machine}",
                  os_release.stderr.strip())
    # The agent copies its binary into the machine, where kubelet runs it as its credential
    # plugin, which a node joined with a bootstrap token doesn't exercise.
    in_machine = ssh(f"sudo systemd-run --machine={shlex.quote(machine)} --pipe --wait --quiet "
                     "/usr/local/bin/aks-flex-node version", check=False)
    checks.expect(in_machine.returncode == 0 and "AKS Flex Node Agent" in in_machine.stdout,
                  f"the agent's binary, which kubelet runs as its credential plugin, runs in {machine}",
                  (in_machine.stdout + in_machine.stderr).strip()[-500:])
    shell = agent(f"machinectl shell {shlex.quote(machine)} /bin/sh -c 'echo shell-ok' </dev/null", check=False)
    checks.expect("shell-ok" in shell.stdout,
                  f"machinectl shell from the agent's container reaches {machine}",
                  (shell.stdout + shell.stderr).strip())

    leader = machine_leader(machine)
    make_room_for_agent_starts(1)
    log(f"restarting {AGENT_UNIT} while {machine} runs (leader pid {leader})")
    ssh(f"sudo systemctl restart {AGENT_UNIT}")
    wait_for_agent()
    checks.expect(machine_state(machine) == "running" and machine_leader(machine) == leader,
                  f"{machine} kept running, with the same leader, while the agent's container restarted")

    if reboot:
        log("rebooting the VM")
        ssh("sudo systemctl reboot", check=False, timeout=15)
        wait_until("the VM to go down", 120, lambda: ssh("true", check=False, timeout=5).returncode != 0, 3)
        wait_for_ssh()
        wait_for_agent()
        checks.expect(True, "the agent container came back after a reboot")
        wait_machine(machine, "running", timeout=300)
        checks.expect(True, f"{machine} runs again after the reboot")


# ---------------------------------------------------------------------------------------------
# Commands


def check_prerequisites(with_agent: bool) -> None:
    tools = ["qemu-system-x86_64", "qemu-img", "qemu-nbd", "ssh", "ssh-keygen", "ip", "iptables",
             "setsid", "sysctl"]
    if with_agent:
        tools.append(ENGINE)
        tools += ["kind", "kubectl", "go"]
    missing = [tool for tool in tools if shutil.which(tool) is None]
    if missing:
        die("missing tools: " + ", ".join(missing))
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        die("/dev/kvm is missing or not readable and writable; this harness needs KVM")
    ovmf_firmware()
    if run(["sudo", "-n", "true"], check=False, capture=True).returncode != 0:
        die("sudo needs a password; run `sudo -v` in this terminal first")


def cmd_up(args: argparse.Namespace) -> None:
    local = Path(args.image).expanduser() if args.image else None
    if local and not local.is_file():
        die(f"image not found: {local}")
    check_prerequisites(args.agent)
    image = local or aclimage.gallery_image(IMAGE_DIR)
    stop_vm()
    STATE.mkdir(parents=True, exist_ok=True)
    STATE.chmod(0o700)
    STATE_MARKER.touch()
    public_key = ensure_ssh_key()

    create_network()
    bootstrap = {}
    # Nothing of the agent runs until it's bootstrapped, so the agent needs a cluster to join.
    if args.agent:
        CLUSTER_MARKER.touch()
        create_cluster()
        install_controller()
        register_machine(kubernetes_version())
        kubectl("delete", "node", HOSTNAME, "--ignore-not-found", quiet=True)
        bootstrap = bootstrap_inputs()
    else:
        CLUSTER_MARKER.unlink(missing_ok=True)
    if args.agent:
        build_agent_image()
        push_agent_image()

    doc = add_harness_access(render_ignition(args.agent), public_key)
    if args.agent and args.label_containerd:
        log("labeling /var/lib/containerd before containerd starts: the agent's containers run as spc_t")
        doc = add_containerd_labels(doc)
    ignition = STATE / IGNITION_NAME
    # The bootstrap unit runs bootstrap at first boot because these are there.
    for path, content in bootstrap.items():
        doc["storage"]["files"].append({"path": path, "mode": 0o600, "overwrite": True,
                                        "contents": {"source": data_url(content)}})
    # It holds the bootstrap token, so it's private from the start.
    ignition.unlink(missing_ok=True)
    with os.fdopen(os.open(ignition, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as out:
        out.write(json.dumps(doc, indent=2) + "\n")

    # The config server only runs while the VM provisions. Later boots must not need it.
    with FileServer({IGNITION_NAME: ignition}):
        launch_vm(image, f"http://{GATEWAY}:{SERVE_PORT}/{IGNITION_NAME}")
        wait_for_ssh()
    if args.agent:
        wait_for_bootstrap()
        wait_for_agent()
        wait_for_node_ready()
    log(f"VM {HOSTNAME} is up at {VM_IP}; try `{Path(sys.argv[0]).name} status`")


def snapshotter_pull_args() -> str:
    """The snapshotter flags the bootstrap unit pulls the image with, from its environment."""
    environment = ssh_out(f"systemctl show {BOOTSTRAP_UNIT} -p Environment --value")
    for variable in shlex.split(environment):
        name, _, value = variable.partition("=")
        if name == "AKS_FLEX_NODE_SNAPSHOTTER_PULL_ARGS":
            return value
    return ""


def cmd_push(_: argparse.Namespace) -> None:
    require_vm()
    build_agent_image()
    push_agent_image()
    # The agent's unit runs the image bootstrap pulled, so pull the new one as bootstrap does. An
    # AgentUpgrade's selection of another image would override it.
    log(f"pulling {VM_AGENT_IMAGE} on the VM and restarting {AGENT_UNIT}")
    ssh(f"sudo {CTR} image pull --plain-http {snapshotter_pull_args()} {VM_AGENT_IMAGE} >/dev/null",
        timeout=600)
    ssh(f"sudo rm -f {AGENT_IMAGE_SELECTION} && sudo systemctl restart {AGENT_UNIT}")
    wait_for_agent()


# check's groups, in the order they run. Each starts from a joined node and leaves one, so any of
# them can run alone; agentreset joins again with the good candidate's image when upgrade ran.
# machines restarts the agent once and reboots, which resets its unit's start limit, so it runs
# before upgrade's four restarts rather than waiting for them to age out.
CHECK_GROUPS = ("container", "cluster", "repave", "nodereboot", "machines", "upgrade",
                "agentreset", "reset", "deletion")
# The groups that need the cluster, which up --no-agent doesn't create.
CLUSTER_CHECK_GROUPS = set(CHECK_GROUPS) - {"container"}


def check_groups(value: str) -> tuple[str, ...]:
    """Parse check --only: groups separated by commas, run in CHECK_GROUPS's order."""
    names = {name.strip() for name in value.split(",") if name.strip()}
    unknown = names - set(CHECK_GROUPS)
    if unknown or not names:
        raise argparse.ArgumentTypeError(
            f"unknown group(s) {', '.join(sorted(unknown)) or '(none given)'}; "
            f"choose from {', '.join(CHECK_GROUPS)}")
    return tuple(name for name in CHECK_GROUPS if name in names)


def cmd_check(args: argparse.Namespace) -> None:
    require_vm()
    checks = Checks()
    cluster = cluster_enabled()
    good_image = ""

    def upgrade() -> None:
        nonlocal good_image
        good_image = check_agent_upgrade(checks)

    def machines() -> None:
        check_machine_lifecycle(checks, reboot=args.reboot)
        if cluster and args.reboot:
            wait_for_node_ready()
            checks.expect(True, f"node {HOSTNAME} is Ready again after the reboot")

    groups = {
        "container": lambda: check_container(checks),
        "cluster": lambda: check_cluster(checks),
        "repave": lambda: check_repave(checks),
        "nodereboot": lambda: check_node_reboot(checks),
        "upgrade": upgrade,
        "machines": machines,
        "agentreset": lambda: check_agent_reset(checks, rejoin_image=good_image),
        "reset": lambda: check_local_reset(checks),
        "deletion": lambda: check_deletion_signal(checks),
    }
    for name in args.only:
        if name in CLUSTER_CHECK_GROUPS and not cluster:
            log(f"skipping {name}: the VM was brought up without the agent or a cluster")
            continue
        with checks.group(name):
            groups[name]()
    checks.done()


def cmd_status(_: argparse.Namespace) -> None:
    if not vm_running():
        log("the VM is not running")
        return
    log(f"VM {HOSTNAME} at {VM_IP} is running")
    print(ssh(f"systemctl status {AGENT_UNIT} --no-pager -n 0 | head -5; echo; "
              f"sudo {CTR} task ls; echo; machinectl list --all; echo; machinectl list-images",
              check=False).stdout)
    if cluster_enabled():
        kubectl("get", "nodes", "-o", "wide", check=False)
        print(ssh(f"systemctl status {BOOTSTRAP_UNIT} --no-pager -n 0 | head -3", check=False).stdout)


def cmd_ssh(args: argparse.Namespace) -> None:
    require_vm()
    os.execvp("ssh", ["ssh", *SSH_OPTS, *(["-t"] if not args.command else []),
                      f"{SSH_USER}@{VM_IP}", *args.command])


def exec_in_agent(command: list[str]) -> None:
    require_vm()
    tty = sys.stdin.isatty() and sys.stdout.isatty()
    remote = agent_command(command or ["/bin/bash", "-l"], tty)
    os.execvp("ssh", ["ssh", *SSH_OPTS, *(["-t"] if tty else []), f"{SSH_USER}@{VM_IP}", remote])


def cmd_agent(args: argparse.Namespace) -> None:
    exec_in_agent(args.command)


def cmd_console(_: argparse.Namespace) -> None:
    os.execvp("tail", ["tail", "-n", "+1", "-f", str(STATE / "serial.log")])


def cmd_down(_: argparse.Namespace) -> None:
    stop_vm()
    delete_cluster()
    delete_network()
    restore_forwarding()
    stop_registry()
    if STATE.resolve() == DEFAULT_STATE.resolve() or STATE_MARKER.exists():
        shutil.rmtree(STATE, ignore_errors=True)
        log("removed the VM, cluster, network, registry, and state")
    else:
        log(f"removed the VM, cluster, network, and registry; not {STATE}, which up didn't make")
    if IMAGE_DIR.is_dir() and any(IMAGE_DIR.glob("acl-gallery-*.qcow2")):
        log(f"kept the exported images in {IMAGE_DIR}; remove it to reclaim the space")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0],
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)

    up = commands.add_parser("up", help="create a kind cluster, build and push the agent image, "
                                        "and boot the VM, which bootstraps the agent to join it")
    up.add_argument("--image", default=os.environ.get("ACL_IMAGE", ""),
                    help="Azure Container Linux qcow2 image (or ACL_IMAGE); without one, the "
                         "gallery image is exported to ACL_IMAGE_DIR and booted")
    up.add_argument("--no-agent", dest="agent", action="store_false",
                    help="boot plain Azure Container Linux, without the agent container")
    up.add_argument("--label-containerd", action="store_true",
                    default=os.environ.get("ACLC_LABEL_CONTAINERD", "") in ("1", "true", "yes"),
                    help="label /var/lib/containerd as ACL's file_contexts has it before containerd "
                         "starts, so the agent's containers run as spc_t, as on an ACL build that "
                         "labels it (or ACLC_LABEL_CONTAINERD=1)")
    up.set_defaults(func=cmd_up)

    push = commands.add_parser("push", help="rebuild and push the agent image, and restart the container")
    push.set_defaults(func=cmd_push)

    check = commands.add_parser("check", help="run the feasibility checks")
    check.add_argument("--no-reboot", dest="reboot", action="store_false",
                       help="skip the VM reboot")
    check.add_argument("--only", type=check_groups, default=CHECK_GROUPS, metavar="GROUP[,GROUP...]",
                       help="run only these groups of checks, in their usual order: "
                            f"{', '.join(CHECK_GROUPS)}")
    check.set_defaults(func=cmd_check)

    status = commands.add_parser("status", help="show the VM, agent container, and machines")
    status.set_defaults(func=cmd_status)

    ssh_cmd = commands.add_parser("ssh", help="open a shell on the VM, or run a command there")
    ssh_cmd.add_argument("command", nargs=argparse.REMAINDER)
    ssh_cmd.set_defaults(func=cmd_ssh)

    agent_cmd = commands.add_parser("agent", help="open a shell in the agent container, or run a command there")
    agent_cmd.add_argument("command", nargs=argparse.REMAINDER)
    agent_cmd.set_defaults(func=cmd_agent)

    console = commands.add_parser("console", help="follow the VM's serial console")
    console.set_defaults(func=cmd_console)

    down = commands.add_parser("down", help="stop the VM, and remove the network, registry, and state")
    down.set_defaults(func=cmd_down)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
