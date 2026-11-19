"""Model selection, mount isolation, and evidence compatibility regressions."""
import copy
import json
from pathlib import Path
import tempfile
import unittest

import run
import collect
import analyze

HERE = Path(__file__).resolve().parent


class ModelRenderTests(unittest.TestCase):
    def setUp(self):
        self.source = {"spec": {"capacity": {"aws": {
            "ami": "ami-test", "subnetId": "subnet-test", "vpcId": "vpc-test",
            "securityGroupIds": ["sg-test"], "credentialsRef": {"name": "aws"}}}}}

    def config(self, model):
        return json.loads((HERE / ("config." + model + ".example.json")).read_text())

    def test_models_both_arms_and_collector(self):
        for model in ("resnet50", "gpt2"):
            for arm in ("A", "B"):
                c = self.config(model)
                info, members, objects, policy = run.render(c, self.source, "model01", "cost", arm)
                sts = next(x for x in objects if x["kind"] == "StatefulSet")
                pod = sts["spec"]["template"]["spec"]
                container = pod["containers"][0]
                env = {e["name"]: e.get("value") for e in container["env"]}
                self.assertEqual(env["EVAL_MODEL"], model)
                self.assertEqual(info["model"], model)
                self.assertEqual(len(info["training_code_sha256"]), 64)
                self.assertEqual(container["resources"]["limits"]["nvidia.com/gpu"], "1")
                script = next(x for x in objects if x["kind"] == "ConfigMap")["data"]
                self.assertIn(Path(container["command"][-1]).name, script)
                self.assertIn("train_resnet18.py", script)
                self.assertEqual(policy["metadata"]["annotations"].get(
                    "training.dcnlab.com/periodic-checkpoint"), "disabled" if arm == "A" else None)
                self.assertEqual(info["scenario"], "constant" if arm == "A" else "risk-schedule")
                if model == "gpt2":
                    self.assertEqual(len(members), 5)
                    self.assertEqual(env["HF_HUB_OFFLINE"], "1")
                    self.assertEqual(env["EVAL_SEQUENCE_LENGTH"], "512")
                    pv = next(x for x in members if x["kind"] == "PersistentVolume"
                              and x["metadata"]["name"].endswith("-model"))
                    self.assertEqual(pv["spec"]["nfs"]["path"], c["nfs_model_path"])
                    self.assertTrue(pv["spec"]["nfs"]["readOnly"])
                    self.assertEqual(pv["spec"]["persistentVolumeReclaimPolicy"], "Retain")
                    self.assertTrue(next(x for x in container["volumeMounts"] if x["name"] == "model")["readOnly"])
                else:
                    self.assertEqual(len(members), 3)
                with tempfile.TemporaryDirectory() as d:
                    run.save(Path(d) / "run.json", info)
                    self.assertEqual(collect.load_run(d)["model"], model)

    def test_invalid_model_paths_and_hyperparameters(self):
        c = self.config("gpt2")
        for key, value in (("model", "gpt3"), ("nfs_model_path", ""), ("nfs_model_path", "/x/../y"),
                           ("nfs_model_path", "REPLACE"), ("sequence_length", 1),
                           ("sequence_length", 1025), ("sequence_length", True),
                           ("learning_rate", float("nan")), ("learning_rate", 0)):
            bad = dict(c, **{key: value})
            with self.assertRaises(ValueError):
                run.validate(bad, "model01", "cost", "B")

    def test_old_config_defaults_to_resnet18(self):
        c = json.loads((HERE / "config.example.json").read_text())
        c.update(nfs_server="host", nfs_dataset_path="/data")
        info, members, objects, _ = run.render(c, self.source, "old01", "cost", "A")
        self.assertEqual(info["model"], "resnet18")
        self.assertEqual(info["learning_rate"], 0.1)
        self.assertEqual(len(members), 3)

    def test_token_metrics_are_allowlisted_and_validated(self):
        event = dict(run_id="model01", event="step", rank=0, global_step=1,
                     timestamp="2026-10-15T00:00:00Z", loss=2.5,
                     samples_seen=8, tokens_seen=4096, loss_tokens_seen=4088,
                     secret="do-not-collect")
        record = collect.event_from_message("EVAL_EVENT " + json.dumps(event))
        self.assertEqual(record["tokens_seen"], 4096)
        self.assertEqual(record["loss"], 2.5)
        self.assertNotIn("secret", record)
        for key, value in (("tokens_seen", -1), ("loss_tokens_seen", True),
                           ("loss", float("nan"))):
            with self.assertRaises(ValueError):
                collect.event_from_message("EVAL_EVENT " + json.dumps(dict(event, **{key: value})))

    def test_compare_rejects_changed_sequence_or_code(self):
        a = dict(run_id="a", experiment="cost", arm="A", phase="completed",
                 all_rank_completion_observed=True, excluded=False, billing_status="validated",
                 currency="USD", total_cost="2", model="gpt2", goal_steps=100,
                 instance_type="g5.xlarge", batch_size=4, seed=42, dataset="wikitext",
                 sequence_length=512, training_code_sha256="abc")
        for field, value in (("sequence_length", 256), ("training_code_sha256", "def"),
                             ("learning_rate", 0.001), ("nfs_model_path", "/other")):
            b = dict(a, run_id="b", arm="B", **{field: value})
            analyze.baseline_savings([copy.deepcopy(a), b], "a")
            self.assertEqual(b["cost_comparison_status"], "configuration_mismatch")


if __name__ == "__main__":
    unittest.main()
