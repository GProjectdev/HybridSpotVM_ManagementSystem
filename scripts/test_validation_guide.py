import os
from pathlib import Path
import re
import shutil
import subprocess
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
