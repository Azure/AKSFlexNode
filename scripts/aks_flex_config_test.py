import argparse
import importlib.machinery
import importlib.util
from pathlib import Path
import unittest
from unittest import mock


SCRIPT_PATH = Path(__file__).with_name("aks-flex-config")


def load_module():
    loader = importlib.machinery.SourceFileLoader("aks_flex_config", str(SCRIPT_PATH))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class ClusterMetadataTest(unittest.TestCase):
    def test_cluster_metadata_uses_current_kubernetes_version(self):
        module = load_module()
        args = argparse.Namespace(resource_group="rg", cluster_name="cluster", subscription="sub")
        responses = iter(["sub", "tenant", "resource-id", "westus2", "1.34.8", "10.0.0.10"])

        with mock.patch.object(module, "run", side_effect=lambda *unused_args, **unused_kwargs: next(responses)) as run:
            metadata = module.cluster_metadata(args)

        self.assertEqual(metadata["kubernetes_version"], "1.34.8")
        run.assert_any_call(
            [
                "az",
                "aks",
                "show",
                "--resource-group",
                "rg",
                "--name",
                "cluster",
                "--subscription",
                "sub",
                "--query",
                "currentKubernetesVersion",
                "-o",
                "tsv",
            ],
            capture=True,
        )


if __name__ == "__main__":
    unittest.main()
