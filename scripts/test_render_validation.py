import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location(
    "renderer", Path(__file__).with_name("render-validation-manifests.py"))
renderer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(renderer)


class RenderTests(unittest.TestCase):
    def test_images_cluster_and_scoped_scheduling(self):
        deployment = {"kind": "Deployment", "metadata": {"name": "stateful-member"},
                      "spec": {"template": {"spec": {"containers": [
                          {"image": "old", "args": ["--cluster-name=aws", "--mode=member"]}]}}}}
        daemon = copy.deepcopy(deployment)
        daemon["kind"] = "DaemonSet"
        daemon["metadata"]["name"] = "stateful-artifact"
        secret = {"kind": "Secret", "data": {"kubeconfig": "unchanged"}}
        items = renderer.render([None, deployment, daemon, secret], {"old": "new"},
                                "onprem", True)["items"]
        pod = items[0]["spec"]["template"]["spec"]
        self.assertEqual(pod["containers"][0]["image"], "new")
        self.assertEqual(pod["containers"][0]["args"], ["--cluster-name=onprem", "--mode=member"])
        self.assertEqual(len(pod["tolerations"]), 2)
        artifact = items[1]["spec"]["template"]["spec"]
        self.assertNotIn("tolerations", artifact)
        self.assertEqual(artifact["nodeSelector"]["migration.dcnlab.com/artifact-node"], "true")
        self.assertEqual(items[2], secret)

    def test_default_preserves_scheduling_and_payload_is_replaced(self):
        obj = {"kind": "Deployment", "metadata": {"name": "fluidcr-webhook"},
               "spec": {"template": {"spec": {"containers": [
                   {"image": "injector", "args": ["--payload-image=old"]}]}}}}
        pod = renderer.render([obj], {"payload": "reviewed"})["items"][0]["spec"]["template"]["spec"]
        self.assertNotIn("tolerations", pod)
        self.assertEqual(pod["containers"][0]["args"], ["--payload-image=reviewed"])


if __name__ == "__main__":
    unittest.main()
