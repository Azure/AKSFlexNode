"""Unit tests for flexinstall.py, which run without a VM, network, or Podman:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_flexinstall.py'
"""

from __future__ import annotations

import base64
import gzip
import hashlib
import io
import json
import os
import re
import shlex
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
import urllib.parse
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import flexinstall  # noqa: E402
from errors import StepError  # noqa: E402

TOKEN = "abcdef.0123456789abcdef"
CA_PEM = "-----BEGIN CERTIFICATE-----\nMIIBdummyCA\n-----END CERTIFICATE-----\n"
DIGEST = "sha256:" + "ab" * 32
IMAGE = f"10.0.2.2:5096/aks-flex-node-agent@{DIGEST}"
BOOTSTRAP_UNIT_TEXT = "[Unit]\nDescription=Bootstrap\n\n[Service]\nType=oneshot\nExecStart=/bin/true\n"
SETTINGS_TEXT = f"AKS_FLEX_NODE_IMAGE='{IMAGE}'\nAKS_FLEX_NODE_PULL_ARGS='--plain-http'\n"
BASE_CONFIG_BYTES = json.dumps({"azure": {"bootstrapToken": {"token": TOKEN}}}).encode()
HAS_BASH = shutil.which("bash") is not None


def node(**overrides: str) -> flexinstall.FlexNode:
    values = {
        "node_name": "acl-abupdate", "node_ip": "192.168.113.10", "api_server": "192.168.113.2:6443",
        "ca_pem": CA_PEM, "kubernetes_version": "1.37.0", "bootstrap_token": TOKEN, "agent_image": IMAGE,
        **overrides,
    }
    return flexinstall.FlexNode(**values)


def completed(stdout: str = "", returncode: int = 0, stderr: str = "") -> subprocess.CompletedProcess[str]:
    return subprocess.CompletedProcess(["fake"], returncode, stdout, stderr)


def gzip_data_url(text: str) -> str:
    return "data:;base64," + base64.b64encode(gzip.compress(text.encode())).decode()


def butane_output() -> dict:
    """Shaped like Butane's output for the container .bu: a gzip+base64 file, a percent-encoded one, a
    0700 directory, an enabled unit with a drop-in, a masked unit, and a file without overwrite."""
    return {
        "ignition": {"version": "3.4.0"},
        "storage": {
            "directories": [{"path": "/etc/aks-flex-node/credentials", "mode": 0o700}],
            "files": [
                {"path": "/etc/sysconfig/aks-flex-node-container", "mode": 384, "overwrite": True,
                 "contents": {"compression": "gzip", "source": gzip_data_url(SETTINGS_TEXT)}},
                {"path": "/etc/aks-flex-node/bootstrap.env", "mode": 384, "overwrite": True,
                 "contents": {"compression": "", "source": "data:," + urllib.parse.quote(flexinstall.bootstrap_env())}},
                {"path": "/etc/flex-harness/note", "contents": {"source": "data:,hello%20world"}},
            ],
        },
        "systemd": {"units": [
            {"name": "aks-flex-node-bootstrap.service", "enabled": True, "contents": BOOTSTRAP_UNIT_TEXT,
             "dropins": [{"name": "10-harness.conf", "contents": "[Service]\nEnvironment=A=1\n"}]},
            {"name": "aks-flex-node-reset.service", "contents": "[Unit]\nDescription=Reset\n"},
            {"name": "waagent.service", "enabled": False, "mask": True},
        ]},
    }


def members(payload: flexinstall.IgnitionPayload) -> dict[str, tuple[tarfile.TarInfo, bytes | None]]:
    out = {}
    with tarfile.open(fileobj=io.BytesIO(payload.tar), mode="r:") as tar:
        for info in tar.getmembers():
            data = tar.extractfile(info).read() if info.isfile() else None
            out[info.name] = (info, data)
    return out


def bash_syntax(script: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(["bash", "-n"], input=script, text=True, capture_output=True, check=False)


class FakeSsh:
    """Answers each command with the first response whose key is in it, and records the commands."""

    def __init__(self, responses: list[tuple[str, subprocess.CompletedProcess[str]]]) -> None:
        self.responses = responses
        self.calls: list[tuple[str, str | None]] = []

    def __call__(self, command: str, *, input: str | None = None, timeout: float = 120,
                 check: bool = True) -> subprocess.CompletedProcess[str]:
        self.calls.append((command, input))
        for key, result in self.responses:
            if key in command:
                if check and result.returncode != 0:
                    raise StepError(f"{command!r} exited {result.returncode}")
                return result
        return completed()


class IgnitionPayloadTest(unittest.TestCase):
    def payload(self) -> flexinstall.IgnitionPayload:
        return flexinstall.ignition_payload(
            butane_output(), extra_files={flexinstall.BASE_CONFIG: (0o600, BASE_CONFIG_BYTES)})

    def test_members(self) -> None:
        got = members(self.payload())
        want = {
            "etc/aks-flex-node/credentials": (0o700, None),
            "etc/sysconfig/aks-flex-node-container": (0o600, SETTINGS_TEXT.encode()),
            "etc/aks-flex-node/bootstrap.env": (0o600, flexinstall.bootstrap_env().encode()),
            "etc/aks-flex-node/base-config.json": (0o600, BASE_CONFIG_BYTES),
            "etc/flex-harness/note": (0o644, b"hello world"),
            "etc/systemd/system/aks-flex-node-bootstrap.service": (0o644, BOOTSTRAP_UNIT_TEXT.encode()),
            "etc/systemd/system/aks-flex-node-bootstrap.service.d/10-harness.conf":
                (0o644, b"[Service]\nEnvironment=A=1\n"),
            "etc/systemd/system/aks-flex-node-reset.service": (0o644, b"[Unit]\nDescription=Reset\n"),
        }
        self.assertEqual(sorted(got), sorted(want))
        for name, (mode, data) in want.items():
            with self.subTest(name=name):
                info, content = got[name]
                self.assertEqual(stat.S_IMODE(info.mode), mode)
                self.assertEqual((info.uid, info.gid, info.uname, info.gname), (0, 0, "root", "root"))
                self.assertEqual(info.isdir(), data is None)
                self.assertEqual(content, data)
        # Nothing for the masked unit, and no entries for directories that may exist, such as /etc.
        self.assertNotIn("etc/systemd/system/waagent.service", got)
        self.assertNotIn("etc", got)

    def test_directories_come_first(self) -> None:
        names = list(members(self.payload()))
        self.assertEqual(names[0], "etc/aks-flex-node/credentials")

    def test_units_and_paths(self) -> None:
        payload = self.payload()
        self.assertEqual(payload.enable, ["aks-flex-node-bootstrap.service"])
        self.assertEqual(payload.mask, ["waagent.service"])
        self.assertEqual(payload.disable, ["waagent.service"])
        self.assertEqual(payload.create_only, ["/etc/flex-harness/note"])
        for path in ("/etc/aks-flex-node", "/etc/aks-flex-node/base-config.json", "/etc/systemd/system",
                     "/etc/systemd/system/aks-flex-node-bootstrap.service.d"):
            self.assertIn(path, payload.paths)
        self.assertNotIn("/etc", payload.paths)
        self.assertEqual(payload.paths, sorted(payload.paths))

    def test_unsupported(self) -> None:
        cases = {
            "passwd": lambda c: c.update(passwd={"users": [{"name": "core"}]}),
            "links": lambda c: c["storage"].update(links=[{"path": "/etc/l", "target": "/x"}]),
            "filesystems": lambda c: c["storage"].update(filesystems=[{"device": "/dev/vdb"}]),
            "remote source": lambda c: c["storage"]["files"][2]["contents"].update(source="https://example.com/x"),
            "append": lambda c: c["storage"]["files"][2].update(append=[{"source": "data:,x"}]),
            "file owner": lambda c: c["storage"]["files"][2].update(user={"name": "core"}),
            "verification": lambda c: c["storage"]["files"][2]["contents"].update(verification={"hash": "x"}),
            "compression": lambda c: c["storage"]["files"][2]["contents"].update(compression="xz"),
            "version 2": lambda c: c["ignition"].update(version="2.3.0"),
            "merge": lambda c: c["ignition"].update(config={"merge": [{"source": "https://x"}]}),
            "relative path": lambda c: c["storage"]["files"][2].update(path="etc/x"),
            "dot-dot path": lambda c: c["storage"]["files"][2].update(path="/etc/../x"),
            "bad unit name": lambda c: c["systemd"]["units"][1].update(name="../evil.service"),
            "masked with contents": lambda c: c["systemd"]["units"][2].update(contents="[Unit]\n"),
            "duplicate": lambda c: c["storage"]["files"].append(dict(c["storage"]["files"][2])),
            "bad base64": lambda c: c["storage"]["files"][2]["contents"].update(source="data:;base64,***"),
            "bad mode": lambda c: c["storage"]["files"][2].update(mode=0o10000),
        }
        for name, change in cases.items():
            with self.subTest(name):
                config = butane_output()
                change(config)
                with self.assertRaises(StepError):
                    flexinstall.ignition_payload(config)

    def test_extra_file_collides(self) -> None:
        with self.assertRaises(StepError):
            flexinstall.ignition_payload(butane_output(), extra_files={flexinstall.BOOTSTRAP_INPUTS: (0o600, b"x")})

    def test_errors_never_quote_contents(self) -> None:
        config = butane_output()
        config["storage"]["files"][2]["contents"] = {"source": f"data:,{TOKEN}", "compression": "gzip"}
        with self.assertRaises(StepError) as raised:
            flexinstall.ignition_payload(config)
        self.assertNotIn(TOKEN, str(raised.exception))

    def test_empty_sections(self) -> None:
        payload = flexinstall.ignition_payload({"ignition": {"version": "3.4.0"}, "storage": {"directories": None},
                                                "passwd": {}})
        self.assertEqual((payload.enable, payload.mask, payload.paths), ([], [], []))


class ApplyCommandTest(unittest.TestCase):
    def payload(self) -> flexinstall.IgnitionPayload:
        config = butane_output()
        config["storage"]["files"].append({"path": "/etc/flex dir/it's $(here).conf", "mode": 0o600,
                                           "overwrite": False, "contents": {"source": "data:,x"}})
        return flexinstall.ignition_payload(config, extra_files={flexinstall.BASE_CONFIG: (0o600, BASE_CONFIG_BYTES)})

    @unittest.skipUnless(HAS_BASH, "bash isn't installed")
    def test_valid_bash(self) -> None:
        result = bash_syntax(flexinstall.apply_command(self.payload()))
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_quotes_paths(self) -> None:
        command = flexinstall.apply_command(self.payload())
        restorecon = next(line for line in command.splitlines() if "restorecon" in line)
        self.assertIn("/etc/flex dir/it's $(here).conf", shlex.split(restorecon))
        self.assertIn("/etc/flex dir", shlex.split(restorecon))
        self.assertNotIn("$(here)", restorecon.replace(shlex.quote("/etc/flex dir/it's $(here).conf"), ""))
        create_only = [line for line in command.splitlines() if line.startswith("if sudo test")]
        self.assertEqual(len(create_only), 2)
        self.assertIn(shlex.quote("/etc/flex dir/it's $(here).conf"), create_only[0])

    def test_order(self) -> None:
        lines = flexinstall.apply_command(self.payload()).splitlines()
        steps = [next(i for i, line in enumerate(lines) if marker in line)
                 for marker in ("sudo test -e", "sudo tar", "sudo restorecon -F", "daemon-reload",
                                "systemctl enable -- aks-flex-node-bootstrap.service", "systemctl mask -- waagent.service")]
        self.assertEqual(steps, sorted(steps))
        self.assertEqual(lines[0], "set -eu")
        # Only tar reads the archive on stdin.
        self.assertEqual(sum("--file=-" in line for line in lines), 1)

    @unittest.skipUnless(shutil.which("tar"), "tar isn't installed")
    def test_tar_extracts_with_modes(self) -> None:
        payload = flexinstall.ignition_payload(butane_output(),
                                               extra_files={flexinstall.BASE_CONFIG: (0o600, BASE_CONFIG_BYTES)})
        tar_line = next(line for line in flexinstall.apply_command(payload).splitlines() if "tar --extract" in line)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # A directory that exists keeps its mode.
            (root / "etc/aks-flex-node/credentials").mkdir(parents=True, mode=0o755)
            (root / "etc/aks-flex-node/credentials").chmod(0o755)
            args = [arg if arg != "--directory=/" else f"--directory={root}" for arg in shlex.split(tar_line)[1:]]
            result = subprocess.run(args, input=payload.tar, capture_output=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(stat.S_IMODE((root / "etc/aks-flex-node/base-config.json").stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE((root / "etc/sysconfig/aks-flex-node-container").stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE((root / "etc/aks-flex-node/credentials").stat().st_mode), 0o755)
            self.assertEqual((root / "etc/sysconfig/aks-flex-node-container").read_text(), SETTINGS_TEXT)


class ConfigTest(unittest.TestCase):
    def test_base_config(self) -> None:
        config = flexinstall.base_config(node())
        kubelet = config["node"]["kubelet"]
        self.assertEqual(kubelet["nodeIP"], "192.168.113.10")
        self.assertEqual(kubelet["clusterFQDN"], "192.168.113.2:6443")
        self.assertEqual(base64.b64decode(kubelet["caCertData"]).decode(), CA_PEM)
        self.assertEqual(config["agent"]["nodeName"], "acl-abupdate")
        self.assertEqual(config["agent"]["machineClient"],
                         {"mode": "in-cluster", "endpointUrl": flexinstall.PROXY_PATH})
        self.assertTrue(config["agent"]["requireMachineRegistration"])
        self.assertEqual(config["components"]["kubernetes"], "1.37.0")
        self.assertEqual(config["azure"]["bootstrapToken"], {"token": TOKEN})
        self.assertEqual(config["networking"]["dnsServiceIP"], "10.96.0.10")
        self.assertEqual(flexinstall.PROXY_PATH,
                         "/api/v1/namespaces/kube-system/services/http:aks-flex-controller:80/proxy")

    def test_token_stays_out_of_repr_and_errors(self) -> None:
        self.assertNotIn(TOKEN, repr(node()))
        with self.assertRaises(StepError) as raised:
            flexinstall.base_config(node(bootstrap_token="ABCDEF.0123456789abcdef"))
        self.assertNotIn("ABCDEF", str(raised.exception))
        with self.assertRaises(StepError):
            flexinstall.base_config(node(ca_pem="not a certificate"))

    def test_env_file_value(self) -> None:
        cases = {
            "10.0.2.2:5096/a@sha256:00": "'10.0.2.2:5096/a@sha256:00'",
            "--plain-http --skip-verify": "'--plain-http --skip-verify'",
            "$HOME": "'$HOME'",
            "it's": '"it\'s"',
            'it\'s "a" \\ b': '"it\'s \\"a\\" \\\\ b"',
        }
        for value, want in cases.items():
            with self.subTest(value=value):
                self.assertEqual(flexinstall.env_file_value(value), want)

    def test_container_env(self) -> None:
        self.assertEqual(flexinstall.container_env(node()),
                         f"AKS_FLEX_NODE_IMAGE='{IMAGE}'\nAKS_FLEX_NODE_PULL_ARGS='--plain-http'\n")
        self.assertEqual(flexinstall.container_env(node(pull_args="")), f"AKS_FLEX_NODE_IMAGE='{IMAGE}'\n")
        with self.assertRaises(StepError):
            flexinstall.container_env(node(pull_args="--plain-http\nAKS_FLEX_NODE_IMAGE=evil"))

    def test_bootstrap_env(self) -> None:
        # Not quoted: the image's aks-flex-node-bootstrap takes values as written.
        self.assertEqual(flexinstall.bootstrap_env(),
                         "AKS_FLEX_NODE_BASE_CONFIG_FILE=/etc/aks-flex-node/base-config.json\n")
        self.assertNotIn(TOKEN, flexinstall.bootstrap_env())


class RenderButaneTest(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory)
        self.template = self.directory / "agent.bu"
        self.template.write_text("variant: flatcar\nversion: 1.1.0\n")
        self.work = self.directory / "work"
        self.work.mkdir()
        self.seen: dict = {}

    def fake_run(self, result: subprocess.CompletedProcess[str] | Exception):
        def run(cmd: list[str], *, input: str | None = None, timeout: float | None = None,
                stream: bool = False) -> subprocess.CompletedProcess[str]:
            files_dir = next((Path(arg.split(":", 1)[0]) for arg in cmd if arg.startswith(str(self.work))), None)
            if "--files-dir" in cmd and cmd[cmd.index("--files-dir") + 1] != "/files":
                files_dir = Path(cmd[cmd.index("--files-dir") + 1])
            self.seen = {
                "cmd": cmd, "input": input, "dir": files_dir,
                "dir_mode": stat.S_IMODE(files_dir.stat().st_mode),
                "files": {path.name: (stat.S_IMODE(path.stat().st_mode), path.read_bytes())
                          for path in files_dir.iterdir()},
            }
            if isinstance(result, Exception):
                raise result
            return result
        return run

    def render(self, result, *, butane: str | None) -> dict:
        with mock.patch.object(flexinstall.shutil, "which", return_value=butane):
            return flexinstall.render_butane(self.template, {"agent-container.env": "A=1\n", "bootstrap.env": b"B=2\n"},
                                             run=self.fake_run(result), butane_image="example/butane@sha256:00",
                                             workdir=self.work)

    def test_podman_without_butane(self) -> None:
        config = self.render(completed('{"ignition": {"version": "3.4.0"}}'), butane=None)
        self.assertEqual(config, {"ignition": {"version": "3.4.0"}})
        cmd = self.seen["cmd"]
        self.assertEqual(cmd[:4], ["podman", "run", "--rm", "-i"])
        self.assertIn(f"{self.seen['dir']}:/files:ro,z", cmd)
        self.assertEqual(cmd[-4:], ["example/butane@sha256:00", "--strict", "--files-dir", "/files"])
        self.assertEqual(self.seen["input"], self.template.read_text())
        self.assertEqual(self.seen["dir_mode"], 0o700)
        self.assertEqual(self.seen["files"], {"agent-container.env": (0o600, b"A=1\n"), "bootstrap.env": (0o600, b"B=2\n")})
        self.assertEqual(list(self.work.iterdir()), [])

    def test_butane_on_path(self) -> None:
        self.render(completed('{"ignition": {"version": "3.4.0"}}'), butane="/usr/local/bin/butane")
        self.assertEqual(self.seen["cmd"], ["/usr/local/bin/butane", "--strict", "--files-dir", str(self.seen["dir"])])
        self.assertEqual(list(self.work.iterdir()), [])

    def test_removes_its_directory_on_failure(self) -> None:
        cases = {
            "exit status": completed("", 1, "error: unknown field"),
            "not JSON": completed("not json"),
            "not a config": completed("[1]"),
            "run raises": RuntimeError("boom"),
        }
        for name, result in cases.items():
            with self.subTest(name):
                with self.assertRaises((StepError, RuntimeError)):
                    self.render(result, butane=None)
                self.assertEqual(list(self.work.iterdir()), [])

    def test_bad_file_name(self) -> None:
        with self.assertRaises(StepError):
            flexinstall.render_butane(self.template, {"../x": "y"}, run=self.fake_run(completed("{}")),
                                      butane_image="x", workdir=self.work)


class RegistryTest(unittest.TestCase):
    def test_push(self) -> None:
        calls: list[list[str]] = []

        def run(cmd: list[str], **_: object) -> subprocess.CompletedProcess[str]:
            calls.append(cmd)
            if cmd[:2] == ["podman", "container"]:
                return completed("true\n")
            if cmd[:2] == ["podman", "push"]:
                Path(cmd[cmd.index("--digestfile") + 1]).write_text(DIGEST)
            return completed()

        logs: list[str] = []
        registry = flexinstall.Registry("smoke", 5096, run=run, log=logs.append)
        reference = registry.push("localhost/aks-flex-node-agent:smoke", "aks-flex-node-agent", "smoke")
        self.assertEqual(reference, f"10.0.2.2:5096/aks-flex-node-agent@{DIGEST}")
        push = calls[-1]
        self.assertEqual(push[-2:], ["localhost/aks-flex-node-agent:smoke", "127.0.0.1:5096/aks-flex-node-agent:smoke"])
        self.assertIn("--tls-verify=false", push)
        self.assertFalse(Path(push[push.index("--digestfile") + 1]).exists())

    def test_push_fails(self) -> None:
        cases = {
            "push exits": (1, "sha256:" + "0" * 64),
            "bad digest": (0, "md5:00"),
        }
        for name, (code, digest) in cases.items():
            with self.subTest(name):
                def run(cmd: list[str], code: int = code, digest: str = digest,
                        **_: object) -> subprocess.CompletedProcess[str]:
                    if cmd[:2] == ["podman", "container"]:
                        return completed("true\n")
                    if cmd[:2] == ["podman", "push"]:
                        Path(cmd[cmd.index("--digestfile") + 1]).write_text(digest)
                        return completed("", code, "denied")
                    return completed()

                with self.assertRaises(StepError):
                    flexinstall.Registry("smoke", 5096, run=run, log=lambda _: None).push("a", "b", "c")

    def test_start_publishes_on_loopback(self) -> None:
        calls: list[list[str]] = []

        def run(cmd: list[str], **_: object) -> subprocess.CompletedProcess[str]:
            calls.append(cmd)
            return completed("", 1) if cmd[:2] == ["podman", "container"] else completed()

        registry = flexinstall.Registry("smoke", 5096, run=run, log=lambda _: None)
        with mock.patch.object(registry, "answers", return_value=True):
            registry.start()
        started = next(cmd for cmd in calls if cmd[:2] == ["podman", "run"])
        self.assertEqual(started[started.index("--publish") + 1], "127.0.0.1:5096:5000")
        self.assertEqual(started[-1], flexinstall.REGISTRY_IMAGE)
        self.assertIn(["podman", "rm", "--force", "--ignore", "smoke"], calls)

    def test_build(self) -> None:
        calls: list[tuple[list[str], dict]] = []

        def run(cmd: list[str], **kwargs: object) -> subprocess.CompletedProcess[str]:
            calls.append((cmd, kwargs))
            if "rev-parse" in cmd:
                return completed("abc1234\n")
            return completed("", 1) if "diff" in cmd else completed()

        flexinstall.build_agent_image(Path("/repo"), "localhost/aks-flex-node-agent:t", run=run, log=lambda _: None)
        build, kwargs = calls[-1]
        self.assertEqual(build[:4], ["podman", "build", "-f", "/repo/Dockerfile.aks-flex-node-agent"])
        self.assertIn("GIT_COMMIT=abc1234-dirty", build)
        self.assertEqual(build[-3:], ["-t", "localhost/aks-flex-node-agent:t", "/repo"])
        self.assertTrue(kwargs.get("stream"))


class ProvisionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.repo = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.repo)
        (self.repo / "scripts").mkdir()
        (self.repo / flexinstall.BUTANE_TEMPLATE).write_text("variant: flatcar\n")

    def butane(self, cmd: list[str], *, input: str | None = None, **_: object) -> subprocess.CompletedProcess[str]:
        files = Path(cmd[cmd.index("-v") + 1].split(":", 1)[0])
        rendered = {
            "ignition": {"version": "3.4.0"},
            "storage": {"files": [
                {"path": flexinstall.SETTINGS, "mode": 384, "overwrite": True,
                 "contents": {"compression": "gzip", "source": gzip_data_url((files / "agent-container.env").read_text())}},
                {"path": flexinstall.BOOTSTRAP_INPUTS, "mode": 384, "overwrite": True,
                 "contents": {"compression": "gzip", "source": gzip_data_url((files / "bootstrap.env").read_text())}},
            ]},
            "systemd": {"units": [
                {"name": flexinstall.BOOTSTRAP_UNIT, "enabled": True, "contents": BOOTSTRAP_UNIT_TEXT},
                {"name": flexinstall.RESET_UNIT, "contents": "[Unit]\n"},
            ]},
        }
        return completed(json.dumps(rendered))

    def test_provision(self) -> None:
        ssh = FakeSsh([("test -e /etc/systemd/system/aks-flex-node-agent.service", completed("", 1))])
        sent: list[tuple[str, bytes]] = []

        def ssh_bytes(command: str, data: bytes, *, timeout: float = 600) -> subprocess.CompletedProcess[bytes]:
            sent.append((command, data))
            return subprocess.CompletedProcess(["ssh"], 0, b"", b"")

        logs: list[str] = []
        with mock.patch.object(flexinstall.shutil, "which", return_value=None):
            flexinstall.provision(node(), repo=self.repo, ssh=ssh, ssh_bytes=ssh_bytes, run=self.butane, log=logs.append)
        command, data = sent[0]
        payload = members(flexinstall.IgnitionPayload(tar=data, enable=[], mask=[], paths=[]))
        info, base = payload["etc/aks-flex-node/base-config.json"]
        self.assertEqual(stat.S_IMODE(info.mode), 0o600)
        self.assertEqual(json.loads(base or b"")["azure"]["bootstrapToken"]["token"], TOKEN)
        self.assertEqual(payload["etc/aks-flex-node/bootstrap.env"][1], flexinstall.bootstrap_env().encode())
        self.assertIn("sudo systemctl enable -- aks-flex-node-bootstrap.service", command)
        self.assertEqual(ssh.calls[-1][0], "sudo systemctl start --no-block aks-flex-node-bootstrap.service")
        # The token travels only in the archive.
        for text in [*logs, command, *(call for call, _ in ssh.calls)]:
            self.assertNotIn(TOKEN, text)

    def test_refuses_an_installed_agent(self) -> None:
        ssh = FakeSsh([("test -e /etc/systemd/system/aks-flex-node-agent.service", completed())])
        with self.assertRaises(StepError):
            flexinstall.provision(node(), repo=self.repo, ssh=ssh, ssh_bytes=mock.Mock(), run=self.butane,
                                  log=lambda _: None)

    def test_apply_failure(self) -> None:
        ssh = FakeSsh([("test -e", completed("", 1))])
        with mock.patch.object(flexinstall.shutil, "which", return_value=None), self.assertRaises(StepError) as raised:
            flexinstall.provision(node(), repo=self.repo, ssh=ssh, log=lambda _: None, run=self.butane,
                                  ssh_bytes=lambda *a, **k: subprocess.CompletedProcess([], 2, b"", f"tar: {TOKEN}".encode()))
        self.assertNotIn(TOKEN, str(raised.exception))
        self.assertNotIn("--no-block", " ".join(call for call, _ in ssh.calls))


BOOTSTRAP_DONE = """\
### bootstrap
ActiveState=inactive
SubState=dead
Result=success
ExecMainStatus=0
ExecMainExitTimestampMonotonic=123456
ConditionResult=yes
NRestarts=1
### agent
is-enabled=enabled
is-active=active
installed=yes
### inputs
"""


def with_section(text: str, section: str, body: str) -> str:
    sections = flexinstall.parse_sections(text)
    sections[section] = body
    return "".join(f"### {name}\n{content}\n" for name, content in sections.items())


class BootstrapFindingsTest(unittest.TestCase):
    def findings(self, output: str) -> list[tuple[bool, str, str]]:
        return flexinstall.bootstrap_findings(FakeSsh([("### bootstrap", completed(output))]))

    def test_pass(self) -> None:
        findings = self.findings(BOOTSTRAP_DONE)
        self.assertEqual(len(findings), 3)
        self.assertEqual([detail for ok, _, detail in findings if not ok], [])

    def test_skipped_after_a_reboot(self) -> None:
        output = BOOTSTRAP_DONE.replace("ExecMainExitTimestampMonotonic=123456", "ExecMainExitTimestampMonotonic=0")
        output = output.replace("ConditionResult=yes", "ConditionResult=no")
        self.assertTrue(all(ok for ok, _, _ in self.findings(output)))

    def test_fail(self) -> None:
        cases = {
            "running": (BOOTSTRAP_DONE.replace("ActiveState=inactive", "ActiveState=activating"), "succeeded"),
            "exit 78": (BOOTSTRAP_DONE.replace("ExecMainStatus=0", "ExecMainStatus=78")
                        .replace("Result=success", "Result=exit-code"), "succeeded"),
            "never ran": (BOOTSTRAP_DONE.replace("ExecMainExitTimestampMonotonic=123456",
                                                 "ExecMainExitTimestampMonotonic=0"), "succeeded"),
            "agent inactive": (BOOTSTRAP_DONE.replace("is-active=active", "is-active=activating"), "enabled and active"),
            "inputs left": (with_section(BOOTSTRAP_DONE, "inputs", "/etc/aks-flex-node/bootstrap.env\n"
                                                                  "/etc/aks-flex-node/base-config.json"), "inputs"),
        }
        for name, (output, description) in cases.items():
            with self.subTest(name):
                failed = [(d, detail) for ok, d, detail in self.findings(output) if not ok]
                self.assertEqual(len(failed), 1, failed)
                self.assertIn(description, failed[0][0])
        failed = [detail for ok, _, detail in self.findings(cases["inputs left"][0]) if not ok]
        self.assertEqual(failed, ["/etc/aks-flex-node/bootstrap.env, /etc/aks-flex-node/base-config.json"])

    def test_unreadable(self) -> None:
        with self.assertRaises(StepError):
            flexinstall.bootstrap_findings(FakeSsh([("### bootstrap", completed("", 255, "timed out"))]))

    def test_wait(self) -> None:
        logs: list[str] = []
        flexinstall.wait_for_bootstrap(FakeSsh([("### bootstrap", completed(BOOTSTRAP_DONE))]), timeout=1,
                                       log=logs.append, interval=0)
        self.assertIn("installed", logs[-1])
        failed = BOOTSTRAP_DONE.replace("ActiveState=inactive", "ActiveState=failed")
        ssh = FakeSsh([("### bootstrap", completed(failed)), ("journalctl", completed(f"token {TOKEN} rejected"))])
        with self.assertRaises(StepError) as raised:
            flexinstall.wait_for_bootstrap(ssh, timeout=1, log=lambda _: None, interval=0)
        self.assertNotIn(TOKEN, str(raised.exception))
        self.assertIn("[token redacted]", str(raised.exception))
        running = BOOTSTRAP_DONE.replace("ActiveState=inactive", "ActiveState=activating")
        with self.assertRaises(StepError):
            flexinstall.wait_for_bootstrap(FakeSsh([("### bootstrap", completed(running))]), timeout=0,
                                           log=lambda _: None, interval=0)


AGENT_OK = """\
### tasks
TASK             PID     STATUS
aks-flex-node    4242    RUNNING
### context
system_u:system_r:spc_t:s0
### unit
is-active=active
### state
{"appliedSettingsVersion":"1","activeMachine":"kube1"}
### machines
kube1 running
kube2
"""


class AgentFindingsTest(unittest.TestCase):
    def findings(self, output: str) -> list[tuple[bool, str, str]]:
        return flexinstall.agent_findings(FakeSsh([("### tasks", completed(output))]))

    def test_pass(self) -> None:
        findings = self.findings(AGENT_OK)
        self.assertEqual(len(findings), 4)
        self.assertEqual([detail for ok, _, detail in findings if not ok], [])

    def test_fail(self) -> None:
        cases = {
            "no task": (with_section(AGENT_OK, "tasks", "TASK PID STATUS"), "RUNNING"),
            "stopped": (AGENT_OK.replace("RUNNING", "STOPPED"), "RUNNING"),
            "kernel_t": (AGENT_OK.replace("spc_t", "kernel_t"), "spc_t"),
            "unit failed": (AGENT_OK.replace("is-active=active", "is-active=failed"), "is active"),
            "machine stopped": (AGENT_OK.replace("kube1 running", "kube1 "), "active machine"),
            "other machine": (AGENT_OK.replace('"kube1"', '"kube2"'), "active machine"),
            "no state": (with_section(AGENT_OK, "state", ""), "active machine"),
        }
        for name, (output, description) in cases.items():
            with self.subTest(name):
                failed = [d for ok, d, _ in self.findings(output) if not ok]
                self.assertEqual(len(failed), 1, failed)
                self.assertIn(description, failed[0])

    def test_daemon_state(self) -> None:
        cases = {
            "present": (completed('{"activeMachine": "kube2"}\n'), {"activeMachine": "kube2"}),
            "absent": (completed("", 1, "No such file"), {}),
            "garbage": (completed("{not json"), {}),
            "not an object": (completed("[1, 2]"), {}),
        }
        for name, (result, want) in cases.items():
            with self.subTest(name):
                ssh = FakeSsh([("daemon-state.json", result)])
                self.assertEqual(flexinstall.daemon_state(ssh), want)
                self.assertEqual(ssh.calls[0][0], "sudo cat /etc/aks-flex-node/daemon-state.json")

    def test_wait_for_agent(self) -> None:
        ready = "### agent\nstatus=RUNNING\nstate=active\nready=yes\ndenied=\n"
        logs: list[str] = []
        flexinstall.wait_for_agent(FakeSsh([("date +%s", completed("1700000000\n")), ("### agent", completed(ready))]),
                                   timeout=1, log=logs.append, interval=0)
        self.assertIn("running", logs[-1])
        denied = "### agent\nstatus=\nstate=activating\nready=no\ndenied=flex-agent: error: the host's " \
                 "machinectl failed: Access denied\n"
        with self.assertRaises(StepError) as raised:
            flexinstall.wait_for_agent(FakeSsh([("### agent", completed(denied))]), timeout=60, log=lambda _: None,
                                       interval=0)
        self.assertIn("spc_t kernel_t:dbus", str(raised.exception))
        self.assertTrue(flexinstall.agent_ready(FakeSsh([("### agent", completed(ready))])))
        self.assertFalse(flexinstall.agent_ready(FakeSsh([("### agent", completed(ready.replace("yes", "no")))])))


HASH_A, HASH_B = "a" * 64, "b" * 64
MANIFEST = f"""\
{HASH_A}  /etc/aks-flex-node/config.json
{HASH_A}  /etc/aks-flex-node/daemon-state.json
{HASH_B}  /etc/systemd/system/multi-user.target.wants/aks-flex-node-agent.service
{HASH_B}  /etc/systemd/nspawn/kube1.nspawn
image 10.0.2.2:5096/aks-flex-node-agent@{DIGEST} {DIGEST}
active kube1
machine kube1 running
machine kube2 stopped
sudo: a warning nobody asked for
"""


class ManifestTest(unittest.TestCase):
    def test_parse(self) -> None:
        manifest = flexinstall.parse_state_manifest(MANIFEST)
        self.assertEqual(manifest, {
            "/etc/aks-flex-node/config.json": HASH_A,
            "/etc/aks-flex-node/daemon-state.json": HASH_A,
            "/etc/systemd/system/multi-user.target.wants/aks-flex-node-agent.service": HASH_B,
            "/etc/systemd/nspawn/kube1.nspawn": HASH_B,
            f"image 10.0.2.2:5096/aks-flex-node-agent@{DIGEST}": DIGEST,
            "active": "kube1",
            "machine kube1": "running",
            "machine kube2": "stopped",
        })

    def test_unchanged(self) -> None:
        manifest = flexinstall.parse_state_manifest(MANIFEST)
        findings = flexinstall.manifest_findings(manifest, dict(manifest))
        self.assertEqual(len(findings), 3)
        self.assertTrue(all(ok for ok, _, _ in findings), findings)

    def test_changes(self) -> None:
        before = flexinstall.parse_state_manifest(MANIFEST)
        cases = {
            "file changed": (lambda m: m.update({"/etc/aks-flex-node/config.json": HASH_B}), 0,
                             "changed: /etc/aks-flex-node/config.json"),
            "file removed": (lambda m: m.pop("/etc/systemd/nspawn/kube1.nspawn"), 0,
                             "removed: /etc/systemd/nspawn/kube1.nspawn"),
            "file added": (lambda m: m.update({"/etc/aks-flex-node/agent-image.env": HASH_A}), 0,
                           "added: /etc/aks-flex-node/agent-image.env"),
            "image changed": (lambda m: m.update({f"image 10.0.2.2:5096/aks-flex-node-agent@{DIGEST}": "sha256:00"}), 1,
                              f"changed: 10.0.2.2:5096/aks-flex-node-agent@{DIGEST}"),
            "machine stopped": (lambda m: m.update({"machine kube1": "stopped"}), 2, "after: kube1 stopped"),
            "other machine": (lambda m: m.update({"active": "kube2", "machine kube2": "running"}), 2, "after: kube2"),
        }
        for name, (change, index, detail) in cases.items():
            with self.subTest(name):
                after = dict(before)
                change(after)
                findings = flexinstall.manifest_findings(before, after)
                self.assertEqual([i for i, (ok, _, _) in enumerate(findings) if not ok], [index])
                self.assertIn(detail, findings[index][2])
                self.assertNotIn(HASH_A, findings[index][2])
                self.assertNotIn(HASH_B, findings[index][2])

    def test_empty_before_fails(self) -> None:
        self.assertFalse(any(ok for ok, _, _ in flexinstall.manifest_findings({}, {})))

    @unittest.skipUnless(HAS_BASH, "bash isn't installed")
    def test_script_parses(self) -> None:
        result = bash_syntax(flexinstall.STATE_MANIFEST_SCRIPT)
        self.assertEqual(result.returncode, 0, result.stderr)
        # As core, it runs the function again under sudo, from its definition.
        script = (flexinstall.STATE_MANIFEST_SCRIPT.replace('if [ "$(id -u)" = 0 ]', "if false")
                  .replace("sudo bash -c", "bash -n -c"))
        result = subprocess.run(["bash", "-c", script], capture_output=True, text=True, check=False)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (0, "", ""))

    def test_script_prints_no_contents(self) -> None:
        script = flexinstall.STATE_MANIFEST_SCRIPT
        for reader in ("cat", "head", "tail", "less", "more", "strings", "xxd", "od", "base64", "grep"):
            self.assertIsNone(re.search(rf"(^|[\s|;(]){reader}\s", script), reader)
        self.assertTrue(all(".activeMachine" in line for line in script.splitlines() if "jq " in line))

    @unittest.skipUnless(HAS_BASH and shutil.which("jq") and shutil.which("sha256sum"), "bash, jq, or sha256sum is missing")
    def test_script_runs(self) -> None:
        """Run the script against a fake root, as root, with ctr and machinectl stubbed."""
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            secret = b"-----BEGIN EC PRIVATE KEY-----\nsecret\n-----END EC PRIVATE KEY-----\n"
            files = {
                "etc/aks-flex-node/config.json": b"{}",
                "etc/aks-flex-node/daemon-state.json": b'{"activeMachine": "kube1"}',
                "etc/aks-flex-node/daemon-credentials/client.key": secret,
                "etc/unbounded/agent/kube1-applied-config.json": b"{}",
                "etc/systemd/system/aks-flex-node-agent.service": b"[Unit]\n",
                "etc/systemd/system/aks-flex-node-agent.service.d/10-x.conf": b"[Unit]\n",
                "etc/systemd/system/systemd-nspawn@kube1.service.d/override.conf": b"[Unit]\n",
                "etc/systemd/system/unbounded-agent-regenerate-config@kube1.service": b"[Unit]\n",
                "etc/systemd/system/sshd.service": b"[Unit]\n",
                "etc/systemd/nspawn/kube1.nspawn": b"[Exec]\n",
                "etc/sysctl.d/99-kubernetes.conf": b"a=1\n",
                "etc/sysctl.d/10-other.conf": b"b=1\n",
                "etc/sysconfig/aks-flex-node-container": b"A=1\n",
            }
            for name, data in files.items():
                (root / name).parent.mkdir(parents=True, exist_ok=True)
                (root / name).write_bytes(data)
            wants = root / "etc/systemd/system/multi-user.target.wants"
            wants.mkdir()
            (wants / "aks-flex-node-agent.service").symlink_to("/etc/systemd/system/aks-flex-node-agent.service")
            (root / "var/lib/machines/kube1").mkdir(parents=True)
            stubs = (
                "id() { echo 0; }\n"
                "ctr() { printf 'REF TYPE DIGEST SIZE PLATFORMS LABELS\\nimg:1 application/x %s 1 MiB linux/amd64 -\\n' "
                f"{DIGEST}; }}\n"
                "machinectl() { if [ \"$2\" = kube1 ]; then echo running; else return 1; fi; }\n"
            )
            script = (flexinstall.STATE_MANIFEST_SCRIPT.replace("/etc/", f"{root}/etc/")
                      .replace("/var/lib/machines", f"{root}/var/lib/machines"))
            result = subprocess.run(["bash", "-c", stubs + script], capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertNotIn("secret", result.stdout)
            manifest = {key.removeprefix(str(root)): value
                        for key, value in flexinstall.parse_state_manifest(result.stdout).items()}
            excluded = {"/etc/systemd/system/sshd.service", "/etc/sysctl.d/10-other.conf"}
            for name, data in files.items():
                path = f"/{name}"
                with self.subTest(path=path):
                    if path in excluded:
                        self.assertNotIn(path, manifest)
                    else:
                        self.assertEqual(manifest.get(path), hashlib.sha256(data).hexdigest())
            link = "/etc/systemd/system/multi-user.target.wants/aks-flex-node-agent.service"
            self.assertEqual(manifest[link],
                             hashlib.sha256(b"link /etc/systemd/system/aks-flex-node-agent.service").hexdigest())
            self.assertEqual(manifest["image img:1"], DIGEST)
            self.assertEqual((manifest["active"], manifest["machine kube1"]), ("kube1", "running"))
            self.assertNotIn("machine kube2", manifest)
            paths = [line.split("  ", 1)[1] for line in result.stdout.splitlines() if "  /" in line]
            self.assertEqual(paths, sorted(paths))


class TridentKubeconfigTest(unittest.TestCase):
    CERT = "-----BEGIN CERTIFICATE-----\nMIIBclientcert\n-----END CERTIFICATE-----\n"
    KEY = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIkeymaterial\n-----END EC PRIVATE KEY-----\n"

    def script(self, **overrides: str) -> str:
        return flexinstall.trident_kubeconfig_script(server=overrides.get("server", "192.168.113.2:6443"),
                                                     ca_pem=CA_PEM, **{k: v for k, v in overrides.items() if k != "server"})

    @unittest.skipUnless(HAS_BASH, "bash isn't installed")
    def test_valid_bash(self) -> None:
        result = bash_syntax(self.script())
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_contents(self) -> None:
        script = self.script()
        self.assertNotIn("PRIVATE KEY-----\n", script)
        self.assertNotRegex(script, r"echo[^\n]*\$key")
        self.assertIn("install -D -m 0600 -o root -g root /dev/stdin \"$kubeconfig\"", script)
        self.assertIn("kubeconfig=/etc/aks-flex-node/trident-acl-agent.kubeconfig", script)
        self.assertIn("unit=trident-acl-agent.service", script)
        self.assertIn('sudo systemctl restart "$unit"', script)
        self.assertIn('sudo restorecon -F "$kubeconfig"', script)
        self.assertIn("server=https://192.168.113.2:6443", script)
        self.assertIn(f"ca={base64.b64encode(CA_PEM.encode()).decode()}", script)
        with self.assertRaises(StepError):
            flexinstall.trident_kubeconfig_script(server="x", ca_pem="nope")

    @unittest.skipUnless(HAS_BASH and shutil.which("jq") and shutil.which("base64"), "bash, jq, or base64 is missing")
    def test_runs(self) -> None:
        """Run the script against a fake machine root, with sudo, install, restorecon, and systemctl stubbed."""
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pki = root / "machines/kube1/var/lib/kubelet/pki"
            pki.mkdir(parents=True)
            (pki / "kubelet-client-2026-11-12.pem").write_text(self.CERT + self.KEY)
            (pki / "kubelet-client-current.pem").symlink_to("/var/lib/kubelet/pki/kubelet-client-2026-11-12.pem")
            state = root / "daemon-state.json"
            log = root / "log"
            kubeconfig = root / "out/trident.kubeconfig"
            stubs = (
                f"export LOG={shlex.quote(str(log))}\n"
                "sudo() { \"$@\"; }\n"
                "install() { local dest=\"${@: -1}\"; mkdir -p \"$(dirname \"$dest\")\"; cat >\"$dest\"; "
                "chmod 600 \"$dest\"; echo \"install $*\" >>\"$LOG\"; }\n"
                "restorecon() { echo \"restorecon $*\" >>\"$LOG\"; }\n"
                "systemctl() { echo \"systemctl $*\" >>\"$LOG\"; }\n"
            )

            def run_script() -> subprocess.CompletedProcess[str]:
                script = (self.script(path=str(kubeconfig))
                          .replace(flexinstall.DAEMON_STATE, str(state))
                          .replace("/var/lib/machines", str(root / "machines")))
                return subprocess.run(["bash", "-c", stubs + script], capture_output=True, text=True, check=False)

            result = run_script()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("no active machine", result.stderr)

            state.write_text('{"activeMachine": "kube2"}')
            result = run_script()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("no kubelet client certificate", result.stderr)

            state.write_text('{"activeMachine": "kube1"}')
            result = run_script()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertNotIn("keymaterial", result.stdout + result.stderr)
            text = kubeconfig.read_text()
            self.assertEqual(stat.S_IMODE(os.stat(kubeconfig).st_mode), 0o600)
            fields = dict(line.strip().split(": ", 1) for line in text.splitlines() if ": " in line)
            self.assertEqual(fields["server"], "https://192.168.113.2:6443")
            self.assertEqual(base64.b64decode(fields["certificate-authority-data"]).decode(), CA_PEM)
            self.assertEqual(base64.b64decode(fields["client-certificate-data"]).decode(), self.CERT)
            self.assertEqual(base64.b64decode(fields["client-key-data"]).decode(), self.KEY)
            self.assertEqual(fields["current-context"], "flex")
            calls = log.read_text().splitlines()
            self.assertIn("-m 0600", calls[0])
            self.assertEqual(calls[1:], [f"restorecon -F {kubeconfig}", "systemctl restart trident-acl-agent.service"])


class SelinuxTest(unittest.TestCase):
    def test_denials(self) -> None:
        lines = "\n".join([
            'audit: type=1400 avc:  denied  { send_msg } comm="aks-flex-node" scontext=spc_t tclass=dbus',
            'audit: type=1400 avc:  denied  { read } comm="sshd" path="/etc/shadow"',
            'audit: type=1107 avc:  denied  { stop } function="bus_unit_method_reset_failed" comm="aks-flex-node"',
            'audit: type=1400 avc:  granted  { read } comm="aks-flex-node"',
        ])
        ssh = FakeSsh([("journalctl", completed(lines))])
        denials = flexinstall.avc_denials(ssh)
        self.assertEqual(len(denials), 2)
        self.assertEqual(flexinstall.denial_issue(denials[0]), "")
        self.assertEqual(flexinstall.denial_issue(denials[1]), "https://github.com/Azure/AKSFlexNode/issues/385")
        self.assertEqual(len(flexinstall.avc_denials(ssh, patterns=())), 3)
        self.assertEqual(flexinstall.avc_denials(ssh, patterns=("sshd",)), [lines.splitlines()[1]])

    @unittest.skipUnless(HAS_BASH, "bash isn't installed")
    def test_scripts_parse(self) -> None:
        ssh = FakeSsh([])
        flexinstall.avc_denials(ssh)
        for script in (ssh.calls[0][0], flexinstall._BOOTSTRAP_PROBE, flexinstall._AGENT_PROBE,
                       flexinstall._agent_ready_probe("1700000000")):
            result = bash_syntax(script)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_redact(self) -> None:
        text = f"token {TOKEN} and\n-----BEGIN EC PRIVATE KEY-----\nabc\n-----END EC PRIVATE KEY-----\nend"
        redacted = flexinstall.redact(text)
        self.assertNotIn(TOKEN, redacted)
        self.assertNotIn("abc", redacted)
        self.assertTrue(redacted.endswith("end"))


if __name__ == "__main__":
    unittest.main()
