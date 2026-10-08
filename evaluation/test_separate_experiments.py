from pathlib import Path
import json
import subprocess
import sys
import tempfile
import unittest

HERE = Path(__file__).resolve().parent


class SeparateExperimentsTests(unittest.TestCase):
    def invoke(self, experiment, *args):
        return subprocess.run([sys.executable, str(HERE / (experiment + ".py")), *args],
                              capture_output=True, text=True)

    def test_arm_boundaries(self):
        for experiment, arm in (("cost", "D"), ("checkpoint", "A")):
            result = self.invoke(experiment, "render", "--config", "unused.json",
                                 "--run-id", "test", "--arm", arm, "--out", "unused")
            self.assertEqual(result.returncode, 2)
            self.assertIn("invalid choice", result.stderr)

    def test_wrong_evidence_rejected_before_cluster_access(self):
        with tempfile.TemporaryDirectory() as directory:
            for experiment, other in (("cost", "checkpoint"), ("checkpoint", "cost")):
                (Path(directory) / "run.json").write_text(json.dumps({"experiment": other}))
                for command in ("prepare", "start", "stop", "cleanup", "risk"):
                    extra = ["--confirm", "test"] if command == "cleanup" else []
                    if command == "risk":
                        extra = ["--value", "0.8"]
                    result = self.invoke(experiment, command, "--evidence", directory, *extra)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("different experiment", result.stderr)

    def test_help_is_experiment_specific(self):
        for experiment, arms in (("cost", "{A,B}"), ("checkpoint", "{F60,F300,F600,D}")):
            result = self.invoke(experiment, "render", "--help")
            self.assertEqual(result.returncode, 0)
            self.assertIn(arms, result.stdout)
            self.assertNotIn("--experiment", result.stdout)
