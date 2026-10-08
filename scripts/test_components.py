import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

import yaml

from test_validation_guide import UniqueLoader

ROOT = Path(__file__).resolve().parents[1]
MANAGEMENT = {
    "vm-spot-risk-collector": ("collector", "collector.Setup"),
    "policy-manager": ("management", "management.SetupPolicy"),
    "checkpoint-coordinator": ("management", "management.SetupCheckpoint"),
    "spot-recovery-controller": ("management", "management.SetupRecovery"),
}
MEMBER = {
    "training-runtime-collector": "runtime.yaml",
    "spot-watcher": "watcher.yaml",
}


class ComponentTests(unittest.TestCase):
    def test_renderer_replaces_each_component_independently(self):
        variables = {
            "vm-spot-risk-collector": "SPOT_RISK_COLLECTOR_IMAGE",
            "policy-manager": "POLICY_MANAGER_IMAGE",
            "checkpoint-coordinator": "CHECKPOINT_COORDINATOR_IMAGE",
            "spot-recovery-controller": "SPOT_RECOVERY_IMAGE",
            "training-runtime-collector": "RUNTIME_COLLECTOR_IMAGE",
            "spot-watcher": "SPOT_WATCHER_IMAGE",
        }
        env = os.environ.copy()
        for name, variable in variables.items():
            env[variable] = "registry.example/" + name + ":tested"
        for variable in ("PV_IMAGE", "STATEFUL_IMAGE", "INJECTOR_IMAGE", "PAYLOAD_IMAGE"):
            env.pop(variable, None)
        paths = [ROOT / "config/management/deployment.yaml"] + [
            ROOT / "config/member" / path for path in MEMBER.values()]
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts/render-validation-manifests.py")],
            input="\n---\n".join(path.read_text() for path in paths),
            env=env, text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        for obj in json.loads(result.stdout)["items"]:
            image = obj["spec"]["template"]["spec"]["containers"][0]["image"]
            self.assertEqual(image, env[variables[obj["metadata"]["name"]]])

    def test_independent_images_and_entrypoints(self):
        docs = list(yaml.load_all(
            (ROOT / "config/management/deployment.yaml").read_text(), Loader=UniqueLoader))
        self.assertEqual({d["metadata"]["name"] for d in docs}, set(MANAGEMENT))
        docs += [yaml.load((ROOT / "config/member" / path).read_text(), Loader=UniqueLoader)
                 for path in MEMBER.values()]
        images = set()
        for doc in docs:
            name = doc["metadata"]["name"]
            pod = doc["spec"]["template"]["spec"]
            containers = pod["containers"]
            self.assertEqual(len(containers), 1)
            container = containers[0]
            expected_image = {
                "policy-manager": "jeongseungjun/hybrid-spot-vm-system:policy_manager_v3.0",
                "checkpoint-coordinator": "jeongseungjun/hybrid-spot-vm-system:checkpoint_coordinator_v3.0",
            }.get(name, "ghcr.io/gprojectdev/" + name + ":dev")
            self.assertEqual(container["image"], expected_image)
            images.add(container["image"])
            self.assertFalse(any(a.startswith("--mode") for a in container.get("args", [])))
            self.assertEqual(doc["spec"]["selector"]["matchLabels"],
                             doc["spec"]["template"]["metadata"]["labels"])
            source = (ROOT / "cmd" / name / "main.go").read_text()
            self.assertIn('app.Run("' + name + '"', source)
            if name in MANAGEMENT:
                self.assertIn(MANAGEMENT[name][1], source)
                self.assertIn("--kubeconfig=/etc/karmada/kubeconfig", container["args"])
                self.assertFalse(pod["automountServiceAccountToken"])
            if name == "spot-watcher":
                self.assertEqual(doc["kind"], "DaemonSet")
                self.assertIn("member.SpotWatcher", source)
            else:
                self.assertEqual(doc["kind"], "Deployment")
        self.assertEqual(len(images), 6)
        self.assertFalse((ROOT / "cmd/manager/main.go").exists())

    def test_build_contract_includes_all_components(self):
        dockerfile = (ROOT / "Dockerfile").read_text()
        workflow = yaml.safe_load((ROOT / ".github/workflows/ci.yaml").read_text())
        matrix = workflow["jobs"]["images"]["strategy"]["matrix"]["component"]
        self.assertEqual(set(matrix), set(MANAGEMENT) | set(MEMBER) | {"placement-webhook"})
        for component in matrix:
            self.assertIn(component, dockerfile)
        self.assertIn('"./cmd/$COMPONENT"', dockerfile)
        self.assertNotIn("./cmd/manager", dockerfile)

    def test_group_images_are_required_and_rendered(self):
        webhook = (ROOT / "config/management/placement-webhook.yaml").read_text()
        member = {"apiVersion": "apps/v1", "kind": "Deployment",
                  "metadata": {"name": "stateful-member"},
                  "spec": {"template": {"spec": {"containers": [{
                      "name": "manager", "image": "ghcr.io/gprojectdev/stateful-migration-system:dev",
                      "args": ["--group-control-image=placeholder"]}]}}}}
        source = webhook + "\n---\n" + json.dumps(member)
        env = os.environ.copy()
        env["PLACEMENT_WEBHOOK_IMAGE"] = "registry.example/placement:test"
        env["STATEFUL_IMAGE"] = "registry.example/stateful:test"
        env.pop("GROUP_CONTROL_IMAGE", None)
        command = [sys.executable, str(ROOT / "scripts/render-validation-manifests.py")]
        result = subprocess.run(command, input=source, env=env, text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("GROUP_CONTROL_IMAGE", result.stderr)
        env["GROUP_CONTROL_IMAGE"] = "registry.example/group-control:test"
        result = subprocess.run(command, input=source, env=env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        objects = json.loads(result.stdout)["items"]
        deployments = {o["metadata"]["name"]: o for o in objects if o["kind"] == "Deployment"}
        self.assertEqual(deployments["placement-webhook"]["spec"]["template"]["spec"]["containers"][0]["image"], env["PLACEMENT_WEBHOOK_IMAGE"])
        self.assertEqual(deployments["stateful-member"]["spec"]["template"]["spec"]["containers"][0]["args"], ["--group-control-image=" + env["GROUP_CONTROL_IMAGE"]])


if __name__ == "__main__":
    unittest.main()
