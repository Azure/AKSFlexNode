#!/usr/bin/env python3
"""Prototype: run the AKS Flex Node agent as a privileged containerd container on Azure Container Linux.

    hack/acl-container/harness.py up [--image ACL.qcow2]   build and push the agent image, boot the VM
        [--selinux-workaround]                             (current ACL images need the workaround)
    hack/acl-container/harness.py push                     rebuild and push the image, restart the container
    hack/acl-container/harness.py check                    run the feasibility checks
    hack/acl-container/harness.py status                   show the VM, agent container, and machines
    hack/acl-container/harness.py ssh [COMMAND]            shell on the VM, or run a command there
    hack/acl-container/harness.py agent [COMMAND]          shell in the agent container, or run a command there
    hack/acl-container/harness.py machine ARGS             run flex-machine in the agent container
    hack/acl-container/harness.py console                  follow the VM's serial console
    hack/acl-container/harness.py down                     stop the VM, remove the network, registry, and state

The VM is provisioned only by the Ignition config that agent-container.bu renders, plus harness
access: an SSH user, a hostname, and a static address. The agent image is built from this
repository with image/Containerfile and pushed to a registry on the bridge, which the VM's
containerd pulls from. See README.md for usage and DESIGN.md for what this proves.
"""

from __future__ import annotations

import argparse
import http.server
import json
import os
import secrets
import shlex
import shutil
import subprocess
import sys
import textwrap
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import aclimage  # noqa: E402
import ukiboot  # noqa: E402

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
STATE = Path(os.environ.get("ACLC_STATE_DIR", str(REPO / ".vm" / "acl-container")))
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
# Development only, until ACL's SELinux policy lets the agent container reach the host's D-Bus;
# see selinux-workaround.bu. up records whether it applied it, so push and check match it.
SELINUX_WORKAROUND_BUTANE = HERE / "selinux-workaround.bu"
SELINUX_WORKAROUND_MARKER = STATE / "selinux-workaround"

# Must match agent-container.bu.
AGENT_UNIT = "aks-flex-node-container.service"
CTR_NAMESPACE = "aks-flex"
CONTAINER_ID = "aks-flex-node"
CTR = f"ctr --namespace {CTR_NAMESPACE}"

CHECK_MACHINE = "aclc-check"
IGNITION_NAME = "config.ign"

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
        cwd: Path | None = None) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(cmd, check=False, text=True, input=input, cwd=cwd,
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


def wait_until(description: str, timeout: float, probe, interval: float = 5) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if probe():
            return
        time.sleep(interval)
    die(f"timed out after {int(timeout)}s waiting for {description}")


# ---------------------------------------------------------------------------------------------
# Agent image and registry


def build_agent_image(squash: bool) -> None:
    commit = capture(["git", "rev-parse", "--short", "HEAD"], cwd=REPO) if (REPO / ".git").exists() else "unknown"
    dirty = run(["git", "diff", "--quiet", "HEAD"], check=False, cwd=REPO).returncode != 0
    log(f"building {LOCAL_AGENT_IMAGE} with {ENGINE}")
    # With the SELinux workaround, the VM unpacks the image with containerd's native snapshotter,
    # which keeps a full copy of the filesystem at every layer. One layer keeps that to one copy.
    squash_args = ["--squash-all"] if squash and ENGINE == "podman" else []
    run([ENGINE, "build", *squash_args, "-f", str(HERE / "image" / "Containerfile"),
         "--build-arg", f"GIT_COMMIT={commit}{'-dirty' if dirty else ''}",
         "-t", LOCAL_AGENT_IMAGE, str(REPO)])


def selinux_workaround_enabled() -> bool:
    return SELINUX_WORKAROUND_MARKER.exists()


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
    start_registry()
    digest_file = STATE / "agent-image.digest"
    if ENGINE == "podman":
        run(["podman", "push", "--quiet", "--tls-verify=false", "--digestfile", str(digest_file),
             LOCAL_AGENT_IMAGE, VM_AGENT_IMAGE])
        digest = digest_file.read_text().strip()
    else:
        loopback = f"127.0.0.1:{REGISTRY_PORT}/{AGENT_REPOSITORY}:{AGENT_TAG}"
        run(["docker", "tag", LOCAL_AGENT_IMAGE, loopback])
        run(["docker", "push", "--quiet", loopback])
        digest = capture(["docker", "inspect", "-f", "{{index .RepoDigests 0}}", loopback]).split("@")[-1]
        digest_file.write_text(digest + "\n")
    log(f"pushed {VM_AGENT_IMAGE}@{digest}")
    return digest


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
    for iface in (TAP, BRIDGE):
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
# Ignition


def butane(template: Path, files: Path) -> dict:
    """Render a Butane config, as an operator does, from a directory of the files it names."""
    rendered = capture([ENGINE, "run", "--rm", "-i", "-v", f"{files}:/files:ro,z", BUTANE_IMAGE,
                        "--strict", "--files-dir", "/files"], input=template.read_text())
    return json.loads(rendered)


def render_ignition(with_agent: bool, selinux_workaround: bool) -> dict:
    """Render agent-container.bu, from the one file it names. With the SELinux workaround,
    selinux-workaround.bu is merged in through Ignition's own config merge, which adds its drop-in
    to the agent's unit. Without the agent, the VM gets only the harness's access."""
    if not with_agent:
        return {"ignition": {"version": "3.4.0"}}
    files = STATE / "butane-files"
    shutil.rmtree(files, ignore_errors=True)
    files.mkdir(mode=0o700)
    (files / "agent-container.env").write_text(textwrap.dedent(f"""\
        AKS_FLEX_NODE_IMAGE={VM_AGENT_IMAGE}
        AKS_FLEX_NODE_PULL_ARGS=--plain-http
    """))
    doc = butane(BUTANE_TEMPLATE, files)
    if selinux_workaround:
        workaround = butane(SELINUX_WORKAROUND_BUTANE, files)
        doc["ignition"].setdefault("config", {}).setdefault("merge", []).append(
            {"source": data_url(json.dumps(workaround))})
    return doc


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
            if "agent container ready" in ssh(
                    f"id=$(systemctl show {AGENT_UNIT} -p InvocationID --value) && [ -n \"$id\" ] && "
                    f"journalctl -b --no-pager -o cat _SYSTEMD_INVOCATION_ID=$id", check=False).stdout:
                return True
        # The entrypoint exits when the host's machinectl fails, and systemd restarts it, so a
        # denial would otherwise only show as a timeout.
        failure = ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -o cat | grep -m1 'machinectl failed'",
                      check=False).stdout.strip()
        if "Access denied" in failure:
            if selinux_workaround_enabled():
                hint = ("This is with the SELinux workaround; look for denials with "
                        "`sudo journalctl -b | grep 'avc:  denied'` on the VM.")
            else:
                hint = ("ACL's SELinux policy denies it until it allows "
                        "`allow spc_t kernel_t:dbus send_msg;` (see SELINUX.md). Until then, run "
                        "`up --selinux-workaround`.")
            die(f"the agent container cannot reach the host's systemd over D-Bus: {failure}\n{hint}")
        state = ssh(f"systemctl show {AGENT_UNIT} -p ActiveState --value", check=False).stdout.strip()
        if state == "failed":
            print(ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -n 40", check=False).stdout)
        return False

    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if running():
            log("the agent container is running")
            return
        time.sleep(5)
    print(ssh(f"journalctl -u {AGENT_UNIT} -b --no-pager -n 60", check=False).stdout)
    die(f"timed out after {int(timeout)}s waiting for the agent container")


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

    def expect(self, ok: bool, description: str, detail: str = "") -> None:
        if ok:
            log(f"ok: {description}")
        else:
            self.failures.append(description)
            log(f"FAIL: {description}{': ' + detail if detail else ''}")

    def done(self) -> None:
        if self.failures:
            die(f"{len(self.failures)} check(s) failed: " + "; ".join(self.failures))
        log("all checks passed")


def machine_state(machine: str) -> str:
    """Return the machine's machined state, such as running, or "" if machined has no such
    machine."""
    return ssh(f"machinectl show {machine} -p State --value", check=False).stdout.strip()


def machine_leader(machine: str) -> str:
    return ssh(f"machinectl show {machine} -p Leader --value", check=False).stdout.strip()


def wait_machine(machine: str, want: str, timeout: float = 180) -> None:
    wait_until(f"machine {machine} to be {want or 'gone'}", timeout,
               lambda: machine_state(machine) == want, 3)


def check_container(checks: Checks) -> None:
    checks.expect(ssh(f"systemctl is-active {AGENT_UNIT}", check=False).stdout.strip() == "active",
                  f"{AGENT_UNIT} is active")
    checks.expect(agent_task_status() == "RUNNING", "the agent task runs in containerd")

    expected = "kernel_t" if selinux_workaround_enabled() else "spc_t"
    context = agent_out("tr -d '\\0' </proc/self/attr/current")
    checks.expect(f":{expected}:" in context,
                  f"the agent container runs as {expected}, "
                  f"{'with' if expected == 'kernel_t' else 'without'} the SELinux workaround", context)

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


def check_machine_lifecycle(checks: Checks, reboot: bool, keep: bool) -> None:
    machine = CHECK_MACHINE
    agent(f"flex-machine remove {machine} >/dev/null 2>&1 || true", check=False)

    log(f"creating machine {machine} from the container (pulls the rootfs image)")
    created = agent(f"flex-machine create {machine} --start", check=False, timeout=900)
    checks.expect(created.returncode == 0, f"flex-machine create {machine} --start",
                  (created.stdout + created.stderr).strip()[-2000:])
    if created.returncode != 0:
        return
    wait_machine(machine, "running")
    checks.expect(machine in ssh_out("machinectl list --no-legend"),
                  "the machine the container started is in the host's machinectl list")

    unit_cgroup = ssh_out(f"systemctl show systemd-nspawn@{machine}.service -p ControlGroup --value")
    checks.expect(unit_cgroup.startswith("/machine.slice/"),
                  "the machine runs under the host's machine.slice, outside the container",
                  unit_cgroup)

    wait_until(f"systemd in {machine} to accept commands", 120,
               lambda: agent(f"flex-machine exec {machine} true", check=False).returncode == 0, 3)
    os_release = agent(f"flex-machine exec {machine} cat /etc/os-release", check=False)
    checks.expect(os_release.returncode == 0 and "ID=" in os_release.stdout,
                  "systemd-run --machine from the container runs a command in the machine",
                  os_release.stderr.strip())
    shell = agent(f"machinectl shell {machine} /bin/sh -c 'echo shell-ok' </dev/null", check=False)
    checks.expect("shell-ok" in shell.stdout, "machinectl shell from the container reaches the machine",
                  (shell.stdout + shell.stderr).strip())

    leader = machine_leader(machine)
    log(f"restarting {AGENT_UNIT} while {machine} runs (leader pid {leader})")
    ssh(f"sudo systemctl restart {AGENT_UNIT}")
    wait_for_agent()
    checks.expect(machine_state(machine) == "running" and machine_leader(machine) == leader,
                  "the machine kept running, with the same leader, while the agent container restarted")

    agent(f"flex-machine stop {machine}")
    checks.expect(machine_state(machine) == "", "flex-machine stop powered the machine off")
    agent(f"flex-machine start {machine}")
    wait_machine(machine, "running")
    checks.expect(True, "flex-machine start started it again")

    if reboot:
        agent(f"flex-machine enable {machine}")
        log("rebooting the VM")
        ssh("sudo systemctl reboot", check=False, timeout=15)
        wait_until("the VM to go down", 120, lambda: ssh("true", check=False, timeout=5).returncode != 0, 3)
        wait_for_ssh()
        wait_for_agent()
        checks.expect(True, "the agent container came back after a reboot")
        wait_machine(machine, "running")
        checks.expect(True, "the enabled machine started at boot")
        agent(f"flex-machine disable {machine}")

    if keep:
        log(f"keeping machine {machine}")
        return
    agent(f"flex-machine remove {machine}")
    checks.expect(machine_state(machine) == "", "flex-machine remove stopped the machine")
    gone = ssh(f"sudo test ! -e /var/lib/machines/{machine} -a ! -e /etc/systemd/nspawn/{machine}.nspawn",
               check=False).returncode == 0
    checks.expect(gone, "flex-machine remove deleted the machine's directory and .nspawn file")


# ---------------------------------------------------------------------------------------------
# Commands


def check_prerequisites(with_agent: bool) -> None:
    tools = ["qemu-system-x86_64", "qemu-img", "qemu-nbd", "ssh", "ssh-keygen", "ip", "iptables",
             "setsid", "sysctl"]
    if with_agent:
        tools.append(ENGINE)
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
    public_key = ensure_ssh_key()
    workaround = args.agent and args.selinux_workaround
    if workaround:
        SELINUX_WORKAROUND_MARKER.touch()
    else:
        SELINUX_WORKAROUND_MARKER.unlink(missing_ok=True)

    create_network()
    if args.agent:
        log(f"SELinux workaround: {'on' if workaround else 'off'}")
        build_agent_image(squash=workaround)
        push_agent_image()

    doc = add_harness_access(render_ignition(args.agent, workaround), public_key)
    ignition = STATE / IGNITION_NAME
    ignition.write_text(json.dumps(doc, indent=2) + "\n")
    ignition.chmod(0o600)

    # The config server only runs while the VM provisions. Later boots must not need it.
    with FileServer({IGNITION_NAME: ignition}):
        launch_vm(image, f"http://{GATEWAY}:{SERVE_PORT}/{IGNITION_NAME}")
        wait_for_ssh()
    if args.agent:
        wait_for_agent()
    log(f"VM {HOSTNAME} is up at {VM_IP}; try `{Path(sys.argv[0]).name} status`")


def cmd_push(_: argparse.Namespace) -> None:
    require_vm()
    build_agent_image(squash=selinux_workaround_enabled())
    push_agent_image()
    log(f"restarting {AGENT_UNIT}, which pulls the new image")
    ssh(f"sudo systemctl restart {AGENT_UNIT}")
    wait_for_agent()


def cmd_check(args: argparse.Namespace) -> None:
    require_vm()
    checks = Checks()
    check_container(checks)
    check_machine_lifecycle(checks, reboot=args.reboot, keep=args.keep)
    checks.done()


def cmd_status(_: argparse.Namespace) -> None:
    if not vm_running():
        log("the VM is not running")
        return
    log(f"VM {HOSTNAME} at {VM_IP} is running; SELinux workaround "
        f"{'on' if selinux_workaround_enabled() else 'off'}")
    print(ssh(f"systemctl status {AGENT_UNIT} --no-pager -n 0 | head -5; echo; "
              f"sudo {CTR} task ls; echo; machinectl list --all; echo; machinectl list-images",
              check=False).stdout)


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


def cmd_machine(args: argparse.Namespace) -> None:
    exec_in_agent(["flex-machine", *args.args])


def cmd_console(_: argparse.Namespace) -> None:
    os.execvp("tail", ["tail", "-n", "+1", "-f", str(STATE / "serial.log")])


def cmd_down(_: argparse.Namespace) -> None:
    stop_vm()
    delete_network()
    restore_forwarding()
    stop_registry()
    shutil.rmtree(STATE, ignore_errors=True)
    log("removed the VM, network, registry, and state")
    if IMAGE_DIR.is_dir() and any(IMAGE_DIR.glob("acl-gallery-*.qcow2")):
        log(f"kept the exported images in {IMAGE_DIR}; remove it to reclaim the space")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0],
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest="command", required=True)

    up = commands.add_parser("up", help="build and push the agent image, and boot the VM")
    up.add_argument("--image", default=os.environ.get("ACL_IMAGE", ""),
                    help="Azure Container Linux qcow2 image (or ACL_IMAGE); without one, the "
                         "gallery image is exported to ACL_IMAGE_DIR and booted")
    up.add_argument("--no-agent", dest="agent", action="store_false",
                    help="boot plain Azure Container Linux, without the agent container")
    up.add_argument("--selinux-workaround", action="store_true",
                    default=os.environ.get("ACLC_SELINUX_WORKAROUND", "") in ("1", "true", "yes"),
                    help="run the agent container through selinux-workaround.bu, which ACL images "
                         "need until their policy allows the container's D-Bus messages (or "
                         "ACLC_SELINUX_WORKAROUND=1)")
    up.set_defaults(func=cmd_up)

    push = commands.add_parser("push", help="rebuild and push the agent image, and restart the container")
    push.set_defaults(func=cmd_push)

    check = commands.add_parser("check", help="run the feasibility checks")
    check.add_argument("--no-reboot", dest="reboot", action="store_false",
                       help="skip the VM reboot")
    check.add_argument("--keep", action="store_true", help=f"keep the {CHECK_MACHINE} machine")
    check.set_defaults(func=cmd_check)

    status = commands.add_parser("status", help="show the VM, agent container, and machines")
    status.set_defaults(func=cmd_status)

    ssh_cmd = commands.add_parser("ssh", help="open a shell on the VM, or run a command there")
    ssh_cmd.add_argument("command", nargs=argparse.REMAINDER)
    ssh_cmd.set_defaults(func=cmd_ssh)

    agent_cmd = commands.add_parser("agent", help="open a shell in the agent container, or run a command there")
    agent_cmd.add_argument("command", nargs=argparse.REMAINDER)
    agent_cmd.set_defaults(func=cmd_agent)

    machine = commands.add_parser("machine", help="run flex-machine in the agent container",
                                  add_help=False)
    machine.add_argument("args", nargs=argparse.REMAINDER)
    machine.set_defaults(func=cmd_machine)

    console = commands.add_parser("console", help="follow the VM's serial console")
    console.set_defaults(func=cmd_console)

    down = commands.add_parser("down", help="stop the VM, and remove the network, registry, and state")
    down.set_defaults(func=cmd_down)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
