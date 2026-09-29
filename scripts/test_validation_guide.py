import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[1]


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node, deep=False):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in result:
            raise ValueError("duplicate YAML key: " + str(key))
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping)


class GuideTests(unittest.TestCase):
    def test_release_workload_generator_is_two_rank_and_fresh(self):
        text = (ROOT / "docs/release-validation-20260929.md").read_text(encoding="utf-8")
        blocks = re.findall(r"python3 - <<'PY'\n(.*?)\nPY", text, re.S)
        self.assertEqual(len(blocks), 1)
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            sample = base / "stateful/config/samples/two-replica"
            sample.mkdir(parents=True)
            # Match the input shape; controller-only metadata must not be invented.
            source = [{"apiVersion": "v1", "kind": "Service", "metadata": {"name": "trainer"},
                       "spec": {"selector": {"app": "trainer"}}},
                      {"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": {"name": "trainer"},
                       "spec": {"replicas": 2, "template": {"metadata": {"annotations": {
                           "fluidcr.dcnlab.com/inject": "true"}}, "spec": {"containers": [{
                               "name": "trainer", "env": [{"name": "FLUIDCR_DISTRIBUTED", "value": "0"}],
                               "resources": {"limits": {}}}]}}}}]
            (sample / "workload.yaml").write_text(yaml.safe_dump_all(source), encoding="utf-8")
            scripts = base / "fluidcr/examples/ray/with-fluidcr"
            scripts.mkdir(parents=True)
            (scripts / "training-script.yaml").write_text(yaml.safe_dump({
                "apiVersion": "v1", "kind": "ConfigMap",
                "metadata": {"name": "fluidcr-ddp-train-script"}, "data": {"train_ddp.py": "pass"}}),
                encoding="utf-8")
            env = dict(os.environ, NS="fresh-test", STATEFUL=str(base / "stateful"),
                       FLUID=str(base / "fluidcr"), EVIDENCE=tmp, GPU_RUNTIME_CLASS="",
                       TRAINER_IMAGE="trainer:test", STORAGE_CLASS="nfs-client")
            import sys
            import json
            result = subprocess.run([sys.executable, "-c", blocks[0]], env=env,
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            items = json.loads((base / "workload.json").read_text())["items"]
            self.assertTrue(all(d["metadata"]["namespace"] == "fresh-test" for d in items))
            sts = next(d for d in items if d["kind"] == "StatefulSet")
            self.assertEqual(sts["spec"]["replicas"], 2)
            pod = sts["spec"]["template"]["spec"]
            self.assertNotIn("runtimeClassName", pod)
            self.assertFalse(pod["automountServiceAccountToken"])
            c = pod["containers"][0]
            self.assertEqual(c["command"][-1], "/workspace/train_ddp.py")
            envs = {e["name"]: e for e in c["env"]}
            self.assertEqual(envs["FLUIDCR_DISTRIBUTED"]["value"], "1")
            self.assertEqual(envs["WORLD_SIZE"]["value"], "2")
            self.assertIn("fresh-test.svc", envs["MASTER_ADDR"]["value"])
            self.assertEqual(sts["spec"]["volumeClaimTemplates"][0]["metadata"]["name"], "rank-data")

    def test_runbook_manifests_are_separate_documents(self):
        ric = list(yaml.load_all((ROOT / "config/runbook/nodeprovision-status.yaml")
                                .read_text(encoding="utf-8"), Loader=UniqueLoader))
        self.assertEqual(len(ric), 1)
        self.assertEqual(ric[0]["kind"], "ResourceInterpreterCustomization")
        custom = ric[0]["spec"]["customizations"]
        for field in ("statusReflection", "statusAggregation"):
            self.assertTrue(custom[field]["luaScript"].strip().endswith("end"))
        storage = list(yaml.load_all((ROOT / "config/runbook/artifact-store.yaml")
                                    .read_text(encoding="utf-8"), Loader=UniqueLoader))
        self.assertEqual([d["kind"] for d in storage], ["PersistentVolume", "PersistentVolumeClaim"])
        self.assertEqual(storage[0]["spec"]["persistentVolumeReclaimPolicy"], "Retain")
        self.assertEqual(storage[1]["spec"]["volumeName"], storage[0]["metadata"]["name"])

    def test_all_bash_blocks_parse_without_execution(self):
        paths = [ROOT / "README.md"] + sorted((ROOT / "docs").glob("*.md"))
        text = "\n".join(path.read_text(encoding="utf-8") for path in paths)
        blocks = re.findall(r"(?:```|~~~)bash\n(.*?)\n(?:```|~~~)", text, re.S)
        self.assertGreater(len(blocks), 20)
        bash = "C:/msys64/usr/bin/bash.exe" if os.name == "nt" else shutil.which("bash")
        self.assertTrue(bash, "bash required for guide syntax verification")
        for index, code in enumerate(blocks, 1):
            with self.subTest(block=index):
                result = subprocess.run([bash, "-n"], input=code + "\n", text=True,
                                        encoding="utf-8", capture_output=True, check=False)
                self.assertEqual(result.returncode, 0, result.stderr)
        for index, code in enumerate(re.findall(r"python3 - <<'PY'\n(.*?)\nPY", text, re.S), 1):
            compile(code, "guide-python-" + str(index), "exec")


if __name__ == "__main__":
    unittest.main()
