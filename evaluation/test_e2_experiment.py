import copy
import json
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import e2_experiment as e2
import test_e1_experiment


class E2Tests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.out = self.root / "evidence"
        source = {"spec": {"capacity": {"aws": {"ami": "ami-test", "subnetId": "subnet-test",
                  "vpcId": "vpc-test", "securityGroupIds": ["sg-test"], "credentialsRef": {"name": "aws"}}}}}
        e2.runner.save(self.root / "source.json", source)
        self.args = SimpleNamespace(config=Path(__file__).parent / "config.gpt2.example.json",
                                    policy_source=self.root / "source.json", run_id="e2-test", out=self.out,
                                    goal_steps=200, max_seconds=7200, resume_steps=10)
        e2.render(self.args)

    def test_offline_render_fixed_composition(self):
        value = e2.info(self.out)
        self.assertEqual(value["scenario"], "interruption")
        self.assertNotIn("risk_schedule", value)
        self.assertTrue(value["restore_audit"])
        policy = e2.runner.read(self.out / "policy.yaml")
        self.assertEqual(policy["spec"]["policy"]["fixedOnDemand"], 1)
        self.assertEqual(policy["spec"]["policy"]["alpha"], 0.05)
        self.assertEqual(policy["spec"]["sourceCluster"], "aws")
        self.assertEqual(policy["spec"]["checkpoint"]["minIntervalSeconds"], 300)
        with self.assertRaises(FileExistsError):
            e2.render(self.args)

    def test_wrong_run_or_scope_rejected(self):
        with self.assertRaises(ValueError):
            e2.info(self.out, "other")
        value = e2.info(self.out)
        value["cluster"] = "onpre1"
        e2.runner.save(self.out / "run.json", value)
        with self.assertRaises(ValueError):
            e2.info(self.out)

    def test_failed_checkpoint_gate_cannot_inject(self):
        args = ["e2_experiment.py", "interrupt", "--evidence", str(self.out), "--confirm", "e2-test"]
        with patch("sys.argv", args), patch.object(e2, "checkpoint_gate", side_effect=ValueError("not ready")), \
                patch.object(e2.fis, "start") as inject:
            with self.assertRaises(ValueError):
                e2.main()
            inject.assert_not_called()

    def test_render_enables_audit_environment(self):
        objects = e2.runner.read(self.out / "karmada.yaml")["items"]
        sts = next(o for o in objects if o["kind"] == "StatefulSet")
        env = sts["spec"]["template"]["spec"]["containers"][0]["env"]
        self.assertIn({"name": "EVAL_RESTORE_AUDIT", "value": "1"}, env)

    def test_completion_waits_for_fis_and_verified_group(self):
        value = e2.info(self.out)
        with patch.object(e2.runner, "verify_scenario") as verify:
            self.assertFalse(e2.completion_check(self.out, value))
            verify.assert_not_called()
        e2.runner.save(self.out / "fis-started.json", {})
        with patch.object(e2, "recovered_ranks", return_value=([], {"0": 10, "1": 10})), \
                patch.object(e2.runner, "verify_scenario", side_effect=RuntimeError("pending")):
            self.assertFalse(e2.completion_check(self.out, value))
        with patch.object(e2, "recovered_ranks", return_value=([], {"0": 10, "1": 10})), \
                patch.object(e2.runner, "verify_scenario"):
            self.assertTrue(e2.completion_check(self.out, value))
        with patch.object(e2, "recovered_ranks", return_value=([], {"0": 10, "1": 11})), \
                patch.object(e2.runner, "verify_scenario") as verify:
            self.assertFalse(e2.completion_check(self.out, value))
            verify.assert_not_called()

    def test_gate_requires_current_full_durable_checkpoint(self):
        value = e2.info(self.out)
        value.update(policy_uid="policy", workload_uid="workload")
        e2.runner.save(self.out / "run.json", value)
        cp = test_e1_experiment.E1ExperimentTests().checkpoint()
        cp["metadata"]["annotations"] = {"training.dcnlab.com/checkpoint-id": "round"}
        cp["metadata"]["labels"] = {e2.runner.POLICY_UID: "policy"}
        cp["spec"] = {"workloadRef": {"uid": "workload"}}
        pods = {"items": [{"metadata": {"uid": u}, "status": {"phase": "Running"}} for u in ("u0", "u1")]}
        with patch.object(e2.runner, "owned"), patch.object(e2.runner, "get", side_effect=[pods, {"items": [cp]}]):
            e2.checkpoint_gate(self.out)
        saved = e2.runner.read(self.out / "e2-checkpoint.json")
        self.assertIn("durableRef", saved["status"]["pods"][0]["checkpointFiles"][0])
        for broken in ("uid", "digest", "workload", "policy"):
            bad = copy.deepcopy(cp)
            if broken == "uid":
                bad["status"]["pods"][0]["podUID"] = "old"
            elif broken == "digest":
                bad["status"]["pods"][0]["checkpointFiles"][0]["sha256"] = "invalid"
            elif broken == "workload":
                bad["spec"]["workloadRef"]["uid"] = "other"
            else:
                bad["metadata"]["labels"][e2.runner.POLICY_UID] = "other"
            with patch.object(e2.runner, "owned"), patch.object(e2.runner, "get", side_effect=[pods, {"items": [bad]}]):
                with self.assertRaises(ValueError):
                    e2.checkpoint_gate(self.out)

    def test_scheduled_children_and_retained_full_result(self):
        value = {"policy_uid": "policy", "workload_uid": "workload"}
        parent = {"metadata": {"uid": "parent", "name": "periodic", "namespace": "ns",
                  "labels": {e2.runner.POLICY_UID: "policy"}},
                  "spec": {"workloadRef": {"uid": "workload"}}}
        child = test_e1_experiment.E1ExperimentTests().checkpoint()
        child["metadata"].update(name="child", namespace="ns",
            labels={"training.dcnlab.com/scheduled-parent": "parent"},
            annotations={"training.dcnlab.com/checkpoint-id": "round",
                         "training.dcnlab.com/scheduled-parent-uid": "parent",
                         "training.dcnlab.com/scheduled-parent-name": "periodic"})
        child["spec"] = copy.deepcopy(parent["spec"])
        del child["status"]["observedGeneration"]
        child["status"]["conditions"] = [{"type": "Ready", "status": "True",
            "reason": "CheckpointCompleted", "observedGeneration": 1}]
        candidates = list(e2.checkpoint_candidates([parent, child], value))
        self.assertTrue(any(e2.durable_world(c, ["u0", "u1"], "round") for c in candidates))
        self.assertNotIn("observedGeneration", child["status"])
        for mutation in ("parent_uid", "parent_policy", "workload", "condition"):
            p, c = copy.deepcopy(parent), copy.deepcopy(child)
            if mutation == "parent_uid":
                p["metadata"]["uid"] = "recreated"
            elif mutation == "parent_policy":
                p["metadata"]["labels"][e2.runner.POLICY_UID] = "other"
            elif mutation == "workload":
                c["spec"]["workloadRef"]["uid"] = "other"
            else:
                c["status"]["conditions"][0]["observedGeneration"] = 0
            self.assertFalse(any(e2.durable_world(x, ["u0", "u1"], "round")
                for x in e2.checkpoint_candidates([p, c], value)), mutation)
        status = copy.deepcopy(child["status"])
        status["observedGeneration"] = 1
        parent["status"] = {"lastSuccessfulFullCheckpoint": {
            "uid": "child", "name": "child", "phase": "Completed", "checkpointID": "round",
            "completionTime": status["completionTime"],
            "result": {"spec": child["spec"], "status": status}}}
        self.assertTrue(any(e2.durable_world(x, ["u0", "u1"], "round")
            for x in e2.checkpoint_candidates([parent], value)))
        status["pods"][0]["podUID"] = "old"
        self.assertFalse(any(e2.durable_world(x, ["u0", "u1"], "round")
            for x in e2.checkpoint_candidates([parent], value)))

    def test_incomplete_report_not_success_and_export_allowlist(self):
        (self.out / "private.txt").write_text("CREDENTIAL", encoding="utf-8")
        (self.out / "presentation").mkdir()
        (self.out / "presentation" / "secret.txt").write_text("CREDENTIAL", encoding="utf-8")
        e2.report(self.out)
        summary = e2.runner.read(self.out / "presentation" / "summary.json")
        self.assertFalse(summary["candidate_pass"])
        archive = self.root / "e2-test-e2-presentation.tar.gz"
        with tarfile.open(archive) as tar:
            self.assertFalse(any("secret" in n or "policy.yaml" in n or "private" in n for n in tar.getnames()))
            self.assertIn("e2-test-e2", archive.name)


if __name__ == "__main__":
    unittest.main()
