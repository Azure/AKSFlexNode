"""Unit tests for flexcluster.py, which run without a cluster, sudo, or the network:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_flexcluster.py'
"""

from __future__ import annotations

import base64
import copy
import dataclasses
import datetime
import itertools
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import flexcluster  # noqa: E402
from errors import StepError  # noqa: E402

SPEC = flexcluster.ClusterSpec(user="tester")
PREFIX = "acl-abupdate.io"
UTC = datetime.timezone.utc
CA_PEM = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"

NODE = {
    "metadata": {
        "name": "acl-node",
        "uid": "uid-1",
        "resourceVersion": "42",
        "annotations": {f"{PREFIX}/a": "1", f"{PREFIX}/b": "2", "other.io/x": "y", f"{PREFIX}x/c": "3"},
    },
    "spec": {
        "taints": [
            {"key": "node.kubernetes.io/unreachable", "effect": "NoExecute", "timeAdded": "2026-11-12T10:00:00Z"},
            {"key": "example.com/t", "value": "v", "effect": "NoSchedule"},
        ],
    },
    "status": {
        "conditions": [
            {"type": "MemoryPressure", "status": "False", "reason": "KubeletHasSufficientMemory"},
            {"type": "Ready", "status": "True", "reason": "KubeletReady", "message": "kubelet is posting ready status"},
        ],
        "addresses": [{"type": "InternalIP", "address": "192.168.113.10"}, {"type": "Hostname", "address": "acl-node"}],
        "nodeInfo": {"kubeletVersion": "v1.37.0"},
    },
}
LEASE = {"spec": {"holderIdentity": "acl-node", "renewTime": "2026-11-12T20:54:01.123456Z"}}
VERSION = json.dumps({"clientVersion": {"gitVersion": "v1.37.1"}, "serverVersion": {"gitVersion": "v1.37.0"}})


class FakeRun:
    """A run that records commands and answers them from rules: the first rule whose needle is in
    the command line, or starts it for a needle starting with ^, and that has answers left,
    answers."""

    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.rules: list[list] = []

    def on(self, needle: str, stdout: str = "", stderr: str = "", returncode: int = 0,
           times: int | None = None) -> FakeRun:
        self.rules.append([needle, returncode, stdout, stderr, times])
        return self

    def __call__(self, cmd, *, input=None, timeout=None, stream=False):
        self.calls.append({"cmd": list(cmd), "input": input, "timeout": timeout, "stream": stream})
        line = " ".join(cmd)
        returncode, stdout, stderr = 0, "", ""
        for rule in self.rules:
            needle = rule[0]
            matches = line.startswith(needle[1:]) if needle.startswith("^") else needle in line
            if matches and (rule[4] is None or rule[4] > 0):
                if rule[4] is not None:
                    rule[4] -= 1
                returncode, stdout, stderr = rule[1], rule[2], rule[3]
                break
        if stream:
            return subprocess.CompletedProcess(cmd, returncode, None, None)
        return subprocess.CompletedProcess(cmd, returncode, stdout, stderr)

    def commands(self, needle: str = "") -> list[list[str]]:
        return [call["cmd"] for call in self.calls if needle in " ".join(call["cmd"])]

    def lines(self) -> list[str]:
        return [" ".join(call["cmd"]) for call in self.calls]


class ClusterTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp, ignore_errors=True))
        self.run = FakeRun()
        self.logs: list[str] = []

    def cluster(self, spec: flexcluster.ClusterSpec = SPEC) -> flexcluster.Cluster:
        cluster = flexcluster.Cluster(spec, kubeconfig=self.tmp / "kubeconfig", repo=self.tmp / "repo",
                                      work_dir=self.tmp / "work", run=self.run, log=self.logs.append)
        cluster.sleep = lambda seconds: None
        clock = itertools.count(step=50)
        cluster.monotonic = lambda: float(next(clock))
        cluster.which = lambda name: f"/home/u/go/bin/{name}"
        cluster.inotify_path = self.tmp / "max_user_instances"
        return cluster

    def identity_rules(self, uid: str = "ns-uid") -> None:
        self.run.on("get namespace kube-system", stdout=uid)
        self.run.on("config view --minify --raw", stdout=base64.b64encode(CA_PEM.encode()).decode())


def links(*entries: dict) -> str:
    return json.dumps(list(entries))


def link(name: str, *, master: str = "", up: bool = True, addresses: tuple[str, ...] = ()) -> dict:
    entry = {"ifname": name, "flags": ["BROADCAST", "MULTICAST"] + (["UP"] if up else []),
             "addr_info": [{"family": "inet", "local": a.split("/")[0], "prefixlen": int(a.split("/")[1])}
                           for a in addresses]}
    if master:
        entry["master"] = master
    return entry


# ---------------------------------------------------------------------------------------------


class ClusterSpecTest(unittest.TestCase):
    def test_defaults_and_properties(self) -> None:
        spec = flexcluster.ClusterSpec()
        spec.validate()
        self.assertEqual(spec.gateway, "192.168.113.1")
        self.assertEqual(spec.kind_ip, "192.168.113.2")
        self.assertEqual(spec.vm_ip, "192.168.113.10")
        self.assertEqual(spec.api_server, "192.168.113.2:6443")
        self.assertEqual(spec.node_container, "aclab-control-plane")

    def test_round_trip(self) -> None:
        spec = flexcluster.ClusterSpec(name="aclab-dev", subnet="10.20.30", bridge="virbr-x", tap="tap-x",
                                       veth_host="veth-x", veth_kind="eth-x", node_image="kindest/node:v1.37.0",
                                       user="1000")
        data = json.loads(json.dumps(spec.to_dict()))
        self.assertEqual(flexcluster.ClusterSpec.from_dict(data), spec)
        self.assertEqual(flexcluster.ClusterSpec.from_dict({**data, "later": "ignored"}), spec)

    def test_frozen(self) -> None:
        with self.assertRaises(dataclasses.FrozenInstanceError):
            SPEC.name = "other"  # type: ignore[misc]

    def test_invalid(self) -> None:
        cases = {
            "bridge too long": {"bridge": "virbr-sixteen-ch"},
            "tap with a slash": {"tap": "tap/x"},
            "veth with a space": {"veth_kind": "eth x"},
            "same names": {"tap": "virbr-aclab"},
            "two octets": {"subnet": "192.168"},
            "four octets": {"subnet": "192.168.113.0"},
            "octet too big": {"subnet": "192.168.300"},
            "leading zero": {"subnet": "192.168.013"},
            "letters": {"subnet": "a.b.c"},
            "uppercase name": {"name": "AcLab"},
            "name too long": {"name": "a" * 50},
            "name ends with a dash": {"name": "aclab-"},
            "user with a space": {"user": "a b"},
            "image with a quote": {"node_image": 'kindest/node:"x"'},
            "not a string": {"subnet": 192},
        }
        for name, change in cases.items():
            with self.subTest(name), self.assertRaises(StepError):
                flexcluster.ClusterSpec(**change).validate()
        with self.assertRaises(StepError):
            flexcluster.ClusterSpec.from_dict({"subnet": "300.1.1"})

    def test_cluster_validates_its_spec(self) -> None:
        with self.assertRaises(StepError):
            flexcluster.Cluster(flexcluster.ClusterSpec(subnet="x"), kubeconfig=Path("k"), repo=Path("."),
                                work_dir=Path("w"), run=FakeRun(), log=print)


class NetworkPlanTest(unittest.TestCase):
    FORBIDDEN = ("MASQUERADE", "sysctl", "nat", "POSTROUTING", "ip_forward", "nft", "nmcli", "DOCKER-USER")

    def test_up(self) -> None:
        up = flexcluster.network_up_commands(SPEC)
        self.assertEqual(up, [
            ["sudo", "-n", "ip", "link", "add", "virbr-aclab", "type", "bridge"],
            ["sudo", "-n", "ip", "addr", "add", "192.168.113.1/24", "dev", "virbr-aclab"],
            ["sudo", "-n", "ip", "link", "set", "virbr-aclab", "up"],
            ["sudo", "-n", "ip", "tuntap", "add", "dev", "tap-aclab", "mode", "tap", "user", "tester"],
            ["sudo", "-n", "ip", "link", "set", "tap-aclab", "master", "virbr-aclab"],
            ["sudo", "-n", "ip", "link", "set", "tap-aclab", "up"],
            ["sudo", "-n", "iptables", "-I", "FORWARD", "-i", "virbr-aclab", "-o", "virbr-aclab", "-j", "ACCEPT"],
        ])

    def test_no_nat_or_sysctl(self) -> None:
        for cmd in flexcluster.network_up_commands(SPEC) + flexcluster.network_down_commands(SPEC):
            self.assertEqual(cmd[:2], ["sudo", "-n"])
            for word in self.FORBIDDEN:
                self.assertNotIn(word, cmd)

    def test_down_mirrors_up(self) -> None:
        up = flexcluster.network_up_commands(SPEC)
        down = flexcluster.network_down_commands(SPEC)
        made = [cmd[6] if cmd[3] == "tuntap" else cmd[5] for cmd in up
                if cmd[2:5] in (["ip", "link", "add"], ["ip", "tuntap", "add"])]
        deleted = [cmd[-1] for cmd in down if cmd[2:5] == ["ip", "link", "delete"]]
        self.assertEqual(made, ["virbr-aclab", "tap-aclab"])
        # The rule goes first, then the veth attaching made, then the links in reverse.
        self.assertEqual(deleted, ["veth-aclab-kind", "tap-aclab", "virbr-aclab"])
        inserted = next(cmd for cmd in up if "-I" in cmd)
        removed = next(cmd for cmd in down if "-D" in cmd)
        self.assertEqual(down[0], removed)
        self.assertEqual(inserted[inserted.index("-I") + 1:], removed[removed.index("-D") + 1:])

    def test_up_needs_a_tap_owner(self) -> None:
        with self.assertRaises(StepError):
            flexcluster.network_up_commands(flexcluster.ClusterSpec())

    def test_repair_from_nothing_is_up(self) -> None:
        self.assertEqual(flexcluster.network_repair_commands(SPEC, [], False), flexcluster.network_up_commands(SPEC))

    def test_repair_whole_network_is_nothing(self) -> None:
        present = [link("virbr-aclab", addresses=("192.168.113.1/24",)), link("tap-aclab", master="virbr-aclab")]
        self.assertEqual(flexcluster.network_repair_commands(SPEC, present, True), [])

    def test_repair_keeps_an_existing_tap(self) -> None:
        # The bridge was deleted, which detached the TAP; QEMU may hold it, so it isn't recreated.
        present = [link("tap-aclab")]
        commands = flexcluster.network_repair_commands(SPEC, present, True)
        self.assertEqual(commands, [
            ["sudo", "-n", "ip", "link", "add", "virbr-aclab", "type", "bridge"],
            ["sudo", "-n", "ip", "addr", "add", "192.168.113.1/24", "dev", "virbr-aclab"],
            ["sudo", "-n", "ip", "link", "set", "virbr-aclab", "up"],
            ["sudo", "-n", "ip", "link", "set", "tap-aclab", "master", "virbr-aclab"],
        ])

    def test_forward_rule_count(self) -> None:
        listing = ("-P FORWARD ACCEPT\n"
                   "-A FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT\n"
                   "-A FORWARD -i virbr-aclc -j ACCEPT\n"
                   "-A FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT\n")
        self.assertEqual(flexcluster.forward_rule_count(SPEC, listing), 2)
        self.assertEqual(flexcluster.forward_rule_count(SPEC, "-P FORWARD ACCEPT\n"), 0)

    def test_attach(self) -> None:
        commands = flexcluster.attach_commands(SPEC, "4242")
        self.assertTrue(all(cmd[:2] == ["sudo", "-n"] for cmd in commands))
        self.assertIn(["sudo", "-n", "ip", "link", "set", "eth-aclab", "netns", "4242"], commands)
        self.assertIn(["sudo", "-n", "nsenter", "-t", "4242", "-n", "ip", "addr", "add", "192.168.113.2/24",
                       "dev", "eth-aclab"], commands)
        self.assertEqual(commands[0], ["sudo", "-n", "ip", "link", "add", "veth-aclab-kind", "type", "veth",
                                       "peer", "name", "eth-aclab"])


class KindConfigTest(unittest.TestCase):
    def test_cert_sans(self) -> None:
        config = flexcluster.kind_config(SPEC)
        self.assertIn("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\n", config)
        self.assertIn('certSANs: ["localhost", "127.0.0.1", "192.168.113.2"]', config)
        self.assertIn("kind: ClusterConfiguration", config)
        self.assertNotIn("image:", config)

    def test_node_image(self) -> None:
        config = flexcluster.kind_config(dataclasses.replace(SPEC, node_image="kindest/node:v1.37.0@sha256:ab"))
        self.assertIn('  - role: control-plane\n    image: "kindest/node:v1.37.0@sha256:ab"\n', config)

    def test_advertise_script(self) -> None:
        script = flexcluster.advertise_script(SPEC)
        self.assertIn("--node-ip=192.168.113.2", script)
        self.assertNotIn("@NODE_IP@", script)

    def test_kube_proxy_kubeconfig(self) -> None:
        text = "apiVersion: v1\nclusters:\n- cluster:\n    server: https://aclab-control-plane:6443\n  name: default\n"
        rewritten = flexcluster.kube_proxy_kubeconfig(text, SPEC.api_server)
        self.assertIn("    server: https://192.168.113.2:6443\n", rewritten)
        self.assertEqual(flexcluster.kube_proxy_kubeconfig(rewritten, SPEC.api_server), rewritten)

    def test_kindnet_patch(self) -> None:
        daemonset = {"spec": {"template": {"spec": {"containers": [
            {"name": "kindnet-cni", "env": [{"name": "HOST_IP"}, {"name": "CONTROL_PLANE_ENDPOINT",
                                                                   "value": "aclab-control-plane:6443"}]}]}}}}
        patch = flexcluster.kindnet_patch(daemonset, SPEC.api_server)
        self.assertEqual(patch["spec"]["template"]["spec"]["containers"],
                         [{"name": "kindnet-cni", "env": [{"name": "CONTROL_PLANE_ENDPOINT",
                                                           "value": "192.168.113.2:6443"}]}])
        daemonset["spec"]["template"]["spec"]["containers"][0]["env"][1]["value"] = SPEC.api_server
        self.assertIsNone(flexcluster.kindnet_patch(daemonset, SPEC.api_server))
        with self.assertRaises(StepError):
            flexcluster.kindnet_patch({"spec": {}}, SPEC.api_server)

    def test_inotify_warning(self) -> None:
        self.assertIn("sudo sysctl fs.inotify.max_user_instances=512", flexcluster.inotify_warning("128\n"))
        self.assertIsNone(flexcluster.inotify_warning("512\n"))
        self.assertIsNone(flexcluster.inotify_warning("garbage"))


class ManifestTest(unittest.TestCase):
    OBJECTS = [
        {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "aks-flex-machines", "namespace": "kube-system"},
         "data": {}},
        {"apiVersion": "apps/v1", "kind": "Deployment",
         "metadata": {"name": "aks-flex-controller", "namespace": "kube-system", "labels": None},
         "spec": {"template": {"metadata": {"labels": {"app.kubernetes.io/name": "aks-flex-controller"}},
                               "spec": {"containers": [{"name": "c", "image": "ghcr.io/x:latest",
                                                        "imagePullPolicy": "Always", "args": ["--x"]}]}}}},
    ]

    def test_controller_manifest(self) -> None:
        manifest = flexcluster.controller_manifest(self.OBJECTS, "localhost/aks-flex-controller:aclab", "abc")
        self.assertEqual(manifest["kind"], "List")
        for item in manifest["items"]:
            self.assertEqual(item["metadata"]["labels"][flexcluster.OWNED_LABEL], "true")
        deployment = manifest["items"][1]
        template = deployment["spec"]["template"]
        container = template["spec"]["containers"][0]
        self.assertEqual(container["image"], "localhost/aks-flex-controller:aclab")
        self.assertEqual(container["imagePullPolicy"], "Never")
        self.assertIn("--enable-csr-approver=true", container["args"])
        self.assertEqual(template["metadata"]["annotations"][flexcluster.CONTROLLER_DIGEST_ANNOTATION], "abc")
        # reset_to deletes the harness's pods by the label, so the controller's mustn't carry it.
        self.assertNotIn(flexcluster.OWNED_LABEL, template["metadata"]["labels"])
        self.assertEqual(self.OBJECTS[1]["spec"]["template"]["spec"]["containers"][0]["image"], "ghcr.io/x:latest")

    def test_controller_manifest_needs_the_deployment(self) -> None:
        with self.assertRaises(StepError):
            flexcluster.controller_manifest(self.OBJECTS[:1], "i", "d")

    def test_kubectl_items(self) -> None:
        self.assertEqual(flexcluster.kubectl_items('{"kind": "List", "items": [{"kind": "A"}]}'), [{"kind": "A"}])
        self.assertEqual(flexcluster.kubectl_items('{"kind": "A"}'), [{"kind": "A"}])
        # create --dry-run prints several objects one after another.
        self.assertEqual(flexcluster.kubectl_items('{\n "kind": "A"\n}\n{\n "kind": "B"\n}\n'),
                         [{"kind": "A"}, {"kind": "B"}])
        self.assertEqual(flexcluster.kubectl_items(""), [])
        with self.assertRaises(StepError):
            flexcluster.kubectl_items('{"kind": "A"} garbage')

    def test_node_rbac(self) -> None:
        bindings = flexcluster.node_rbac()
        self.assertEqual({b["roleRef"]["name"] for b in bindings},
                         {"system:node-bootstrapper",
                          "system:certificates.k8s.io:certificatesigningrequests:nodeclient"})
        for binding in bindings:
            self.assertEqual(binding["subjects"][0]["name"], flexcluster.BOOTSTRAP_GROUP)
            self.assertEqual(binding["metadata"]["labels"], {flexcluster.OWNED_LABEL: "true"})

    def test_machine_json(self) -> None:
        machine = flexcluster.machine_json("acl-node", "1.37.0", "7")
        self.assertEqual(machine["name"], "acl-node")
        self.assertTrue(machine["id"].endswith("/agentPools/aclab/machines/acl-node"))
        self.assertEqual(machine["properties"]["eTag"], "7")
        self.assertEqual(machine["properties"]["kubernetes"]["orchestratorVersion"], "1.37.0")

    def test_parse_duration(self) -> None:
        self.assertEqual(flexcluster.parse_duration("24h").total_seconds(), 86400)
        self.assertEqual(flexcluster.parse_duration("1h30m").total_seconds(), 5400)
        for bad in ("", "24", "1d", "-1h", "0s", "h"):
            with self.subTest(bad), self.assertRaises(StepError):
                flexcluster.parse_duration(bad)

    def test_bootstrap_token_secret(self) -> None:
        expiration = datetime.datetime(2026, 11, 13, 1, 2, 3, tzinfo=UTC)
        secret = flexcluster.bootstrap_token_secret("abc123", "0123456789abcdef", expiration)
        self.assertEqual(secret["metadata"]["name"], "bootstrap-token-abc123")
        self.assertEqual(secret["type"], "bootstrap.kubernetes.io/token")
        self.assertEqual(secret["stringData"]["expiration"], "2026-11-13T01:02:03Z")
        self.assertEqual(secret["stringData"]["auth-extra-groups"], flexcluster.BOOTSTRAP_GROUP)
        self.assertEqual(secret["metadata"]["labels"], {flexcluster.OWNED_LABEL: "true"})

    def test_probe_manifest(self) -> None:
        manifest = flexcluster.probe_manifest("acl-node", "acl-abupdate")
        namespace, deployment = manifest["items"]
        self.assertEqual(namespace["metadata"]["labels"][flexcluster.OWNED_LABEL], "true")
        spec = deployment["spec"]
        self.assertEqual(spec["replicas"], 1)
        self.assertEqual(spec["template"]["spec"]["nodeSelector"], {"kubernetes.io/hostname": "acl-node"})
        self.assertEqual(spec["template"]["spec"]["tolerations"], [{"operator": "Exists"}])
        container = spec["template"]["spec"]["containers"][0]
        self.assertEqual(container["image"], "mcr.microsoft.com/azurelinux/busybox:1.36")
        self.assertIn("trap 'exit 0' TERM", container["command"][2])
        self.assertEqual(spec["template"]["metadata"]["labels"][flexcluster.OWNED_LABEL], "true")

    def test_redact(self) -> None:
        text = "token abcdef.0123456789abcdef here\n-----BEGIN RSA PRIVATE KEY-----\nxyz\n-----END RSA PRIVATE KEY-----\n"
        redacted = flexcluster.redact(text)
        self.assertNotIn("0123456789abcdef", redacted)
        self.assertNotIn("xyz", redacted)


class RecordTest(ClusterTest):
    def changed_node(self) -> dict:
        node = copy.deepcopy(NODE)
        node["metadata"]["resourceVersion"] = "43"
        node["metadata"]["annotations"].update({f"{PREFIX}/a": "9", f"{PREFIX}/extra": "x"})
        del node["metadata"]["annotations"][f"{PREFIX}/b"]
        node["spec"]["unschedulable"] = True
        node["spec"]["taints"] = [
            {"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule", "timeAdded": "2026-11-12T11:00:00Z"},
            {"key": "scenario.io/added", "effect": "NoExecute"},
        ]
        return node

    def test_node_record(self) -> None:
        record = flexcluster.node_record(NODE, PREFIX)
        self.assertEqual(record, {
            "node": "acl-node",
            "uid": "uid-1",
            "annotations": {f"{PREFIX}/a": "1", f"{PREFIX}/b": "2"},
            "unschedulable": False,
            "taints": [{"key": "example.com/t", "effect": "NoSchedule", "value": "v"}],
        })

    def test_no_patch_when_equal(self) -> None:
        record = flexcluster.node_record(NODE, PREFIX)
        self.assertIsNone(flexcluster.reset_patch(NODE, record, PREFIX))
        reordered = copy.deepcopy(NODE)
        reordered["spec"]["taints"].reverse()
        reordered["spec"]["taints"][0]["timeAdded"] = "2026-11-12T12:00:00Z"
        self.assertIsNone(flexcluster.reset_patch(reordered, record, PREFIX))

    def test_patch_restores_the_record(self) -> None:
        record = flexcluster.node_record(NODE, PREFIX)
        patch = flexcluster.reset_patch(self.changed_node(), record, PREFIX)
        self.assertEqual(patch["metadata"]["resourceVersion"], "43")
        self.assertEqual(patch["metadata"]["annotations"],
                         {f"{PREFIX}/extra": None, f"{PREFIX}/a": "1", f"{PREFIX}/b": "2"})
        self.assertEqual(patch["spec"]["unschedulable"], None)
        # The node controller's taints stay as they are, timeAdded included.
        self.assertEqual(patch["spec"]["taints"], [
            {"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule", "timeAdded": "2026-11-12T11:00:00Z"},
            {"key": "example.com/t", "effect": "NoSchedule", "value": "v"},
        ])

    def test_patch_to_cordoned_without_taints(self) -> None:
        record = {"uid": "uid-1", "annotations": {}, "unschedulable": True, "taints": []}
        node = copy.deepcopy(NODE)
        node["spec"]["taints"] = [{"key": "example.com/t", "value": "v", "effect": "NoSchedule"}]
        patch = flexcluster.reset_patch(node, record, PREFIX)
        self.assertEqual(patch["spec"], {"unschedulable": True, "taints": None})
        self.assertEqual(set(patch["metadata"]["annotations"]), {f"{PREFIX}/a", f"{PREFIX}/b"})

    def test_uid_mismatch(self) -> None:
        record = {**flexcluster.node_record(NODE, PREFIX), "uid": "uid-0"}
        with self.assertRaisesRegex(StepError, "recreated.*rejoin"):
            flexcluster.reset_patch(NODE, record, PREFIX)

    def test_record(self) -> None:
        self.identity_rules()
        self.run.on("get node acl-node", stdout=json.dumps(NODE))
        record = self.cluster().record("acl-node", PREFIX)
        self.assertEqual(record["uid"], "uid-1")
        self.assertEqual(record["cluster"]["uid"], "ns-uid")
        import hashlib
        self.assertEqual(record["cluster"]["caSha256"], hashlib.sha256(CA_PEM.encode()).hexdigest())
        json.dumps(record)

    def test_reset_to(self) -> None:
        self.identity_rules()
        self.run.on("get node acl-node", stdout=json.dumps(self.changed_node()))
        record = {**flexcluster.node_record(NODE, PREFIX), "cluster": {"uid": "ns-uid", "caSha256": ""}}
        record["cluster"] = self.cluster().identity()
        self.run.calls.clear()
        self.cluster().reset_to("acl-node", record, PREFIX)
        patches = self.run.commands("patch node acl-node")
        self.assertEqual(len(patches), 1)
        self.assertEqual(json.loads(patches[0][-1])["metadata"]["resourceVersion"], "43")
        pods = self.run.commands("delete pods")
        self.assertEqual(len(pods), 1)
        self.assertIn("acl-abupdate.io/owned=true,app.kubernetes.io/name!=acl-abupdate-probe", pods[0])
        self.assertIn("--all-namespaces", pods[0])
        operations = self.run.commands("delete machineoperations")
        self.assertIn("acl-abupdate.io/owned=true", operations[0])

    def test_reset_to_uid_mismatch(self) -> None:
        self.run.on("get node acl-node", stdout=json.dumps(NODE))
        record = {**flexcluster.node_record(NODE, PREFIX), "uid": "uid-0"}
        with self.assertRaisesRegex(StepError, "rejoin"):
            self.cluster().reset_to("acl-node", record, PREFIX)
        self.assertEqual(self.run.commands("patch node"), [])
        self.assertEqual(self.run.commands("delete"), [])

    def test_reset_to_another_cluster(self) -> None:
        self.identity_rules(uid="new-uid")
        record = {**flexcluster.node_record(NODE, PREFIX), "cluster": {"uid": "old-uid", "caSha256": "x"}}
        with self.assertRaisesRegex(StepError, "recreated"):
            self.cluster().reset_to("acl-node", record, PREFIX)

    def test_reset_to_retries_a_conflict(self) -> None:
        self.run.on("get node acl-node", stdout=json.dumps(self.changed_node()))
        self.run.on("patch node", returncode=1, times=1,
                    stderr='Error from server (Conflict): Operation cannot be fulfilled on nodes "acl-node": '
                           "the object has been modified; please apply your changes to the latest version")
        self.run.on("delete pods", returncode=1, stderr="timed out waiting for the condition", times=1)
        self.run.on("delete machineoperations", returncode=1,
                    stderr='error: the server doesn\'t have a resource type "machineoperations"')
        self.cluster().reset_to("acl-node", flexcluster.node_record(NODE, PREFIX), PREFIX)
        self.assertEqual(len(self.run.commands("patch node")), 2)
        # A pod on a node that's down never finishes terminating, so it's forced.
        self.assertIn("--force", self.run.commands("delete pods")[1])

    def test_reset_to_node_gone(self) -> None:
        with self.assertRaisesRegex(StepError, "gone"):
            self.cluster().reset_to("acl-node", flexcluster.node_record(NODE, PREFIX), PREFIX)


class NodeTest(ClusterTest):
    def test_parsing(self) -> None:
        self.assertEqual(flexcluster.condition(NODE, "Ready")["status"], "True")
        self.assertEqual(flexcluster.condition(NODE, "DiskPressure"), {})
        self.assertEqual(flexcluster.internal_ips(NODE), ["192.168.113.10"])
        self.assertIn("Ready=True (KubeletReady: kubelet is posting ready status)",
                      flexcluster.describe_conditions(NODE))
        self.assertEqual(flexcluster.describe_conditions(None), "the Node doesn't exist")
        for version, release in {"v1.37.0": "1.37.0", "v1.37.0+k3s1": "1.37.0", "1.37.2-rc.1": "1.37.2"}.items():
            self.assertEqual(flexcluster.release_version(version), release)
        self.assertEqual(flexcluster.parse_server_version(VERSION), "1.37.0")
        with self.assertRaises(StepError):
            flexcluster.parse_server_version('{"clientVersion": {}}')

    def test_lease_time(self) -> None:
        renewed = flexcluster.parse_lease_renew_time(LEASE)
        self.assertEqual(renewed, datetime.datetime(2026, 11, 12, 20, 54, 1, 123456, tzinfo=UTC))
        self.assertIsNone(flexcluster.parse_lease_renew_time(None))
        self.assertIsNone(flexcluster.parse_lease_renew_time({"spec": {}}))
        self.assertIsNone(flexcluster.parse_time("not a time"))

    def test_node_and_lease_not_found(self) -> None:
        cluster = self.cluster()
        self.assertIsNone(cluster.node("acl-node"))
        self.assertFalse(cluster.node_ready("acl-node"))
        self.assertIsNone(cluster.lease_renew_time("acl-node"))
        self.assertIn("--ignore-not-found", self.run.commands("get node")[0])

    def test_node_ready_on_api_errors(self) -> None:
        self.run.on("get node", returncode=1, stderr="connection refused")
        self.assertFalse(self.cluster().node_ready("acl-node"))
        with self.assertRaises(StepError):
            self.cluster().node("acl-node")

    def test_wait_ready_times_out_with_conditions(self) -> None:
        node = copy.deepcopy(NODE)
        node["status"]["conditions"][1].update(status="False", reason="KubeletNotReady", message="CNI not ready")
        self.run.on("get node acl-node", stdout=json.dumps(node))
        with self.assertRaisesRegex(StepError, "Ready=False \\(KubeletNotReady: CNI not ready\\)"):
            self.cluster().wait_ready("acl-node", 120)

    def findings(self, node: dict | None, since: datetime.datetime | None) -> list:
        self.run.on("get node acl-node", stdout=json.dumps(node) if node else "")
        self.run.on("version -o json", stdout=VERSION)
        self.run.on("get lease acl-node", stdout=json.dumps(LEASE))
        return self.cluster().node_findings("acl-node", expected_ip="192.168.113.10", since=since)

    def test_node_findings_ok(self) -> None:
        since = datetime.datetime(2026, 11, 12, 20, 54, 0, tzinfo=UTC)
        findings = self.findings(NODE, since)
        self.assertEqual(len(findings), 4)
        self.assertTrue(all(ok for ok, _, _ in findings), findings)

    def test_node_findings_without_since(self) -> None:
        findings = self.findings(NODE, None)
        self.assertEqual(len(findings), 3)
        self.assertEqual(self.run.commands("lease"), [])

    def test_node_findings_failures(self) -> None:
        node = copy.deepcopy(NODE)
        node["status"]["addresses"][0]["address"] = "10.0.2.15"
        node["status"]["nodeInfo"]["kubeletVersion"] = "v1.36.4"
        since = datetime.datetime(2026, 11, 12, 21, 0, 0)  # naive is taken as UTC
        ready, address, version, lease = self.findings(node, since)
        self.assertTrue(ready[0])
        self.assertFalse(address[0])
        self.assertIn("10.0.2.15", address[2])
        self.assertFalse(version[0])
        self.assertIn("v1.36.4", version[2])
        self.assertFalse(lease[0])

    def test_node_findings_node_missing(self) -> None:
        findings = self.findings(None, datetime.datetime(2026, 11, 12, tzinfo=UTC))
        self.assertEqual(len(findings), 4)
        self.assertTrue(all(not ok and detail == "the Node doesn't exist" for ok, _, detail in findings))

    def test_node_findings_api_down(self) -> None:
        self.run.on("get node", returncode=1, stderr="dial tcp: connection refused")
        findings = self.cluster().node_findings("acl-node", expected_ip="192.168.113.10")
        self.assertEqual(len(findings), 1)
        self.assertFalse(findings[0][0])


def pod(name: str, node: str, phase: str = "Running", ready: bool = True, **extra) -> dict:
    conditions = [{"type": "Ready", "status": "True" if ready else "False"}]
    result = {"metadata": {"name": name}, "spec": {"nodeName": node},
              "status": {"phase": phase, "conditions": conditions}}
    for key, value in extra.items():
        result[key] = {**result.get(key, {}), **value}
    return result


class ProbeTest(ClusterTest):
    def test_probe_pod(self) -> None:
        pods = {"items": [
            pod("old", "acl-node", metadata={"name": "old", "deletionTimestamp": "2026-11-12T00:00:00Z"}),
            pod("elsewhere", "other-node"),
            pod("good", "acl-node"),
        ]}
        found, detail = flexcluster.probe_pod(pods, "acl-node")
        self.assertEqual(found["metadata"]["name"], "good")
        self.assertIsNone(flexcluster.probe_pod({"items": []}, "acl-node")[0])

    def test_findings_ok(self) -> None:
        self.run.on("get pods -l app.kubernetes.io/name=acl-abupdate-probe",
                    stdout=json.dumps({"items": [pod("acl-abupdate-probe-1", "acl-node")]}))
        self.run.on("logs acl-abupdate-probe-1", stdout="acl-abupdate-probe-ok\n")
        self.run.on("exec acl-abupdate-probe-1", stdout="acl-abupdate-exec-ok\n")
        findings = self.cluster().probe_findings("acl-node")
        self.assertEqual(len(findings), 3)
        self.assertTrue(all(ok for ok, _, _ in findings), findings)
        self.assertIn(["--", "echo", "acl-abupdate-exec-ok"],
                      [cmd[-3:] for cmd in self.run.commands("exec")])

    def test_findings_unschedulable(self) -> None:
        pending = pod("acl-abupdate-probe-1", "", phase="Pending", ready=False)
        pending["status"]["conditions"].append({"type": "PodScheduled", "status": "False",
                                                "message": "0/1 nodes are available: 1 node(s) didn't match"})
        self.run.on("get pods", stdout=json.dumps({"items": [pending]}))
        findings = self.cluster().probe_findings("acl-node", timeout=0)
        self.assertEqual([ok for ok, _, _ in findings], [False, False, False])
        self.assertIn("didn't match", findings[0][2])
        self.assertEqual(self.run.commands("logs"), [])

    def test_findings_kubelet_unreachable(self) -> None:
        self.run.on("get pods", stdout=json.dumps({"items": [pod("p", "acl-node")]}))
        self.run.on("logs p", returncode=1, stderr="Error from server: dial tcp 192.168.113.10:10250: i/o timeout")
        self.run.on("exec p", stdout="acl-abupdate-exec-ok\n")
        _, logs, executed = self.cluster().probe_findings("acl-node")
        self.assertFalse(logs[0])
        self.assertIn("10250", logs[2])
        self.assertTrue(executed[0])

    def test_ensure_probe(self) -> None:
        self.cluster().ensure_probe("acl-node")
        call = self.run.calls[0]
        self.assertEqual(call["cmd"][-3:], ["apply", "-f", "-"])
        self.assertEqual(json.loads(call["input"])["items"][1]["metadata"]["name"], "acl-abupdate-probe")


class LifecycleTest(ClusterTest):
    def create_rules(self) -> None:
        self.run.on("kind get clusters", stdout="")
        self.run.on("kind get kubeconfig", stdout="apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:41234\n")
        self.run.on("podman inspect --format {{.State.Pid}}", stdout="4242\n")
        self.run.on("^ip -j addr show", stdout="[]")
        self.run.on("jsonpath={.status.addresses", stdout="192.168.113.2")
        self.run.on("get daemonset kindnet -o json", stdout=json.dumps({"spec": {"template": {"spec": {"containers": [
            {"name": "kindnet-cni", "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": "aclab-control-plane:6443"}]}]}}}}))
        self.run.on("get configmap kube-proxy -o json", stdout=json.dumps(
            {"data": {"kubeconfig.conf": "clusters:\n- cluster:\n    server: https://aclab-control-plane:6443\n"}}))

    def test_create(self) -> None:
        self.create_rules()
        cluster = self.cluster()
        cluster.inotify_path.write_text("128\n")
        cluster.create()
        lines = self.run.lines()
        kind_create = next(call for call in self.run.calls if "create cluster" in " ".join(call["cmd"]))
        self.assertEqual(kind_create["cmd"][:5], ["sudo", "-n", "env", "KIND_EXPERIMENTAL_PROVIDER=podman",
                                                  "/home/u/go/bin/kind"])
        self.assertTrue(kind_create["stream"])
        config = Path(kind_create["cmd"][kind_create["cmd"].index("--config") + 1])
        self.assertIn("192.168.113.2", config.read_text())
        self.assertEqual(config.parent.stat().st_mode & 0o777, 0o700)
        # The bridge exists before kind, and kind's node is attached after.
        bridge = lines.index("sudo -n ip link add virbr-aclab type bridge")
        self.assertLess(bridge, lines.index(" ".join(kind_create["cmd"])))
        self.assertLess(lines.index(" ".join(kind_create["cmd"])),
                        lines.index("sudo -n ip link set eth-aclab netns 4242"))
        self.assertTrue(any("--node-ip=192.168.113.2" in line for line in lines))
        self.assertTrue(any("patch configmap kube-proxy" in line for line in lines))
        self.assertTrue(any("patch daemonset kindnet" in line for line in lines))
        self.assertEqual(cluster.kubeconfig.stat().st_mode & 0o777, 0o600)
        self.assertIn("127.0.0.1:41234", cluster.kubeconfig.read_text())
        self.assertTrue(any("sudo sysctl fs.inotify.max_user_instances=512" in line for line in self.logs))
        for line in lines:
            self.assertFalse(line.startswith("sudo") and not line.startswith("sudo -n "), line)

    def test_kind_needs_installing(self) -> None:
        cluster = self.cluster()
        cluster.which = lambda name: None
        with self.assertRaisesRegex(StepError, "kind is needed"):
            cluster.exists()

    def whole(self) -> None:
        self.run.on("kind get clusters", stdout="aclab\nother\n")
        self.run.on("kind get kubeconfig", stdout="clusters:\n- cluster:\n    server: https://127.0.0.1:41234\n")
        self.run.on("{{.State.Running}}", stdout="true\n")
        self.run.on("{{.State.Pid}}", stdout="4242\n")
        self.run.on("iptables -S FORWARD", stdout="-P FORWARD ACCEPT\n-A FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT\n")
        self.run.on("nsenter -t 4242 -n ip -j addr show", stdout=links(link("eth-aclab", addresses=("192.168.113.2/24",))))
        self.run.on("jsonpath={.status.addresses", stdout="192.168.113.2")
        self.run.on("get node aclab-control-plane", stdout=json.dumps(NODE))

    def test_ensure_leaves_a_whole_cluster(self) -> None:
        self.run.on("^ip -j addr show", stdout=links(
            link("virbr-aclab", addresses=("192.168.113.1/24",)), link("tap-aclab", master="virbr-aclab"),
            link("veth-aclab-kind", master="virbr-aclab")))
        self.whole()
        self.cluster().ensure()
        lines = self.run.lines()
        for mutation in ("ip link add", "ip addr add", "iptables -I", "podman start", "tuntap", "netns"):
            self.assertFalse(any(mutation in line for line in lines), mutation)

    def test_ensure_after_a_restart(self) -> None:
        self.run.on("{{.State.Running}}", stdout="false\n", times=1)
        self.run.on("iptables -S FORWARD", stdout="-P FORWARD ACCEPT\n", times=1)
        self.run.on("^ip -j addr show", stdout="[]")
        self.whole()
        self.cluster().ensure()
        lines = self.run.lines()
        self.assertIn("sudo -n ip link add virbr-aclab type bridge", lines)
        self.assertIn("sudo -n ip tuntap add dev tap-aclab mode tap user tester", lines)
        self.assertIn("sudo -n iptables -I FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT", lines)
        self.assertIn("sudo -n podman start aclab-control-plane", lines)
        self.assertIn("sudo -n ip link set eth-aclab netns 4242", lines)
        self.assertLess(lines.index("sudo -n ip link add virbr-aclab type bridge"),
                        lines.index("sudo -n podman start aclab-control-plane"))

    def test_ensure_without_a_cluster(self) -> None:
        with self.assertRaisesRegex(StepError, "create it"):
            self.cluster().ensure()

    def test_delete_collects_failures(self) -> None:
        self.run.on("kind delete cluster", returncode=1, stderr="ERROR: failed to delete cluster\nboom")
        self.run.on("iptables -D FORWARD", times=2)
        self.run.on("iptables -D FORWARD", returncode=1, stderr="iptables: Bad rule")
        self.run.on("ip link delete tap-aclab", returncode=1, stderr='Cannot find device "tap-aclab"')
        self.run.on("ip link delete virbr-aclab", returncode=1, stderr="RTNETLINK answers: Operation not permitted")
        self.run.on("^ip -j addr show", stdout=links(link("virbr-aclab")))
        self.run.on("iptables -S FORWARD", stdout="-P FORWARD ACCEPT\n")
        cluster = self.cluster()
        cluster.kubeconfig.write_text("x")
        failures = cluster.delete()
        self.assertEqual(len(failures), 3, failures)
        self.assertIn("boom", failures[0])
        self.assertIn("Operation not permitted", failures[1])
        self.assertIn("virbr-aclab", failures[2])
        self.assertEqual(len(self.run.commands("iptables -D")), 3)
        self.assertFalse(cluster.kubeconfig.exists())
        lines = self.run.lines()
        self.assertLess(next(i for i, line in enumerate(lines) if "kind delete" in line),
                        next(i for i, line in enumerate(lines) if "ip link delete" in line))

    def test_delete_without_kind(self) -> None:
        self.run.on("^ip -j addr show", stdout="[]")
        cluster = self.cluster()
        cluster.which = lambda name: None
        failures = cluster.delete()
        self.assertEqual(len(failures), 1)
        self.assertIn("kind is needed", failures[0])
        self.assertIn("sudo -n ip link delete virbr-aclab", self.run.lines())

    def test_delete_clean(self) -> None:
        self.run.on("^ip -j addr show", stdout="[]")
        self.run.on("iptables -D FORWARD", returncode=1, stderr="iptables: Bad rule")
        self.run.on("ip link delete", returncode=1, stderr="Cannot find device")
        self.assertEqual(self.cluster().delete(), [])

    def test_delete_never_raises(self) -> None:
        def broken(cmd, **_):
            raise OSError("no such program")

        cluster = flexcluster.Cluster(SPEC, kubeconfig=self.tmp / "k", repo=self.tmp, work_dir=self.tmp / "w",
                                      run=broken, log=self.logs.append)
        cluster.which = lambda name: "/usr/bin/kind"
        self.assertTrue(cluster.delete())

    def test_timeout_is_a_step_error(self) -> None:
        def slow(cmd, *, input=None, timeout=None, stream=False):
            raise subprocess.TimeoutExpired(cmd, timeout)

        cluster = flexcluster.Cluster(SPEC, kubeconfig=self.tmp / "k", repo=self.tmp, work_dir=self.tmp / "w",
                                      run=slow, log=self.logs.append)
        with self.assertRaisesRegex(StepError, "timed out"):
            cluster.kubectl("get", "nodes")

    def test_bootstrap_token(self) -> None:
        token = self.cluster().create_bootstrap_token("2h")
        self.assertRegex(token, r"^[a-z0-9]{6}\.[a-z0-9]{16}$")
        call = self.run.calls[0]
        self.assertEqual(call["cmd"][-3:], ["create", "-f", "-"])
        self.assertNotIn(token.split(".")[1], " ".join(call["cmd"]))
        self.assertEqual(json.loads(call["input"])["stringData"]["token-secret"], token.split(".")[1])
        self.assertFalse(any(token.split(".")[1] in line for line in self.logs))

    def test_register_machine(self) -> None:
        self.run.on("version -o json", stdout=VERSION)
        self.cluster().register_machine("acl-node", "3")
        patch = json.loads(self.run.commands("patch configmap aks-flex-machines")[0][-1])
        machine = json.loads(patch["data"]["acl-node.json"])
        self.assertEqual(machine["properties"]["kubernetes"]["orchestratorVersion"], "1.37.0")
        self.assertEqual(machine["properties"]["eTag"], "3")

    def test_identity(self) -> None:
        self.identity_rules()
        identity = self.cluster().identity()
        self.assertEqual(identity["uid"], "ns-uid")
        self.assertEqual(len(identity["caSha256"]), 64)
        self.assertEqual(self.cluster().ca_pem(), CA_PEM)

    def test_build_and_deploy_controller(self) -> None:
        controller = self.tmp / "work" / "controller" / "aks-flex-controller"
        controller.parent.mkdir(parents=True)
        controller.write_bytes(b"binary")
        self.run.on("podman image inspect", stdout="sha256:old\n")
        self.run.on("podman build", stdout="new\n")
        self.run.on("mod download -json", stdout=json.dumps({"Dir": "/mod/unbounded@v0.21.0"}))
        self.run.on("create --dry-run=client -o json -f", stdout=json.dumps({"kind": "List", "items": [
            {"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
             "metadata": {"name": "machineoperations.unbounded-cloud.io"}}]}))
        self.run.on("create --dry-run=client -o json -k", stdout=json.dumps({"kind": "List",
                                                                              "items": ManifestTest.OBJECTS}))
        self.cluster().build_and_deploy_controller()
        build = self.run.commands("go -C")[0]
        self.assertEqual(build[:4], ["env", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64"])
        self.assertIn("./cmd/aks-flex-controller", build)
        self.assertIn(["podman", "rmi", "sha256:old"], self.run.commands())
        load = self.run.commands("load image-archive")[0]
        self.assertEqual(load[:5], ["sudo", "-n", "env", "KIND_EXPERIMENTAL_PROVIDER=podman", "/home/u/go/bin/kind"])
        self.assertIn("/mod/unbounded@v0.21.0/deploy/machina/crd/unbounded-cloud.io_machineoperations.yaml",
                      self.run.commands("--dry-run=client -o json -f")[0])
        applied = next(call for call in self.run.calls if call["cmd"][-3:] == ["apply", "-f", "-"])
        kinds = [item["kind"] for item in json.loads(applied["input"])["items"]]
        self.assertEqual(kinds, ["CustomResourceDefinition", "ClusterRoleBinding", "ClusterRoleBinding",
                                 "ConfigMap", "Deployment"])
        self.assertFalse((self.tmp / "work" / "aks-flex-controller.tar").exists())

    def test_diagnostics(self) -> None:
        self.run.on("get node acl-node -o yaml", stdout="token: abcdef.0123456789abcdef\n")
        self.run.on("logs deployment/aks-flex-controller", returncode=1, stderr="not found")
        self.run.on("get events", stdout="HEADER\n" + "".join(f"event {n}\n" for n in range(300)))
        directory = self.tmp / "diag"
        self.cluster().diagnostics("acl-node", directory)
        self.assertNotIn("0123456789abcdef", (directory / "node.yaml").read_text())
        self.assertIn("not found", (directory / "controller.log").read_text())
        events = (directory / "events.txt").read_text().splitlines()
        self.assertEqual(events[0], "HEADER")
        self.assertEqual(len(events), 201)
        self.assertEqual(events[-1], "event 299")

    def test_diagnostics_never_raises(self) -> None:
        def broken(cmd, **_):
            raise RuntimeError("broken")

        cluster = flexcluster.Cluster(SPEC, kubeconfig=self.tmp / "k", repo=self.tmp, work_dir=self.tmp / "w",
                                      run=broken, log=self.logs.append)
        cluster.diagnostics("acl-node", self.tmp / "diag")
        file_in_the_way = self.tmp / "file"
        file_in_the_way.write_text("")
        cluster.diagnostics("acl-node", file_in_the_way / "diag")


if __name__ == "__main__":
    unittest.main()
