#!/usr/bin/env python3
"""Local-only tests for the remote runtime probe and its Bash transport."""

import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest
from unittest import mock


HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("flex_runtime", HERE / "runtime.py")
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)


def applied(mode):
    node = {"Isolation": mode, "MachineName": "kube1", "Kubelet": {
        "Labels": {runtime.LABEL: "registration-123"},
        "Auth": {"BootstrapToken": "DO-NOT-PRINT-TOKEN"}},
        "Cluster": {"Version": "1.35.0", "CaCertBase64": "DO-NOT-PRINT-CA"}}
    result = {"isolation": mode, "activeMachine": "" if mode == "none" else "kube1",
              "appliedSettingsVersion": "generation-1", "appliedKubernetesVersion": "1.35.0",
              "nodeConfig": node}
    if mode == "nspawn":
        del node["Kubelet"]["Labels"][runtime.LABEL]
    return result


class Fixture:
    def __init__(self, directory, mode="none", reset=False, localdns_enabled=False):
        self.root = Path(directory)
        self.mode = mode
        self.reset = reset
        self.localdns_enabled = localdns_enabled
        self.calls = []
        self.overrides = {}
        self.runc_rows = [] if reset or mode != "container" else [
            {"id": "unbounded-kube1", "status": "running", "pid": 101}]
        self.machine_rows = "" if reset or mode != "nspawn" else (
            "systemd-nspawn@kube1.service loaded active running Container kube1\n")
        self.probe = runtime.Runtime(self.root, self.run)
        self.put(runtime.RUNC, "runtime")
        if reset:
            self.put("/opt/unbounded/agent/bin/aks-flex-node", "agent", executable=True)
        else:
            state = applied(mode)
            if localdns_enabled:
                state["nodeConfig"]["LocalDNS"] = {"Enabled": True}
            self.save(state)
            self.put(runtime.CONFIG, json.dumps({"bootstrap": {"isolation": mode},
                                                  "token": "DO-NOT-PRINT-CONFIG"}))
            if mode == "none":
                self.path("/opt/unbounded/node/version-1").mkdir(parents=True)
                self.path("/opt/unbounded/node/current").symlink_to("version-1")

    def path(self, path):
        return self.root / path.lstrip("/")

    def put(self, path, text="residue", executable=False):
        target = self.path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)
        if executable:
            target.chmod(0o755)
        return target

    def save(self, state, payload=None):
        mode = state.get("isolation", "nspawn")
        if mode == "nspawn":
            text = json.dumps(state, indent=2) + "\n"
            self.put(runtime.STATE, text)
            self.put(runtime.STATE + ".sha256", hashlib.sha256(text.encode()).hexdigest() + "\n")
        else:
            payload = payload if payload is not None else json.dumps(state, separators=(",", ":"))
            self.put(runtime.STATE, '{"version":1,"checksum":"%s","payload":%s}\n' % (
                hashlib.sha256(payload.encode()).hexdigest(), payload))

    def run(self, argv, **kwargs):
        self.calls.append((argv, kwargs))
        if kwargs.get("timeout", 0) <= 0 or kwargs.get("check") is not False or "shell" in kwargs:
            raise AssertionError("runner must use a finite timeout and no shell")
        args = list(argv)
        if args[:2] == [runtime.RUNC, "exec"]:
            args = args[3:]
        elif args[0] == "systemd-run":
            args = args[5:]
        if args[:2] == ["systemctl", "show"]:
            unit = args[-1]
            values = {"LoadState": "loaded", "UnitFileState": "enabled", "ActiveState": "active",
                      "SubState": "running", "MainPID": "321"}
            if self.reset or (unit in ("unbounded-localdns.service", "localdns.service") and
                              not self.localdns_enabled):
                values.update(LoadState="not-found", UnitFileState="", ActiveState="inactive",
                              SubState="dead", MainPID="0")
            values.update(self.overrides.get(unit, {}))
            out = "".join("%s=%s\n" % item for item in values.items())
        elif args[:2] == ["systemctl", "list-units"]:
            out = self.machine_rows
        elif args[:2] == [runtime.RUNC, "list"]:
            out = json.dumps(self.runc_rows)
        elif args[:2] == ["machinectl", "show"]:
            out = "State=running\nService=systemd-nspawn\nLeader=101\n"
        elif args[0] == "journalctl":
            out = '{"PRIORITY":"3","MESSAGE":"DO-NOT-PRINT-JOURNAL"}\n'
        else:
            out = "command result"
        return subprocess.CompletedProcess(argv, 0, stdout=out, stderr="DO-NOT-PRINT-STDERR")


class StateTests(unittest.TestCase):
    def test_state_formats_and_legacy_default(self):
        for mode in runtime.MODES:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, mode)
                self.assertEqual(fixture.probe.state(), applied(mode))
                if mode == "nspawn":
                    state = applied(mode)
                    del state["isolation"]
                    del state["nodeConfig"]["Isolation"]
                    fixture.save(state)
                    self.assertEqual(fixture.probe.mode(fixture.probe.state()), "nspawn")

    def test_atomic_checksum_preserves_raw_payload(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory)
            state = applied("none")
            state["nodeConfig"]["Kubelet"]["Auth"]["BootstrapToken"] = "escaped \\ \" ü"
            payload = json.dumps(state, indent=3, ensure_ascii=True)
            fixture.save(state, payload)
            self.assertEqual(fixture.probe.state(), state)
            fixture.put(runtime.STATE, fixture.path(runtime.STATE).read_text().replace(
                '"appliedSettingsVersion": "generation-1"', '"appliedSettingsVersion":"generation-1"'))
            with self.assertRaisesRegex(runtime.ProbeError, "checksum mismatch"):
                fixture.probe.state()

    def test_corrupt_records_never_use_legacy_sidecar(self):
        cases = {
            "bad checksum": lambda record: record.update(checksum="0" * 64),
            "bad version": lambda record: record.update(version=2),
            "bool version": lambda record: record.update(version=True),
            "missing payload": lambda record: record.pop("payload"),
            "missing version": lambda record: record.pop("version"),
            "missing checksum": lambda record: record.pop("checksum"),
            "null payload": lambda record: record.update(payload=None),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory)
                record = json.loads(fixture.path(runtime.STATE).read_text())
                mutate(record)
                text = json.dumps(record)
                fixture.put(runtime.STATE, text)
                fixture.put(runtime.STATE + ".sha256", hashlib.sha256(text.encode()).hexdigest())
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.state()

    def test_invalid_state_and_node_contract(self):
        changes = {
            "missing node": lambda state: state.pop("nodeConfig"),
            "node null": lambda state: state.update(nodeConfig=None),
            "wrong mode": lambda state: state.update(isolation="broken"),
            "wrong slot": lambda state: state.update(activeMachine="kube2"),
            "node mode mismatch": lambda state: state["nodeConfig"].update(Isolation="nspawn"),
            "node slot mismatch": lambda state: state["nodeConfig"].update(MachineName="kube2"),
            "generation missing": lambda state: state.pop("appliedSettingsVersion"),
            "labels missing": lambda state: state["nodeConfig"]["Kubelet"].update(Labels={}),
            "scalar injection": lambda state: state.update(appliedSettingsVersion="secret\npassword"),
        }
        for name, mutate in changes.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory)
                state = applied("none")
                mutate(state)
                # Force an atomic record even when testing an invalid mode.
                payload = json.dumps(state)
                fixture.put(runtime.STATE, '{"version":1,"payload":%s,"checksum":"%s"}' % (
                    payload, hashlib.sha256(payload.encode()).hexdigest()))
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.state()

    def test_missing_and_malformed_state_fail_closed(self):
        for text in (None, "not-json DO-NOT-PRINT", "null", "[]", '{"payload": {}, "payload": {}}'):
            with self.subTest(text=text), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory)
                if text is None:
                    fixture.path(runtime.STATE).unlink()
                else:
                    fixture.put(runtime.STATE, text)
                with self.assertRaises(runtime.ProbeError) as error:
                    fixture.probe.state()
                self.assertNotIn("DO-NOT-PRINT", str(error.exception))
                self.assertEqual(fixture.calls, [])

    def test_legacy_requires_valid_checksum(self):
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, "nspawn")
                if corrupt:
                    fixture.put(runtime.STATE + ".sha256", "bad")
                else:
                    fixture.path(runtime.STATE + ".sha256").unlink()
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.state()


class RoutingTests(unittest.TestCase):
    def test_all_modes_preserve_argv(self):
        argv = ["printf", "%s", "spaces and 'quotes'", "$(touch /tmp/pwned)", "", "--flag"]
        prefixes = {"none": [], "container": [runtime.RUNC, "exec", "unbounded-kube1"],
                    "nspawn": ["systemd-run", "--quiet", "--pipe", "--wait", "--machine=kube1"]}
        for mode in runtime.MODES:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, mode)
                self.assertEqual(fixture.probe.route(fixture.probe.state(), argv), prefixes[mode] + argv)
                with self.assertRaisesRegex(runtime.ProbeError, "requires a command"):
                    fixture.probe.route(fixture.probe.state(), [])

    def test_none_requires_real_current_version(self):
        for target in (None, "/etc", "../escape", "missing"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory)
                link = fixture.path("/opt/unbounded/node/current")
                link.unlink()
                if target is not None:
                    link.symlink_to(target)
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.route(fixture.probe.state(), ["true"])

    def test_command_failures_keep_exit_code_without_leaking(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = runtime.Runtime(directory, lambda argv, **kw: subprocess.CompletedProcess(
                argv, 37, stdout="SECRET-STDOUT", stderr="SECRET-STDERR"))
            with self.assertRaises(runtime.ProbeError) as error:
                probe.command(["false", "SECRET-ARG"])
            self.assertEqual(error.exception.code, 37)
            self.assertEqual(str(error.exception), "false exited 37")

    def test_timeouts_and_missing_executable(self):
        for exception, expected in ((subprocess.TimeoutExpired(["x"], 30), "timed out"),
                                    (FileNotFoundError(2, "SECRET"), "errno 2")):
            with self.subTest(exception=type(exception).__name__):
                probe = runtime.Runtime(runner=mock.Mock(side_effect=exception))
                with self.assertRaisesRegex(runtime.ProbeError, expected):
                    probe.command(["probe", "SECRET"])

    def test_cli_exec_exit_status_and_missing_state(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory)
            fixture.probe.runner = lambda argv, **kw: subprocess.CompletedProcess(argv, 17)
            with mock.patch.object(runtime, "Runtime", return_value=fixture.probe), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(runtime.main(["exec", "--", "false"]), 17)
                fixture.path(runtime.STATE).unlink()
                self.assertNotEqual(runtime.main(["exec", "--", "true"]), 0)


class ValidationTests(unittest.TestCase):
    def test_validate_all_modes_and_sanitized_snapshots(self):
        for mode in runtime.MODES:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, mode)
                result = fixture.probe.validate(mode)
                self.assertEqual(set(result), {"mode", "slot", "generation", "identity", "kubeletVersion",
                                               "registration", "corePids"})
                self.assertEqual(result["identity"], "version-1" if mode == "none" else "kube1")
                self.assertEqual(result["registration"], "" if mode == "nspawn" else "registration-123")
                self.assertEqual(len(result["corePids"]), 3)
                self.assertNotIn("DO-NOT-PRINT", json.dumps(result))
                self.assertTrue(all(call[1]["timeout"] > 0 for call in fixture.calls))

    def test_localdns_enabled_and_disabled_modes(self):
        for mode in runtime.MODES:
            for enabled in (False, True):
                with self.subTest(mode=mode, enabled=enabled), tempfile.TemporaryDirectory() as directory:
                    fixture = Fixture(directory, mode, localdns_enabled=enabled)
                    unit = fixture.probe.localdns_unit(fixture.probe.state())
                    result = fixture.probe.validate(mode)
                    self.assertEqual(len(result["corePids"]), 4 if enabled else 3)
                    self.assertEqual(unit in result["corePids"], enabled)
                    for values in ({"ActiveState": "failed", "SubState": "failed", "MainPID": "0"},
                                   {"ActiveState": "activating", "SubState": "auto-restart", "MainPID": "0"}):
                        fixture.overrides[unit] = values
                        with self.assertRaises(runtime.ProbeError):
                            fixture.probe.validate(mode)
                    if not enabled:
                        fixture.overrides[unit] = {"LoadState": "loaded", "ActiveState": "active",
                                                   "SubState": "running", "MainPID": "99"}
                        with self.assertRaises(runtime.ProbeError):
                            fixture.probe.validate(mode)
                        state = applied(mode)
                        state["nodeConfig"]["LocalDNS"] = {"Enabled": False}
                        fixture.save(state)
                        fixture.overrides.clear()
                        self.assertEqual(len(fixture.probe.snapshot()["corePids"]), 3)

    def test_core_services_must_be_running_with_positive_pid(self):
        cases = ({"MainPID": "0"}, {"MainPID": "-1"}, {"MainPID": "abc"}, {"MainPID": ""},
                 {"MainPID": "١"}, {"SubState": "auto-restart"}, {"SubState": "exited"},
                 {"ActiveState": "activating"}, {"LoadState": "not-found"})
        for mode in runtime.MODES:
            for values in cases:
                with self.subTest(mode=mode, values=values), tempfile.TemporaryDirectory() as directory:
                    fixture = Fixture(directory, mode)
                    for unit in fixture.probe.units(fixture.probe.state()):
                        fixture.overrides = {unit: values}
                        with self.assertRaises(runtime.ProbeError):
                            fixture.probe.validate(mode)

    def test_daemon_must_be_installed_enabled_and_active(self):
        for values in ({"LoadState": "not-found"}, {"UnitFileState": "disabled"},
                       {"ActiveState": "inactive"}, {"MainPID": "0"}):
            with self.subTest(values=values), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory)
                fixture.overrides[runtime.DAEMON] = values
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.validate("none")

    def test_mode_equality_and_pending_markers(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory)
            with self.assertRaisesRegex(runtime.ProbeError, "expected mode"):
                fixture.probe.validate("container")
            fixture.put(runtime.CONFIG, '{"bootstrap":{"isolation":"container"}}')
            with self.assertRaisesRegex(runtime.ProbeError, "state/config"):
                fixture.probe.validate("none")
            fixture.put(runtime.CONFIG, '{"bootstrap":{"isolation":"none"}}')
            for marker in runtime.MARKERS:
                fixture.put(marker, "DO-NOT-PRINT-STARTUP")
                with self.subTest(marker=marker), self.assertRaisesRegex(runtime.ProbeError, "residual path"):
                    fixture.probe.validate("none")
                fixture.path(marker).unlink()

    def test_worker_topology_failures(self):
        cases = [
            ("none", "machine"), ("none", "oci"), ("none", "private"),
            ("container", "machine"), ("container", "missing"), ("container", "other-slot"),
            ("container", "two"), ("container", "stopped"), ("container", "zero-pid"),
            ("container", "private-other"), ("container", "bundle-other"), ("container", "unit-other"),
            ("nspawn", "missing"), ("nspawn", "two"), ("nspawn", "oci")]
        for mode, failure in cases:
            with self.subTest(mode=mode, failure=failure), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, mode)
                if failure == "machine":
                    fixture.machine_rows = "systemd-nspawn@kube1.service loaded active running worker\n"
                elif failure == "oci":
                    fixture.runc_rows = [{"id": "unbounded-kube1", "pid": 100, "status": "running"}]
                elif failure == "private":
                    fixture.put("/var/lib/machines/kube1/residue")
                elif failure == "private-other":
                    fixture.put("/var/lib/unbounded/containers/kube2/residue")
                elif failure == "bundle-other":
                    fixture.put("/var/lib/unbounded/containers/bundles/kube2/config.json")
                elif failure == "unit-other":
                    fixture.put("/etc/systemd/system/unbounded-node-kube2.service")
                elif failure == "missing":
                    fixture.runc_rows = []
                    fixture.machine_rows = ""
                elif failure == "other-slot":
                    fixture.runc_rows[0]["id"] = "unbounded-kube2"
                elif failure == "two" and mode == "container":
                    fixture.runc_rows.append({"id": "unbounded-kube2", "pid": 102, "status": "running"})
                elif failure == "two":
                    fixture.machine_rows += "systemd-nspawn@kube2.service loaded active running worker\n"
                elif failure == "stopped":
                    fixture.runc_rows[0]["status"] = "stopped"
                elif failure == "zero-pid":
                    fixture.runc_rows[0]["pid"] = 0
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.validate(mode)

    def test_missing_runtime_cannot_hide_oci_state(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory)
            fixture.path(runtime.RUNC).unlink()
            self.assertEqual(fixture.probe.oci_workers(), [])
            fixture.put("/run/runc/unbounded-kube1/state.json")
            with self.assertRaisesRegex(runtime.ProbeError, "without runtime"):
                fixture.probe.oci_workers()

    def test_nspawn_requires_real_registered_machine(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory, "nspawn")
            original = fixture.probe.command
            def command(argv, **kwargs):
                if argv[:2] == ["machinectl", "show"]:
                    return subprocess.CompletedProcess(argv, 0, stdout="State=closing\nLeader=0\n")
                return original(argv, **kwargs)
            fixture.probe.command = command
            with self.assertRaisesRegex(runtime.ProbeError, "not running"):
                fixture.probe.validate("nspawn")


class ResetTests(unittest.TestCase):
    def test_reset_all_modes_and_idempotency(self):
        for mode in runtime.MODES:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, mode, reset=True)
                fixture.path(runtime.RUNC).unlink()
                expected = {"mode": mode, "reset": True}
                self.assertEqual(fixture.probe.validate_reset(mode), expected)
                self.assertEqual(fixture.probe.validate_reset(mode), expected)

    def test_container_reset_preserves_host_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory, "container", reset=True)
            paths = ("/etc/cni/net.d/host.conf", "/var/lib/cni/networks/host/10.0.0.2",
                     "/var/lib/containerd/meta.db", "/etc/containerd/config.toml")
            for path in paths:
                fixture.put(path, "host-owned")
            fixture.probe.validate_reset("container")
            for path in paths:
                self.assertEqual(fixture.path(path).read_text(), "host-owned")

    def test_none_reset_rejects_cni_ipam_and_containerd_residue(self):
        for path in ("/etc/cni/net.d/host.conf", "/var/lib/cni/networks/host/10.0.0.2",
                     "/var/lib/containerd/meta.db", "/opt/unbounded/node/current"):
            with self.subTest(path=path), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, reset=True)
                fixture.put(path)
                with self.assertRaisesRegex(runtime.ProbeError, "residual path"):
                    fixture.probe.validate_reset("none")

    def test_reset_rejects_owned_residue_and_missing_binary(self):
        paths = (runtime.STATE, runtime.STATE + ".sha256", runtime.CONFIG, "/var/lib/kubelet/config.yaml",
                 "/var/lib/unbounded/containers/kube1/rootfs/file", "/etc/systemd/system/unbounded-node-kube1.service",
                 "/etc/systemd/system/unbounded-node-kube2.service.d/override.conf") + runtime.MARKERS
        for path in paths:
            with self.subTest(path=path), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, "container", reset=True)
                fixture.put(path)
                with self.assertRaises(runtime.ProbeError):
                    fixture.probe.validate_reset("container")
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory, reset=True)
            fixture.path("/opt/unbounded/agent/bin/aks-flex-node").unlink()
            with self.assertRaisesRegex(runtime.ProbeError, "not kept"):
                fixture.probe.validate_reset("none")

    def test_allow_missing_agent_only_skips_binary_assertion(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory, reset=True)
            fixture.path("/opt/unbounded/agent/bin/aks-flex-node").unlink()
            self.assertTrue(fixture.probe.validate_reset("none", allow_missing_agent=True)["reset"])
            with mock.patch.object(runtime, "Runtime", return_value=fixture.probe), contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(runtime.main(["validate-reset", "none", "--allow-missing-agent"]), 0)
            with self.assertRaisesRegex(runtime.ProbeError, "not kept"):
                fixture.probe.validate_reset("none")
            fixture.put("/var/lib/kubelet/config.yaml")
            with self.assertRaisesRegex(runtime.ProbeError, "residual path"):
                fixture.probe.validate_reset("none", allow_missing_agent=True)

    def test_reset_rejects_daemon_or_worker_still_running_or_enabled(self):
        for unit in (runtime.DAEMON, "unbounded-node-kube1.service", "unbounded-kubelet.service"):
            for values in ({"ActiveState": "active", "MainPID": "99"}, {"UnitFileState": "enabled"}):
                with self.subTest(unit=unit, values=values), tempfile.TemporaryDirectory() as directory:
                    fixture = Fixture(directory, reset=True)
                    fixture.overrides[unit] = values
                    with self.assertRaises(runtime.ProbeError):
                        fixture.probe.validate_reset("none")


class DiagnosticsTests(unittest.TestCase):
    def test_diagnostics_do_not_dump_sensitive_material(self):
        for mode in runtime.MODES:
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory, mode)
                fixture.put(runtime.MARKERS[1], "DO-NOT-PRINT-STARTUP")
                result = fixture.probe.diagnostics(mode)
                self.assertNotIn("DO-NOT-PRINT", json.dumps(result))
                self.assertEqual(result["mode"], mode)
                self.assertEqual(result["journal"], {"entries": 1, "priorities": {"3": 1}})
                journal = [argv for argv, _ in fixture.calls if argv[0] == "journalctl"][0]
                self.assertIn("--output-fields=PRIORITY", journal)

    def test_missing_or_corrupt_state_keeps_explicit_mode_and_errors(self):
        for corrupt in (False, True):
            with self.subTest(corrupt=corrupt), tempfile.TemporaryDirectory() as directory:
                fixture = Fixture(directory)
                if corrupt:
                    fixture.put(runtime.STATE, '{"token":"DO-NOT-PRINT"')
                else:
                    fixture.path(runtime.STATE).unlink()
                result = fixture.probe.diagnostics("container")
                self.assertEqual(result["mode"], "container")
                self.assertTrue(result["errors"])
                self.assertNotIn("DO-NOT-PRINT", json.dumps(result))

    def test_boot_guard_uses_only_temporary_markers(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = Fixture(directory)
            fixture.put("/opt/unbounded/node/.check-startup", '#!/bin/sh\nset -eu\n'
                        '[ ! -e "$1" ] || exit 1\n[ ! -e "$2" ] || [ -e "$3" ]\n', executable=True)
            original_state = fixture.path(runtime.STATE).read_bytes()
            fixture.put(runtime.MARKERS[1], "real startup untouched")
            fixture.probe.runner = subprocess.run
            self.assertTrue(fixture.probe.boot_guard("none")["bootGuard"])
            self.assertEqual(fixture.path(runtime.STATE).read_bytes(), original_state)
            self.assertEqual(fixture.path(runtime.MARKERS[1]).read_text(), "real startup untouched")
            fixture.put("/opt/unbounded/node/.check-startup", "#!/bin/sh\nexit 0\n", executable=True)
            with self.assertRaisesRegex(runtime.ProbeError, "boundary"):
                fixture.probe.boot_guard("none")


class TransportTests(unittest.TestCase):
    def test_shell_safe_runtime_on_and_exit_status(self):
        args = ["exec", "--", "printf", "%s", "spaces ' \" $() ; `tick`", "", "line\nbreak", "-n"]
        script = 'source "$1"; shift; remote_exec() { printf "%s\\n" "$1" "$2"; return 37; }; runtime_on "$@"'
        result = subprocess.run(["bash", "-c", script, "test", str(HERE / "lib/runtime.sh"),
                                 "192.0.2.1"] + args, text=True, capture_output=True, timeout=5, check=False)
        self.assertEqual(result.returncode, 37, result.stderr)
        host, command = result.stdout.split("\n", 1)
        self.assertEqual(host, "192.0.2.1")
        words = shlex.split(command)
        self.assertEqual(words[-len(args):], args)
        self.assertEqual(words[:2], ["timeout", "--signal=TERM"])
        self.assertIn("sudo", words)
        self.assertIn("-n", words)
        self.assertIn("/tmp/aks-flex-node-e2e-runtime.py", words)

    def test_runtime_install_uses_existing_copy_helper(self):
        script = ('source "$1"; source "$1"; remote_copy() { printf "%s\\n" "$@"; }; '
                  'runtime_install 192.0.2.1')
        result = subprocess.run(["bash", "-c", script, "test", str(HERE / "lib/runtime.sh")],
                                text=True, capture_output=True, timeout=5, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [str(HERE / "runtime.py"), "192.0.2.1",
                                                     "/tmp/aks-flex-node-e2e-runtime.py"])


if __name__ == "__main__":
    unittest.main()
