import importlib.machinery
import importlib.util
import types
import unittest
from pathlib import Path
from unittest import mock


SCRIPT_PATH = Path(__file__).with_name("aks-flex-config")


def load_helper():
    loader = importlib.machinery.SourceFileLoader("aks_flex_config", str(SCRIPT_PATH))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class RenderConfigTest(unittest.TestCase):
    def setUp(self):
        self.helper = load_helper()
        self.metadata = {
            "subscription_id": "00000000-0000-0000-0000-000000000000",
            "tenant_id": "11111111-1111-1111-1111-111111111111",
            "resource_id": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks",
            "location": "eastus",
            "kubernetes_version": "1.35.5",
            "dns_service_ip": "10.42.0.10",
            "agent_pool_name": "aksflexnodes",
        }

    def test_identity_config_includes_legacy_kubernetes_version_alias(self):
        args = types.SimpleNamespace(username="")

        config = self.helper.render_config(args, "identity", self.metadata)

        self.assertEqual(config["components"]["kubernetes"], "1.35.5")
        self.assertEqual(config["kubernetes"]["version"], "1.35.5")

    def test_bootstrap_config_includes_current_and_legacy_kubelet_fields(self):
        args = types.SimpleNamespace()
        server_url = "https://test-cluster-abc123.hcp.eastus.azmk8s.io:443"

        def fake_run(command, *, input_text=None, capture=False):
            del input_text, capture
            if command[-1] == "jsonpath={.clusters[0].cluster.server}":
                return server_url
            if command[-1] == "jsonpath={.clusters[0].cluster.certificate-authority-data}":
                return "base64-ca-data"
            raise AssertionError(f"unexpected command: {command}")

        with mock.patch.object(self.helper, "load_admin_kubeconfig"), mock.patch.object(
            self.helper, "generate_bootstrap_token", return_value="abcdef.0123456789abcdef"
        ), mock.patch.object(self.helper, "run", side_effect=fake_run):
            config = self.helper.render_config(args, "bootstrap-token", self.metadata)

        kubelet = config["node"]["kubelet"]
        self.assertEqual(config["components"]["kubernetes"], "1.35.5")
        self.assertEqual(config["kubernetes"]["version"], "1.35.5")
        self.assertEqual(config["networking"]["dnsServiceIP"], "10.42.0.10")
        self.assertEqual(kubelet["clusterFQDN"], "test-cluster-abc123.hcp.eastus.azmk8s.io:443")
        self.assertEqual(kubelet["serverURL"], server_url)
        self.assertEqual(kubelet["dnsServiceIP"], "10.42.0.10")
        self.assertEqual(kubelet["caCertData"], "base64-ca-data")


if __name__ == "__main__":
    unittest.main()
