import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("install_system", Path(__file__).with_name("install-system.py"))
INSTALL = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(INSTALL)


class InstallTests(unittest.TestCase):
    def documents(self, stage):
        return [obj for path in INSTALL.STAGES[stage] for obj in INSTALL.load_documents(INSTALL.ROOT / path)]

    def test_all_stages_images(self):
        for stage in INSTALL.STAGES:
            result = INSTALL.render(self.documents(stage), "registry.example.org/team/system", "test", "pull-auth")
            for obj in result["items"]:
                if obj["kind"] in ("Deployment", "DaemonSet"):
                    pod = obj["spec"]["template"]["spec"]
                    self.assertEqual(pod["containers"][0]["image"],
                                     "registry.example.org/team/system:" + obj["metadata"]["name"] + "-test")
                    self.assertEqual(pod["imagePullSecrets"], [{"name": "pull-auth"}])

    def test_non_aws_member_excludes_watcher(self):
        self.assertFalse(any(o["kind"] == "DaemonSet" for o in self.documents("member")))

    def test_management_does_not_register_webhook(self):
        self.assertFalse(any("WebhookConfiguration" in o["kind"] for o in self.documents("management")))

    def test_images_required_and_input_unchanged(self):
        docs = self.documents("management")
        before = repr(docs)
        with self.assertRaises(ValueError):
            INSTALL.render(docs, None, None)
        INSTALL.render(docs, "registry.example.org/team/system", "test")
        self.assertEqual(before, repr(docs))

    def test_default_is_offline(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(sys, "argv", ["install-system.py", "crds", "--out", directory + "/crds.json"]):
                with patch.object(subprocess, "run", side_effect=AssertionError("cluster write")):
                    with patch.object(subprocess, "check_output", side_effect=AssertionError("cluster read")):
                        INSTALL.main()
            self.assertEqual(len(json.loads(Path(directory, "crds.json").read_text())["items"]), 5)

    def test_apply_requires_explicit_target(self):
        with patch.object(sys, "argv", ["install-system.py", "crds", "--out", "unused", "--apply"]):
            with self.assertRaises(SystemExit):
                INSTALL.main()

    def test_wrong_context_never_applies(self):
        with tempfile.TemporaryDirectory() as directory:
            argv = ["install-system.py", "crds", "--out", directory + "/crds.json",
                    "--apply", "--kubeconfig", "test.config", "--confirm-context", "expected"]
            with patch.object(sys, "argv", argv), patch.object(subprocess, "check_output", return_value="wrong\n"):
                with patch.object(subprocess, "run") as run:
                    with self.assertRaises(ValueError):
                        INSTALL.main()
                    run.assert_not_called()

    def test_apply_waits_for_crds_and_pins_context(self):
        with tempfile.TemporaryDirectory() as directory:
            argv = ["install-system.py", "crds", "--out", directory + "/crds.json",
                    "--apply", "--kubeconfig", "test.config", "--confirm-context", "expected"]
            with patch.object(sys, "argv", argv), patch.object(subprocess, "check_output", return_value="expected\n"):
                with patch.object(subprocess, "run") as run:
                    INSTALL.main()
                    self.assertEqual(run.call_count, 6)
                    for call in run.call_args_list:
                        self.assertEqual(call.args[0][:5],
                                         ["kubectl", "--kubeconfig", "test.config", "--context", "expected"])
                        self.assertTrue(call.kwargs["check"])

    def test_unavailable_file_is_error(self):
        with self.assertRaises(ValueError):
            INSTALL.load_documents(INSTALL.ROOT / "absent.yaml")

    def test_unknown_transforms_are_not_silently_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "kustomization.yaml")
            path.write_text("kind: Kustomization\nresources: []\nnamespace: other\n")
            with self.assertRaises(ValueError):
                INSTALL.load_documents(path)


if __name__ == "__main__":
    unittest.main()
