#!/usr/bin/env python3
"""Credential-safe, mode-aware probes run on a Flex E2E host."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile


MODES = ("none", "container", "nspawn")
SLOTS = ("kube1", "kube2")
STATE = "/etc/aks-flex-node/daemon-state.json"
CONFIG = "/etc/aks-flex-node/config.json"
RUNC = "/opt/unbounded/runtime/runc"
DAEMON = "aks-flex-node-agent.service"
LABEL = "kubernetes.azure.com/flex-runtime-generation"
MARKERS = tuple("/etc/aks-flex-node/" + name for name in (
    "none-reset", "none-startup.json", "none-upgrade-journal.json",
    "none-upgrade-config", "container-startup.json"))
NONE_UNITS = ("unbounded-kubelet.service", "unbounded-containerd.service",
              "unbounded-localdns.service", "aks-flex-node-npd.service")
PROPERTIES = ("LoadState", "UnitFileState", "ActiveState", "SubState", "MainPID")


class ProbeError(Exception):
    def __init__(self, message, code=1):
        super().__init__(message)
        self.code = code


def require(condition, message):
    if not condition:
        raise ProbeError(message)


def scalar(value, field, empty=False):
    require(isinstance(value, str) and (empty or bool(value)) and
            len(value) <= 256 and re.fullmatch(r"[A-Za-z0-9_.:+/@=-]*", value),
            "invalid " + field)
    return value


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON field")
        result[key] = value
    return result


def decode(text, label):
    try:
        return json.loads(text, object_pairs_hook=unique_object)
    except (ValueError, UnicodeError):
        raise ProbeError("invalid JSON in " + label) from None


def payload_bytes(text):
    """Keep the payload's original spacing/escapes: Go hashes json.RawMessage."""
    decoder = json.JSONDecoder()
    pos = text.index("{") + 1
    while True:
        pos += len(text[pos:]) - len(text[pos:].lstrip())
        key, pos = decoder.raw_decode(text, pos)
        pos += len(text[pos:]) - len(text[pos:].lstrip())
        require(text[pos] == ":", "invalid state record")
        pos += 1
        pos += len(text[pos:]) - len(text[pos:].lstrip())
        start = pos
        _, pos = decoder.raw_decode(text, pos)
        if key == "payload":
            return text[start:pos].encode("utf-8")
        pos += len(text[pos:]) - len(text[pos:].lstrip())
        require(text[pos] == ",", "missing state payload")
        pos += 1


class Runtime:
    def __init__(self, root="/", runner=subprocess.run):
        # An explicit root keeps filesystem unit tests off the actual host.
        self.root = Path(root)
        self.runner = runner

    def path(self, path):
        return self.root / path.lstrip("/")

    def exists(self, path):
        try:
            self.path(path).lstat()
            return True
        except FileNotFoundError:
            return False

    def read(self, path):
        try:
            data = self.path(path).read_bytes()
            require(len(data) <= 16 * 1024 * 1024, "file too large: " + path)
            return data
        except OSError as exc:
            raise ProbeError("read %s: errno %s" % (path, exc.errno)) from None

    def command(self, argv, capture=True, checked=True, timeout=30):
        try:
            result = self.runner(argv, text=True, capture_output=capture,
                                 timeout=timeout, check=False)
        except subprocess.TimeoutExpired:
            raise ProbeError("%s timed out after %ss" % (Path(argv[0]).name, timeout), 124) from None
        except OSError as exc:
            raise ProbeError("execute %s: errno %s" % (Path(argv[0]).name, exc.errno)) from None
        if checked and result.returncode:
            # stderr and argv can contain auth material, even for failed probes.
            raise ProbeError("%s exited %s" % (Path(argv[0]).name, result.returncode),
                             result.returncode if result.returncode > 0 else 128 - result.returncode)
        return result

    def state(self):
        data = self.read(STATE)
        try:
            text = data.decode("utf-8")
        except UnicodeError:
            raise ProbeError("invalid state encoding") from None
        state = decode(text, "state")
        require(isinstance(state, dict), "state is not an object")
        atomic = bool({"version", "payload", "checksum"} & state.keys())
        if atomic:
            require(type(state.get("version")) is int and state["version"] == 1,
                    "unsupported state record version")
            require(isinstance(state.get("payload"), dict), "missing state payload")
            require(state.get("checksum") == hashlib.sha256(payload_bytes(text)).hexdigest(),
                    "state checksum mismatch")
            state = state["payload"]
            require(state.get("isolation") in ("none", "container"),
                    "atomic state requires none or container isolation")
        else:
            digest = self.read(STATE + ".sha256").strip()
            require(digest == hashlib.sha256(data).hexdigest().encode("ascii"),
                    "state checksum mismatch")
        mode = state.get("isolation", "nspawn")
        require(mode in MODES, "invalid state isolation")
        require(atomic or mode == "nspawn", "non-nspawn state requires atomic record")
        slot = state.get("activeMachine", "")
        require(slot == "" if mode == "none" else slot in SLOTS, "invalid state slot")
        node = state.get("nodeConfig")
        require(isinstance(node, dict) and isinstance(node.get("Kubelet"), dict),
                "missing node state")
        require(node.get("Isolation", "nspawn") == mode, "state/node isolation mismatch")
        require(node.get("MachineName") == ("kube1" if mode == "none" else slot),
                "state/node slot mismatch")
        localdns = node.get("LocalDNS")
        require(localdns is None or (isinstance(localdns, dict) and
                type(localdns.get("Enabled", False)) is bool), "invalid node LocalDNS state")
        scalar(state.get("appliedSettingsVersion"), "generation")
        scalar(state.get("appliedKubernetesVersion"), "kubelet version")
        labels = node["Kubelet"].get("Labels", {})
        require(isinstance(labels, dict), "invalid node labels")
        scalar(labels.get(LABEL, ""), "registration", empty=mode == "nspawn")
        return state

    @staticmethod
    def mode(state):
        return state.get("isolation", "nspawn")

    def route(self, state, argv):
        require(bool(argv) and bool(argv[0]), "exec requires a command")
        mode, slot = self.mode(state), state.get("activeMachine", "")
        if mode == "none":
            self.identity(state)
            return list(argv)
        if mode == "container":
            return [RUNC, "exec", "unbounded-" + slot] + list(argv)
        return ["systemd-run", "--quiet", "--pipe", "--wait", "--machine=" + slot] + list(argv)

    def identity(self, state):
        if self.mode(state) != "none":
            return state["activeMachine"]
        link = self.path("/opt/unbounded/node/current")
        try:
            target = Path(os.readlink(link))
            require(not target.is_absolute() and len(target.parts) == 1 and
                    target.name not in (".", "..", "current"), "invalid current version link")
            scalar(target.name, "current version")
            require((link.parent / target).is_dir(), "current version directory missing")
            return target.name
        except OSError as exc:
            raise ProbeError("read current version: errno %s" % exc.errno) from None

    def localdns_unit(self, state):
        return "unbounded-localdns.service" if self.mode(state) == "none" else "localdns.service"

    def units(self, state):
        units = NONE_UNITS if self.mode(state) == "none" else (
            "kubelet.service", "containerd.service", "localdns.service", "node-problem-detector.service")
        if not (state["nodeConfig"].get("LocalDNS") or {}).get("Enabled", False):
            units = tuple(unit for unit in units if unit != self.localdns_unit(state))
        return units

    def properties(self, unit, state=None):
        argv = ["systemctl", "show", "--no-pager", "--property=" + ",".join(PROPERTIES), unit]
        if state is not None:
            argv = self.route(state, argv)
        result = self.command(argv)
        values = {}
        for line in result.stdout.splitlines():
            key, sep, value = line.partition("=")
            if sep and key in PROPERTIES:
                values[key] = scalar(value, "unit " + key, empty=True)
        return values

    @staticmethod
    def running(unit, values, enabled=False):
        pid = values.get("MainPID", "")
        require(values.get("LoadState") == "loaded" and values.get("ActiveState") == "active" and
                values.get("SubState") == "running" and pid.isascii() and pid.isdecimal() and int(pid) > 0,
                "%s not running: load=%s active=%s sub=%s pid=%s" % (
                    unit, values.get("LoadState", "missing"), values.get("ActiveState", "missing"),
                    values.get("SubState", "missing"), pid or "missing"))
        if enabled:
            require(values.get("UnitFileState") == "enabled", unit + " is not enabled")
        return int(pid)

    def snapshot(self):
        state = self.state()
        pids = {unit: self.running(unit, self.properties(unit, state)) for unit in self.units(state)}
        return {"mode": self.mode(state), "slot": state.get("activeMachine", ""),
                "generation": state["appliedSettingsVersion"], "identity": self.identity(state),
                "kubeletVersion": state["appliedKubernetesVersion"],
                "registration": state["nodeConfig"]["Kubelet"].get("Labels", {}).get(LABEL, ""),
                "corePids": pids}

    def nspawn_workers(self):
        result = self.command(["systemctl", "list-units", "--all", "--type=service", "--plain",
                               "--no-legend", "--no-pager", "systemd-nspawn@*.service"])
        workers = set()
        for line in result.stdout.splitlines():
            fields = line.split()
            if fields and fields[0].startswith("systemd-nspawn@"):
                require(len(fields) >= 4, "invalid nspawn unit listing")
                if fields[2] not in ("inactive", "failed"):
                    workers.add(fields[0].removeprefix("systemd-nspawn@").removesuffix(".service"))
        machines = self.path("/run/systemd/machines")
        if machines.exists():
            workers.update(slot for slot in SLOTS if (machines / slot).exists())
        return workers

    def oci_workers(self):
        if not self.exists(RUNC):
            directory = self.path("/run/runc")
            require(not directory.exists() or not any(p.name.startswith("unbounded-")
                                                       for p in directory.iterdir()),
                    "OCI worker state remains without runtime")
            return []
        records = decode(self.command([RUNC, "list", "--format", "json"]).stdout, "runc list")
        require(isinstance(records, list), "invalid runc listing")
        require(all(isinstance(row, dict) and isinstance(row.get("id"), str) for row in records),
                "invalid runc worker record")
        return [row for row in records if row["id"].startswith("unbounded-")]

    def topology(self, mode, slot="", reset=False):
        machines, containers = self.nspawn_workers(), self.oci_workers()
        if reset or mode == "none":
            require(not machines and not containers, "unexpected nspawn/OCI workers")
        elif mode == "container":
            require(not machines, "unexpected nspawn worker")
            require(len(containers) == 1 and containers[0]["id"] == "unbounded-" + slot and
                    containers[0].get("status") == "running" and
                    type(containers[0].get("pid")) is int and containers[0]["pid"] > 0,
                    "expected exactly one matching running OCI worker with positive PID")
            other = "kube2" if slot == "kube1" else "kube1"
            self.absent("/var/lib/unbounded/containers/" + other)
            self.absent("/var/lib/unbounded/containers/bundles/" + other)
            self.absent("/etc/systemd/system/unbounded-node-" + other + ".service")
            self.absent("/etc/systemd/system/unbounded-node-" + other + ".service.d")
            self.running("unbounded-node-" + slot + ".service",
                         self.properties("unbounded-node-" + slot + ".service"))
        else:
            require(machines == {slot} and not containers, "expected only recorded nspawn worker")
            result = self.command(["machinectl", "show", slot, "--property=State",
                                   "--property=Service", "--property=Leader"])
            values = dict(line.split("=", 1) for line in result.stdout.splitlines() if "=" in line)
            require(values.get("State") == "running" and values.get("Service") == "systemd-nspawn" and
                    values.get("Leader", "").isascii() and values.get("Leader", "").isdecimal() and
                    int(values["Leader"]) > 0, "recorded nspawn machine is not running")
        if mode in ("none", "container") or reset:
            for name in SLOTS:
                self.absent("/var/lib/machines/" + name)
        if mode == "none" or reset:
            for name in SLOTS:
                self.absent("/var/lib/unbounded/containers/" + name)

    def absent(self, path):
        require(not self.exists(path), "unexpected residual path: " + path)

    def markers_clear(self):
        for path in MARKERS:
            self.absent(path)

    def validate(self, expected):
        state = self.state()
        require(self.mode(state) == expected, "state isolation differs from expected mode")
        cfg = decode(self.read(CONFIG), "config")
        require(isinstance(cfg, dict) and isinstance(cfg.get("bootstrap"), dict), "invalid bootstrap config")
        require(cfg["bootstrap"].get("isolation", "nspawn") == expected, "state/config isolation mismatch")
        self.running(DAEMON, self.properties(DAEMON), enabled=True)
        self.markers_clear()
        self.topology(expected, state.get("activeMachine", ""))
        localdns = self.localdns_unit(state)
        if localdns not in self.units(state):
            values = self.properties(localdns, state)
            require(values.get("ActiveState") == "inactive" and values.get("MainPID", "0") == "0" and
                    values.get("UnitFileState", "") not in ("enabled", "enabled-runtime"),
                    localdns + " remains active or enabled although LocalDNS is disabled")
        return self.snapshot()

    def validate_reset(self, expected, allow_missing_agent=False):
        self.markers_clear()
        self.absent(STATE)
        self.absent(STATE + ".sha256")
        self.absent(CONFIG)
        self.topology(expected, reset=True)
        daemon = self.properties(DAEMON)
        require(daemon.get("ActiveState") in ("inactive", "failed") and
                daemon.get("UnitFileState", "") not in ("enabled", "enabled-runtime") and
                daemon.get("MainPID", "0") == "0", "daemon remains enabled or running after reset")
        units = list(NONE_UNITS) + ["unbounded-kubelet-launch-check.service"] if expected == "none" else []
        units += ["unbounded-node-" + slot + ".service" for slot in SLOTS]
        units += ["systemd-nspawn@" + slot + ".service" for slot in SLOTS]
        for unit in units:
            self.absent("/etc/systemd/system/" + unit)
            self.absent("/etc/systemd/system/" + unit + ".d")
            values = self.properties(unit)
            require(values.get("ActiveState") in ("inactive", "failed") and
                    values.get("MainPID", "0") == "0" and
                    values.get("UnitFileState", "") not in ("enabled", "enabled-runtime"),
                    unit + " remains enabled or running after reset")
        for slot in SLOTS:
            self.absent("/etc/systemd/nspawn/" + slot + ".nspawn")
            self.absent("/etc/systemd/system/unbounded-agent-regenerate-config@" + slot + ".service")
        for path in ("/opt/unbounded/node", "/var/lib/unbounded/containers", "/var/lib/kubelet"):
            self.absent(path)
        if expected == "none":
            for path in ("/etc/cni/net.d", "/var/lib/cni", "/var/lib/containerd",
                         "/etc/unbounded/containerd/config.toml", "/etc/unbounded/localdns"):
                self.absent(path)
        # Host CNI configuration, IPAM and containerd data do not belong to OCI slots.
        binaries = ("/opt/unbounded/agent/bin/aks-flex-node", "/usr/local/bin/aks-flex-node")
        if not allow_missing_agent:
            require(any(self.path(p).is_file() and os.access(self.path(p), os.X_OK) for p in binaries),
                    "installed agent binary was not kept")
        return {"mode": expected, "reset": True}

    def boot_guard(self, expected):
        require(expected == "none", "boot-guard requires none mode")
        require(self.mode(self.state()) == "none", "state isolation differs from none")
        helper = str(self.path("/opt/unbounded/node/.check-startup"))
        require(os.access(helper, os.X_OK), "installed boot guard is missing or not executable")
        with tempfile.TemporaryDirectory(prefix="flex-e2e-guard-") as directory:
            paths = [Path(directory) / name for name in ("reset", "journal", "startup")]
            for reset, journal, startup in ((False, False, False), (False, True, False),
                                            (False, True, True), (True, False, False),
                                            (True, True, True), (False, False, True)):
                for path, present in zip(paths, (reset, journal, startup)):
                    if present:
                        path.touch()
                    elif path.exists():
                        path.unlink()
                result = self.command([helper] + [str(p) for p in paths], checked=False)
                wanted = 1 if reset or (journal and not startup) else 0
                require(result.returncode == wanted, "installed boot guard violated preparation boundary")
        return {"mode": "none", "bootGuard": True}

    def diagnostics(self, expected=None):
        summary = {"paths": {p: self.exists(p) for p in (STATE,) + MARKERS + (
            "/opt/unbounded/node/current", "/var/lib/unbounded/containers/kube1",
            "/var/lib/unbounded/containers/kube2", "/var/lib/machines/kube1", "/var/lib/machines/kube2")},
                   "units": {}, "errors": []}
        try:
            state = self.state()
            summary["mode"] = self.mode(state)
        except ProbeError as exc:
            summary["errors"].append(str(exc))
            summary["mode"] = expected or "unknown"
            state = None
        for unit in (DAEMON,) + (NONE_UNITS if summary["mode"] == "none" else
                               tuple("unbounded-node-" + slot + ".service" for slot in SLOTS) +
                               tuple("systemd-nspawn@" + slot + ".service" for slot in SLOTS)):
            try:
                summary["units"][unit] = self.properties(unit)
            except ProbeError as exc:
                summary["errors"].append(str(exc))
        if state is not None:
            for unit in self.units(state):
                try:
                    summary["units"]["node:" + unit] = self.properties(unit, state)
                except ProbeError as exc:
                    summary["errors"].append(str(exc))
        # Count recent journal entries by priority; never expose MESSAGE or command lines.
        try:
            argv = ["journalctl", "--no-pager", "-n", "100", "-o", "json",
                    "--output-fields=PRIORITY", "-u", DAEMON]
            rows = self.command(argv).stdout.splitlines()
            priorities = {}
            for row in rows:
                priority = decode(row, "journal metadata").get("PRIORITY", "unknown")
                require(priority in tuple(str(i) for i in range(8)) + ("unknown",), "invalid journal priority")
                priorities[priority] = priorities.get(priority, 0) + 1
            summary["journal"] = {"entries": len(rows), "priorities": priorities}
        except ProbeError as exc:
            summary["errors"].append(str(exc))
        return summary


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_subparsers(dest="action", required=True)
    actions.add_parser("snapshot")
    execute = actions.add_parser("exec")
    execute.add_argument("command", nargs=argparse.REMAINDER)
    for name in ("validate", "validate-reset", "boot-guard", "diagnostics"):
        action = actions.add_parser(name)
        action.add_argument("mode", choices=MODES, **({"nargs": "?"} if name == "diagnostics" else {}))
        if name == "validate-reset":
            action.add_argument("--allow-missing-agent", action="store_true",
                                help="initial fresh-host reset only: skip the kept-agent assertion")
    args = parser.parse_args(argv)
    runtime = Runtime()
    try:
        if args.action == "exec":
            command = args.command[1:] if args.command[:1] == ["--"] else args.command
            result = runtime.command(runtime.route(runtime.state(), command), capture=False, timeout=120)
            return result.returncode
        if args.action == "snapshot":
            result = runtime.snapshot()
        elif args.action == "diagnostics":
            result = runtime.diagnostics(args.mode)
        elif args.action == "validate":
            result = runtime.validate(args.mode)
        elif args.action == "validate-reset":
            result = runtime.validate_reset(args.mode, args.allow_missing_agent)
        else:
            result = runtime.boot_guard(args.mode)
        print(json.dumps(result, sort_keys=True))
        return 0
    except ProbeError as exc:
        print("runtime: " + str(exc), file=sys.stderr)
        return exc.code
    except OSError as exc:
        print("runtime: filesystem operation failed: errno %s" % exc.errno, file=sys.stderr)
        return 1


if __name__ == "__main__":
    # Bound filesystem probes too, including a stalled mount or a stuck remote exec.
    signal.signal(signal.SIGALRM, lambda *_: sys.exit("runtime: action deadline exceeded"))
    signal.alarm(180)
    sys.exit(main())
