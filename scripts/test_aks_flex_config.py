import re
import runpy
import unittest
from pathlib import Path


class RBACManifestTest(unittest.TestCase):
    def test_contains_only_bootstrap_roles(self):
        script = Path(__file__).with_name("aks-flex-config")
        manifest = runpy.run_path(script)["RBAC_MANIFEST"]

        roles = re.findall(r"roleRef:\n(?:  .+\n){2}  name: (.+)", manifest)

        self.assertEqual(
            [
                "system:node-bootstrapper",
                "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
            ],
            roles,
        )
        self.assertEqual(2, manifest.count("kind: ClusterRoleBinding"))


if __name__ == "__main__":
    unittest.main()
