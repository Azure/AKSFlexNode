"""Unit tests for omaha.py, which run without a VM; the TLS round trip needs the openssl CLI and
only uses loopback:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_omaha.py'
"""

from __future__ import annotations

import hashlib
import http.client
import json
import os
import shutil
import socket
import ssl
import stat
import subprocess
import sys
import tempfile
import time
import unittest
import unittest.mock
import urllib.parse
import xml.etree.ElementTree as ET
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import omaha  # noqa: E402
from errors import StepError  # noqa: E402

# The digest trident's own tests use: echo -n "trident-cosi-metadata-round-trip-test" | openssl dgst -sha384
DIGEST = "f1ed407a796610fce7ee7a41bf9bb16d48b638ed9fac2893e76b0381bc3f44cd3bed0833891ca1ef0088cb661c1ef471"
NODE_UPDATE_ID = "11111111-1111-1111-1111-111111111111"
STAGE_ID = "aaaaaaaa-1111-1111-1111-111111111111"
FINALIZE_ID = "bbbbbbbb-2222-2222-2222-222222222222"
PREFIX = "acl.azure.com"
APP_ID = "e96281a6-d1af-4bde-9a0a-97b76e56dc57"

# Requests as the agent's quick-xml writer emits them (core/nebraska/wire.rs): byte for byte what
# wire.rs's Request serializes for these apps, but for the random sessionid.
HEAD = ('<?xml version="1.0" encoding="UTF-8"?>'
        '<request protocol="3.0" ismachine="1" sessionid="3f2c1a8e-5b0d-4c1e-9a77-0c7e2d4b9f10">')


def omaha_request(version: str, children: str = "", *, app_id: str = APP_ID, track: str = "pin-20261106.1.0") -> bytes:
    app = f'<app appid="{app_id}" version="{version}" track="{track}" machineid="0f6b2c3d4e5f60718293a4b5c6d7e8f9"'
    app += f">{children}</app>" if children else "/>"
    return f'{HEAD}<os platform="linux" version="{version}" arch="amd64"/>{app}</request>'.encode()


UPDATE_CHECK = omaha_request("0.0.0", "<updatecheck/>")
DOWNLOAD_STARTED = omaha_request("0.0.0", '<event eventtype="13" eventresult="1"/>')
COMPLETION = omaha_request("20261106.1.0",
                           '<event eventtype="3" eventresult="2" previousversion="0.0.0"/><ping active="1"/><updatecheck/>')
FAILURE = omaha_request("0.0.0", '<event eventtype="3" eventresult="0" previousversion="0.0.0"/>')
PROBE = omaha_request("0.0.0")
CODEBASE = "https://10.0.2.2:8297/images/"


def package(path: Path = Path("/nonexistent/20261106.1.cosi"), size: int = 4096) -> omaha.Package:
    return omaha.Package("20261106.1", "20261106.1.0", path, size, DIGEST)


def respond(body: bytes, offered: omaha.Package | None) -> ET.Element:
    return ET.fromstring(omaha.omaha_response(omaha.parse_omaha_request(body), offered, CODEBASE))


def run(cmd: list[str], *, input: str | None = None, timeout: float | None = None,
        stream: bool = False) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(cmd, input=input, text=True, capture_output=not stream, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        return subprocess.CompletedProcess(cmd, 127, "", str(error))


class WireTests(unittest.TestCase):
    def test_digest_is_sha384(self):
        self.assertEqual(hashlib.sha384(b"trident-cosi-metadata-round-trip-test").hexdigest(), DIGEST)

    def test_update_check_is_offered_the_package(self):
        response = respond(UPDATE_CHECK, package())
        self.assertEqual(response.tag, "response")
        self.assertEqual(response.get("protocol"), "3.0")
        apps = response.findall("app")
        self.assertEqual(len(apps), 1)
        self.assertEqual(apps[0].get("appid"), APP_ID)
        self.assertEqual(apps[0].get("status"), "ok")
        self.assertEqual(apps[0].findall("event"), [])
        check = apps[0].find("updatecheck")
        self.assertEqual(check.get("status"), "ok")
        codebase = check.find("urls/url").get("codebase")
        self.assertTrue(codebase.startswith("https://") and codebase.endswith("/"), codebase)
        self.assertEqual(check.find("manifest").get("version"), "20261106.1.0")
        packages = check.findall("manifest/packages/package")
        self.assertEqual(len(packages), 1)
        self.assertEqual(packages[0].get("name"), "20261106.1.cosi")
        self.assertEqual(packages[0].get("size"), "4096")
        self.assertEqual(packages[0].get("required"), "true")
        self.assertIsNone(packages[0].get("hash"), "the legacy hash would mask a broken label")
        labels = packages[0].findall("labels/label")
        self.assertEqual([(label.get("key"), label.get("value")) for label in labels], [("cosi.metadata.sha384", DIGEST)])
        self.assertRegex(labels[0].get("value"), r"^[0-9a-f]{96}$")
        # The agent resolves the name against the codebase (client.rs join_file).
        self.assertEqual(urllib.parse.urljoin(codebase, packages[0].get("name")),
                         "https://10.0.2.2:8297/images/20261106.1.cosi")

    def test_response_starts_with_the_declaration(self):
        body = omaha.omaha_response(omaha.parse_omaha_request(UPDATE_CHECK), package(), CODEBASE)
        self.assertTrue(body.startswith(b'<?xml version="1.0" encoding="UTF-8"?>'))

    def test_label_is_lowercase_hex(self):
        upper = omaha.Package("20261106.1", "20261106.1.0", Path("/x"), 1, DIGEST.upper())
        label = respond(UPDATE_CHECK, upper).find("app/updatecheck/manifest/packages/package/labels/label")
        self.assertEqual(label.get("value"), DIGEST)

    def test_progress_event_is_acknowledged(self):
        app = respond(DOWNLOAD_STARTED, package()).find("app")
        self.assertEqual(app.get("status"), "ok")
        self.assertEqual([event.get("status") for event in app.findall("event")], ["ok"])
        self.assertIsNone(app.find("updatecheck"))
        self.assertIsNone(app.find("ping"))

    def test_completion_batch_gets_noupdate(self):
        app = respond(COMPLETION, package()).find("app")
        self.assertEqual(app.get("status"), "ok")
        self.assertEqual([event.get("status") for event in app.findall("event")], ["ok"])
        self.assertEqual(app.find("ping").get("status"), "ok")
        self.assertEqual(app.find("updatecheck").get("status"), "noupdate")
        self.assertIsNone(app.find("updatecheck/manifest"))

    def test_terminal_event_gets_noupdate_whatever_the_version(self):
        body = omaha_request("0.0.0", '<event eventtype="3" eventresult="2" previousversion="0.0.0"/><updatecheck/>')
        self.assertEqual(respond(body, package()).find("app/updatecheck").get("status"), "noupdate")

    def test_noupdate_when_nothing_is_offered(self):
        app = respond(UPDATE_CHECK, None).find("app")
        self.assertEqual(app.get("status"), "ok")
        self.assertEqual(app.find("updatecheck").get("status"), "noupdate")

    def test_noupdate_at_the_offered_version(self):
        body = omaha_request("20261106.1.0", "<updatecheck/>")
        self.assertEqual(respond(body, package()).find("app/updatecheck").get("status"), "noupdate")

    def test_offered_again_after_a_failure(self):
        # A failed stage reports 3/0 alone; the next stage's check must get the package again.
        self.assertEqual(respond(FAILURE, package()).find("app/updatecheck"), None)
        self.assertEqual(respond(UPDATE_CHECK, package()).find("app/updatecheck").get("status"), "ok")

    def test_probe_gets_a_bare_ok_app(self):
        app = respond(PROBE, package()).find("app")
        self.assertEqual(app.get("status"), "ok")
        self.assertEqual(list(app), [])

    def test_one_acknowledgement_per_event(self):
        body = omaha_request("0.0.0", '<event eventtype="13" eventresult="1"/><event eventtype="14" eventresult="1"/>')
        self.assertEqual(len(respond(body, None).findall("app/event")), 2)

    def test_offered_per_app_and_track(self):
        request = omaha.parse_omaha_request(UPDATE_CHECK)
        cases = {
            "any app and track": ({}, "ok"),
            "its app and track": ({"app_id": APP_ID.upper(), "track": "pin-20261106.1.0"}, "ok"),
            "another app": ({"app_id": "00000000-0000-0000-0000-000000000000"}, "noupdate"),
            "another track": ({"track": "pin-20261105.1.0"}, "noupdate"),
        }
        for name, (scope, want) in cases.items():
            with self.subTest(name):
                response = ET.fromstring(omaha.omaha_response(request, package(), CODEBASE, **scope))
                self.assertEqual(response.find("app/updatecheck").get("status"), want)
                offered, reason = omaha.update_decision(request.apps[0], package(), **scope)
                self.assertEqual(offered is None, want == "noupdate", reason)

    def test_parse_request(self):
        request = omaha.parse_omaha_request(COMPLETION)
        self.assertEqual(request.protocol, "3.0")
        self.assertEqual(request.os_arch, "amd64")
        (app,) = request.apps
        self.assertEqual((app.app_id, app.version, app.track), (APP_ID, "20261106.1.0", "pin-20261106.1.0"))
        self.assertEqual(app.events, (omaha.OmahaEvent("3", "2", "0.0.0"),))
        self.assertTrue(app.ping and app.updatecheck)
        self.assertEqual(app.kind, "updatecheck")
        self.assertEqual(app.elements, ["event 3/2", "ping", "updatecheck"])
        self.assertEqual(omaha.parse_omaha_request(DOWNLOAD_STARTED).apps[0].kind, "event")
        self.assertEqual(omaha.parse_omaha_request(PROBE).apps[0].kind, "probe")
        self.assertEqual(omaha.request_notes(request), [])

    def test_malformed_requests(self):
        for body in (b"not xml", b"<response/>", b'<!DOCTYPE r [<!ENTITY a "b">]><request/>'):
            with self.subTest(body=body), self.assertRaises(ValueError):
                omaha.parse_omaha_request(body)

    def test_notes_on_what_nebraska_drops(self):
        body = omaha_request("0.0.0", '<event eventtype="3" eventresult="2"/><event eventtype="7" eventresult="1"/>')
        notes = omaha.request_notes(omaha.parse_omaha_request(body))
        self.assertTrue(any("without previousversion" in note for note in notes), notes)
        self.assertTrue(any("7/1" in note for note in notes), notes)
        braced = omaha.parse_omaha_request(omaha_request("0.0.0", "<updatecheck/>").replace(
            b'machineid="0f6b2c3d4e5f60718293a4b5c6d7e8f9"', b'machineid="{12345678-1234-4234-8234-1234567890ab}"'))
        self.assertTrue(any("braced" in note for note in omaha.request_notes(braced)))

    def test_cookie_state(self):
        cookie = "ASLBSA=acl-abupdate-0123"
        self.assertEqual(omaha.cookie_state(None, cookie), "missing")
        self.assertEqual(omaha.cookie_state(" ", cookie), "missing")
        self.assertEqual(omaha.cookie_state(cookie, cookie), "correct")
        self.assertEqual(omaha.cookie_state("ASLBSA=stale", cookie), "wrong")


class RangeTests(unittest.TestCase):
    def test_ranges(self):
        cases = {
            None: None,
            "bytes=0-99": (0, 99),
            "bytes=100-": (100, 999),
            "bytes=-50": (950, 999),
            "bytes=-5000": (0, 999),
            "bytes=990-2000": (990, 999),
            "bytes=999-999": (999, 999),
            "Bytes= 0-0": (0, 0),
        }
        for header, expected in cases.items():
            with self.subTest(header=header):
                self.assertEqual(omaha.parse_range(header, 1000), expected)

    def test_invalid_ranges(self):
        for header in ("bytes=1000-", "bytes=1000-1001", "bytes=50-10", "bytes=-0", "bytes=-", "bytes=0-1,5-6",
                       "items=0-1", "bytes=a-b", "", "bytes=0x10-"):
            with self.subTest(header=header), self.assertRaises(omaha.RangeError):
                omaha.parse_range(header, 1000)
        with self.assertRaises(omaha.RangeError):
            omaha.parse_range("bytes=-1", 0)


class VersionTests(unittest.TestCase):
    def test_tag_to_semver(self):
        self.assertEqual(omaha.tag_to_semver("20261106.1"), "20261106.1.0")
        self.assertEqual(omaha.tag_to_semver("20261106.12.3"), "20261106.12.3")
        self.assertEqual(omaha.tag_to_semver("7"), "7.0.0")
        for tag in ("20261106.01", "latest", "20261106.1-hotfix", "1.2.3.4", "", "20261106..1", "２０２６.1"):
            with self.subTest(tag=tag), self.assertRaises(StepError):
                omaha.tag_to_semver(tag)

    def test_semver(self):
        for version in ("0.0.0", "202606.29.0", "1.2.3-rc.1+build.5"):
            self.assertTrue(omaha.is_semver(version), version)
        for version in ("20261106.1", "01.2.3", "1.2.3-", "v1.2.3"):
            self.assertFalse(omaha.is_semver(version), version)


class PackageTests(unittest.TestCase):
    def test_for_cosi_and_check(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "update.cosi"
            path.write_bytes(b"x" * 10)
            offered = omaha.Package.for_cosi("20261106.1", path, DIGEST.upper())
            self.assertEqual((offered.version, offered.size, offered.metadata_sha384), ("20261106.1.0", 10, DIGEST))
            self.assertEqual(offered.file_name, "20261106.1.cosi")
            omaha.check_package(offered)
            for broken in (omaha.Package("a/b", "1.0.0", path, 10, DIGEST), omaha.Package("x", "1.0", path, 10, DIGEST),
                           omaha.Package("x", "1.0.0", path, 10, DIGEST[:-1]), omaha.Package("x", "1.0.0", path, 11, DIGEST),
                           omaha.Package("x", "1.0.0", Path(directory) / "missing", 10, DIGEST)):
                with self.subTest(package=broken), self.assertRaises(StepError):
                    omaha.check_package(broken)

    def test_offer_checks_before_serving(self):
        server = omaha.OmahaServer(tls=omaha.TLSFiles.at(Path("/nonexistent")), log=lambda message: None)
        with self.assertRaises(StepError):
            server.offer(package())
        self.assertIsNone(server.offered)

    def test_urls(self):
        server = omaha.OmahaServer(tls=omaha.TLSFiles.at(Path("/nonexistent")), log=lambda message: None)
        self.assertEqual(server.update_url, "https://10.0.2.2:8297/v1/update/")
        self.assertEqual(server.image_url(package()), "https://10.0.2.2:8297/images/20261106.1.cosi")
        ipv6 = omaha.OmahaServer(tls=omaha.TLSFiles.at(Path("/x")), public_host="fd00::1", port=1, log=print)
        self.assertEqual(ipv6.update_url, "https://[fd00::1]:1/v1/update/")
        unstarted = omaha.OmahaServer(tls=omaha.TLSFiles.at(Path("/x")), port=0, log=print)
        with self.assertRaises(StepError):
            _ = unstarted.update_url
        with self.assertRaises(StepError):
            omaha.OmahaServer(tls=omaha.TLSFiles.at(Path("/x")), log=print, affinity_cookie="no cookie")


class SanTests(unittest.TestCase):
    def test_normalize(self):
        self.assertEqual(omaha.normalize_sans(["IP:10.0.2.2", "ip:192.168.113.1", "DNS:LocalHost", "127.0.0.1", "::1",
                                               "example.test", "IP:10.0.2.2"]),
                         ["IP:10.0.2.2", "IP:192.168.113.1", "DNS:localhost", "IP:127.0.0.1", "IP:::1", "DNS:example.test"])
        for sans in ([], ["IP:not-an-ip"], ["DNS:bad_name"], ["URI:https://x"]):
            with self.subTest(sans=sans), self.assertRaises(StepError):
                omaha.normalize_sans(sans)

    def test_parse_openssl_output(self):
        text = "X509v3 Subject Alternative Name: \n    IP Address:10.0.2.2, IP Address:0:0:0:0:0:0:0:1, DNS:localhost\n"
        self.assertEqual(omaha.parse_openssl_sans(text), {"IP:10.0.2.2", "IP:::1", "DNS:localhost"})


def node(annotations: dict[str, str] | None) -> dict:
    return {"apiVersion": "v1", "kind": "Node", "metadata": {"name": "aks-flex-0", "annotations": annotations}}


def status_json(operation_id: str, code: str, operation: str = "stage") -> str:
    status = {"schemaVersion": "1.0", "nodeUpdateId": NODE_UPDATE_ID, "operationId": operation_id,
              "operation": operation, "code": code, "message": f"{operation} {code}", "fromVersion": "0.0.0",
              "toVersion": "20261106.1.0", "tridentVersion": "0.29.0", "startedUtc": "2026-11-12T12:00:00Z",
              "lastUpdatedUtc": "2026-11-12T12:01:00Z"}
    if code != "InProgress":
        status["finishedUtc"] = "2026-11-12T12:01:00Z"
    return json.dumps(status)


class AnnotationTests(unittest.TestCase):
    def stage(self, **overrides) -> dict:
        arguments = {"operation": "stage", "target_version": "20261106.1.0", "server": "https://10.0.2.2:8297/v1/update/",
                     "app_id": APP_ID, "track": "pin-20261106.1.0", "node_update_id": NODE_UPDATE_ID,
                     "operation_id": STAGE_ID}
        arguments.update(overrides)
        return omaha.update_request(**arguments)

    def test_update_request(self):
        self.assertEqual(self.stage(), {
            "schemaVersion": "1.0", "nodeUpdateId": NODE_UPDATE_ID, "operationId": STAGE_ID, "operation": "stage",
            "targetVersion": "20261106.1.0", "server": "https://10.0.2.2:8297/v1/update/", "appId": APP_ID,
            "track": "pin-20261106.1.0"})
        rollback = omaha.update_request(operation="rollback", node_update_id=NODE_UPDATE_ID, operation_id=FINALIZE_ID)
        self.assertEqual(rollback, {"schemaVersion": "1.0", "nodeUpdateId": NODE_UPDATE_ID, "operationId": FINALIZE_ID,
                                    "operation": "rollback"})

    def test_invalid_update_requests(self):
        for overrides in ({"operation": "commit"}, {"operation_id": "op-1"}, {"node_update_id": ""},
                          {"server": "http://10.0.2.2:8297/v1/update/"}, {"server": "https:///v1/update"},
                          {"target_version": "20261106.1"}, {"target_version": ""}, {"app_id": None}, {"track": ""},
                          {"operation": "rollback"}):
            with self.subTest(overrides=overrides), self.assertRaises(StepError):
                self.stage(**overrides)

    def test_request_patch(self):
        patch = omaha.request_patch(PREFIX, self.stage())
        annotations = patch["metadata"]["annotations"]
        self.assertEqual(list(annotations), ["acl.azure.com/update-request"])
        self.assertEqual(json.loads(annotations["acl.azure.com/update-request"]), self.stage())
        with self.assertRaises(StepError):
            omaha.request_patch(PREFIX, {**self.stage(), "extra": "x"})
        with self.assertRaises(StepError):
            omaha.request_patch("Not A Prefix", self.stage())

    def test_clear_patch(self):
        self.assertEqual(omaha.clear_patch(PREFIX, ["update-status", "example.com/other"]),
                         {"metadata": {"annotations": {"acl.azure.com/update-status": None, "example.com/other": None}}})
        self.assertEqual(set(omaha.clear_patch(PREFIX)["metadata"]["annotations"]),
                         {"acl.azure.com/update-request", "acl.azure.com/update-status", "acl.azure.com/update-commit-status"})

    def test_parse_status(self):
        done = node({"acl.azure.com/update-status": status_json(STAGE_ID, "Success")})
        status = omaha.parse_status(done, PREFIX)
        self.assertEqual((status.operation_id, status.code, status.terminal, status.operation),
                         (STAGE_ID, "Success", True, "stage"))
        self.assertEqual(status.message, "stage Success")
        self.assertEqual(status.raw["toVersion"], "20261106.1.0")
        running = omaha.parse_status(node({"acl.azure.com/update-status": status_json(STAGE_ID, "InProgress")}), PREFIX)
        self.assertFalse(running.terminal)

    def test_parse_status_for_another_operation(self):
        stale = node({"acl.azure.com/update-status": status_json(STAGE_ID, "Success")})
        self.assertIsNone(omaha.parse_status(stale, PREFIX, operation_id=FINALIZE_ID))
        self.assertEqual(omaha.parse_status(stale, PREFIX, operation_id=STAGE_ID).code, "Success")
        self.assertNotEqual(omaha.parse_status(stale, PREFIX).operation_id, FINALIZE_ID)

    def test_parse_commit_status(self):
        commit = node({"acl.azure.com/update-commit-status": status_json(FINALIZE_ID, "TargetBootFailed", "commit")})
        self.assertIsNone(omaha.parse_status(commit, PREFIX))
        status = omaha.parse_status(commit, PREFIX, omaha.COMMIT_STATUS_KEY, operation_id=FINALIZE_ID)
        self.assertEqual((status.operation, status.code, status.terminal), ("commit", "TargetBootFailed", True))

    def test_parse_status_absent_and_malformed(self):
        self.assertIsNone(omaha.parse_status(node(None), PREFIX))
        self.assertIsNone(omaha.parse_status({}, PREFIX))
        self.assertIsNone(omaha.parse_status(node({"acl.microsoft.com/update-status": status_json(STAGE_ID, "Success")}), PREFIX))
        for text in ("{", "[]", json.dumps({"code": "Success"}), status_json(STAGE_ID, "Done")):
            with self.subTest(text=text), self.assertRaises(StepError):
                omaha.parse_status(node({"acl.azure.com/update-status": text}), PREFIX)

    def test_terminal_codes(self):
        self.assertEqual(omaha.TERMINAL_CODES, {"Success", "AlreadyAtTarget", "NotStaged", "OperationFailed",
                                                "TargetBootFailed", "AgentInternalError", "InvalidRequest"})
        self.assertNotIn("InProgress", omaha.TERMINAL_CODES)


class ThrottleTests(unittest.TestCase):
    def test_pace_spreads_chunks_over_time_across_connections(self):
        server = omaha.OmahaServer(port=0, tls=omaha.TLSFiles.at(Path("/nonexistent")), log=lambda _: None)
        clock = [100.0]
        sleeps: list[float] = []

        def sleep(seconds: float) -> None:
            sleeps.append(round(seconds, 6))
            clock[0] += seconds

        with unittest.mock.patch.object(omaha.time, "monotonic", lambda: clock[0]), \
                unittest.mock.patch.object(omaha.time, "sleep", sleep):
            server._pace(1000)
            self.assertEqual(sleeps, [], "without a throttle nothing waits")
            server.throttle(1000)
            for _ in range(3):
                server._pace(500)
            self.assertEqual(sleeps, [0.5, 0.5, 0.5])
            # A second connection's chunk queues behind the first's, so the total rate holds.
            sleeps.clear()
            server._pace(1000)
            self.assertEqual(sleeps, [1.0])
            server.throttle(0)
            sleeps.clear()
            server._pace(10**9)
            self.assertEqual(sleeps, [])
        with self.assertRaises(StepError):
            server.throttle(-1)


@unittest.skipIf(shutil.which("openssl") is None, "the openssl CLI isn't installed")
class LoopbackTests(unittest.TestCase):
    """make_tls with the real openssl CLI, and a round trip with the server on an ephemeral
    loopback port."""

    SANS = ["IP:10.0.2.2", "IP:192.168.113.1", "IP:127.0.0.1", "DNS:localhost"]

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.logs: list[str] = []
        self.tls = omaha.make_tls(self.root / "tls", self.SANS, run=run, log=self.logs.append)

    def tearDown(self):
        self.temporary.cleanup()

    def client_context(self) -> ssl.SSLContext:
        return ssl.create_default_context(cafile=str(self.tls.ca_cert))

    def test_tls_files(self):
        self.assertEqual(stat.S_IMODE(self.tls.directory.stat().st_mode), 0o700)
        for key in (self.tls.server_key, self.tls.ca_key):
            self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
        extensions = run(["openssl", "x509", "-in", str(self.tls.ca_cert), "-noout", "-ext", "basicConstraints,keyUsage"])
        self.assertIn("CA:TRUE", extensions.stdout)
        self.assertIn("Certificate Sign", extensions.stdout)
        server = run(["openssl", "x509", "-in", str(self.tls.server_cert), "-noout", "-ext",
                      "subjectAltName,extendedKeyUsage"])
        self.assertIn("TLS Web Server Authentication", server.stdout)
        self.assertEqual(omaha.parse_openssl_sans(server.stdout), set(self.SANS))

    def test_make_tls_is_idempotent(self):
        before = {path: path.read_bytes() for path in (self.tls.ca_cert, self.tls.server_cert, self.tls.server_key)}
        self.logs.clear()
        again = omaha.make_tls(self.root / "tls", list(reversed(self.SANS)), run=run, log=self.logs.append)
        self.assertEqual(again, self.tls)
        self.assertEqual({path: path.read_bytes() for path in before}, before)
        self.assertEqual(self.logs, [])

    def test_new_sans_keep_the_ca(self):
        ca = self.tls.ca_cert.read_bytes()
        server = self.tls.server_cert.read_bytes()
        omaha.make_tls(self.root / "tls", ["IP:10.0.2.2"], run=run)
        self.assertEqual(self.tls.ca_cert.read_bytes(), ca)
        self.assertNotEqual(self.tls.server_cert.read_bytes(), server)
        verify = run(["openssl", "verify", "-CAfile", str(self.tls.ca_cert), str(self.tls.server_cert)])
        self.assertEqual(verify.returncode, 0, verify.stderr)

    def test_broken_ca_is_replaced(self):
        ca = self.tls.ca_cert.read_bytes()
        self.tls.ca_key.write_text("not a key")
        omaha.make_tls(self.root / "tls", self.SANS, run=run, log=self.logs.append)
        self.assertNotEqual(self.tls.ca_cert.read_bytes(), ca)
        self.assertTrue(any("has to trust it" in message for message in self.logs), self.logs)

    def wait_for(self, probe, timeout: float = 5) -> bool:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if probe():
                return True
            time.sleep(0.05)
        return False

    def test_round_trip(self):
        data = os.urandom(1000)
        image = self.root / "20261106.1.cosi"
        image.write_bytes(data)
        offered = omaha.Package.for_cosi("20261106.1", image, DIGEST)
        server = omaha.OmahaServer(port=0, tls=self.tls, public_host="127.0.0.1", log=self.logs.append)
        with server:
            server.offer(offered)
            self.assertTrue(server.update_url.startswith(f"https://127.0.0.1:{server.port}/"))
            connection = http.client.HTTPSConnection("127.0.0.1", server.port, context=self.client_context(), timeout=10)

            # An update check without a cookie gets the package and the affinity cookie.
            connection.request("POST", omaha.UPDATE_PATH, body=UPDATE_CHECK, headers={"Content-Type": "application/xml"})
            response = connection.getresponse()
            body = response.read()
            self.assertEqual(response.status, 200)
            self.assertEqual(connection.sock.version(), "TLSv1.2")
            cookie = response.getheader("Set-Cookie")
            self.assertEqual(cookie.split(";")[0], server.affinity_cookie)
            check = ET.fromstring(body).find("app/updatecheck")
            self.assertEqual(check.get("status"), "ok")
            url = urllib.parse.urljoin(check.find("urls/url").get("codebase"),
                                       check.find("manifest/packages/package").get("name"))
            self.assertEqual(url, server.image_url(offered))
            self.assertEqual(server.requests[-1]["cookie_state"], "missing")
            self.assertTrue(server.requests[-1]["set_cookie"])

            # The agent echoes the cookie on its events, on the same keep-alive connection.
            connection.request("POST", omaha.UPDATE_PATH, body=DOWNLOAD_STARTED, headers={"Cookie": server.affinity_cookie})
            response = connection.getresponse()
            self.assertEqual([event.get("status") for event in ET.fromstring(response.read()).findall("app/event")], ["ok"])
            self.assertIsNone(response.getheader("Set-Cookie"))
            self.assertEqual(server.requests[-1]["cookie_state"], "correct")
            connection.request("POST", omaha.UPDATE_PATH, body=FAILURE, headers={"Cookie": "ASLBSA=stale"})
            response = connection.getresponse()
            response.read()
            self.assertIsNone(response.getheader("Set-Cookie"))
            self.assertEqual(server.requests[-1]["cookie_state"], "wrong")

            event = server.wait_for_event("13", "1", timeout=5)
            self.assertEqual({key: event[key] for key in ("eventtype", "eventresult", "version", "previousversion")},
                             {"eventtype": "13", "eventresult": "1", "version": "0.0.0", "previousversion": ""})
            self.assertEqual(server.wait_for_event("3", timeout=5)["previousversion"], "0.0.0")
            with self.assertRaises(StepError):
                server.wait_for_event("13", "1", timeout=0.1, start=len(server.events))
            self.assertEqual([request["kind"] for request in server.requests], ["updatecheck", "event", "event"])

            # tridentd's HEAD, then its ranges.
            path = urllib.parse.urlsplit(url).path
            connection.request("HEAD", path)
            response = connection.getresponse()
            response.read()
            self.assertEqual((response.status, response.getheader("Content-Length"), response.getheader("Accept-Ranges")),
                             (200, "1000", "bytes"))
            for header, status, expected, content_range in (
                    ("bytes=0-99", 206, data[:100], "bytes 0-99/1000"),
                    ("bytes=900-", 206, data[900:], "bytes 900-999/1000"),
                    ("bytes=-50", 206, data[-50:], "bytes 950-999/1000"),
                    (None, 200, data, None),
                    ("bytes=2000-", 416, b"", "bytes */1000"),
                    ("bytes=0-1,4-5", 416, b"", "bytes */1000")):
                with self.subTest(range=header):
                    connection.request("GET", path, headers={"Range": header} if header else {})
                    response = connection.getresponse()
                    self.assertEqual(response.read(), expected)
                    self.assertEqual(response.status, status)
                    self.assertEqual(response.getheader("Content-Range"), content_range)
                    self.assertEqual(response.getheader("Content-Length"), str(len(expected)))
            served = [request for request in server.requests if request["kind"] == "image" and request["method"] == "GET"]
            self.assertEqual([request["bytes"] for request in served], [100, 100, 50, 1000, 0, 0])

            connection.request("GET", "/images/other.cosi")
            response = connection.getresponse()
            response.read()
            self.assertEqual(response.status, 404)
            connection.close()

            # TLS 1.3 isn't offered, and a client that doesn't trust the CA is refused and logged.
            strict = self.client_context()
            strict.minimum_version = ssl.TLSVersion.TLSv1_3
            with self.assertRaises(ssl.SSLError), socket.create_connection(("127.0.0.1", server.port), timeout=5) as raw:
                strict.wrap_socket(raw, server_hostname="127.0.0.1")
            with self.assertRaises(ssl.SSLCertVerificationError), \
                    socket.create_connection(("127.0.0.1", server.port), timeout=5) as raw:
                ssl.create_default_context().wrap_socket(raw, server_hostname="127.0.0.1")
            self.assertTrue(self.wait_for(lambda: sum(request["kind"] == "tls-error" for request in server.requests) >= 2))
            port = server.port
        self.assertFalse(server.running)
        with self.assertRaises(OSError), socket.create_connection(("127.0.0.1", port), timeout=2):
            pass
        self.assertTrue(any("offered 20261106.1.0" in message for message in self.logs), self.logs)


if __name__ == "__main__":
    unittest.main()
