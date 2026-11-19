import json
from pathlib import Path
import tempfile
import unittest

from evaluation.compare_abc import summarize


class CompareABCTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)

    def attempt(self, arm, *, suffix="", status="completed", slot=True, cost=10, seconds=100,
                model="gpt2", workload="a"*64, comparison="b"*64):
        ident = model+arm+suffix
        run = dict(run_id=ident, model=model, arm=arm, world_size=4, goal_steps=4800,
                   workload_lock_sha256=workload, comparison_lock_sha256=comparison,
                   system_lock={"images": "fixture"}, training_code_sha256="e"*64,
                   region="ap-northeast-2", availability_zone="ap-northeast-2c",
                   instance_type="g5.2xlarge", storage_class="nfs-client", nfs_server="fixture",
                   risk_lock_sha256="f"*64, fault_window={"transition_after_seconds": 3600},
                   checkpoint_buffer_reuse=1, group_replacement_batch_size=3)
        metrics = dict(run_id=ident, arm=arm, target_steps=4800,
                       all_rank_completion_observed=status == "completed", final_state_audit_passed=True,
                       T_E2E_seconds=seconds, estimated_gpu_compute_cost_usd=cost)
        for name, value in ((ident+"-run.json", run), (ident+"-metrics.json", metrics)):
            (self.base/name).write_text(json.dumps(value))
        return dict(attempt_id=ident, model=model, arm=arm, block_id="block1", status=status,
                    comparison_slot=slot, run_json=ident+"-run.json", metrics_json=ident+"-metrics.json",
                    failure_reason="test failure" if status == "failed" else None,
                    review=dict(protocol_eligible=True, reviewer="test-fixture", evidence=["fixture-only"]))

    def result(self, attempts):
        return summarize(dict(schema_version=1, attempts=attempts), self.base)

    def test_negative_savings_and_faster_fault_are_not_clipped(self):
        result = self.result([self.attempt("A"), self.attempt("B", cost=12, seconds=150),
                              self.attempt("C", cost=11, seconds=125)])
        contrasts = {x["contrast"]: x for x in result["comparisons"]}
        self.assertAlmostEqual(contrasts["A_vs_B"]["cost_saving_fraction_vs_left"], -.2)
        self.assertEqual(contrasts["B_vs_C"]["delta_time_right_minus_left_seconds"], -25)
        self.assertFalse(result["formal_result"])
        self.assertTrue(all(x["sample_sd"] is None for x in result["descriptives"]))

    def test_failed_slot_not_replaced_and_all_costs_retained(self):
        result = self.result([self.attempt("A"), self.attempt("B", status="failed", cost=3),
                              self.attempt("B", suffix="retry", slot=False, cost=8)])
        self.assertFalse(next(x for x in result["comparisons"] if x["contrast"] == "A_vs_B")["available"])
        self.assertEqual(result["all_attempt_cost_accounting_by_model"]["gpt2"]["observed_attempt_cost_usd"], 21)
        self.assertEqual(len(result["attempts"]), 3)

    def test_missing_cost_not_zero_and_contract_drift_rejected(self):
        result = self.result([self.attempt("A"), self.attempt("B", cost=None), self.attempt("C", comparison="c"*64)])
        self.assertTrue(all(not x["available"] for x in result["comparisons"] if x["contrast"] in {"A_vs_B", "B_vs_C"}))
        self.assertEqual(result["all_attempt_cost_accounting_by_model"]["gpt2"]["executed_attempts_missing_cost"], 1)
        result = self.result([self.attempt("B"), self.attempt("C", comparison="c"*64)])
        self.assertEqual(next(x for x in result["comparisons"] if x["contrast"] == "B_vs_C")["reason"], "B_C_policy_risk_checkpoint_contract_mismatch")

    def test_goal_and_workload_drift_ineligible(self):
        a, b = self.attempt("A"), self.attempt("B", workload="c"*64)
        result = self.result([a, b])
        self.assertEqual(result["comparisons"][0]["reason"], "workload_contract_mismatch")
        path = self.base/b["metrics_json"]
        metrics = json.loads(path.read_text()); metrics["target_steps"] = 512
        path.write_text(json.dumps(metrics))
        result = self.result([b])
        self.assertIn("goal_contract_missing_or_mismatched", result["attempts"][0]["ineligibility_reasons"])

    def test_duplicate_slot_is_error_and_models_are_separate(self):
        with self.assertRaisesRegex(ValueError, "duplicate preassigned"):
            self.result([self.attempt("A"), self.attempt("A", suffix="retry")])
        result = self.result([self.attempt("A"), self.attempt("A", model="resnet50")])
        self.assertEqual(len(result["all_attempt_cost_accounting_by_model"]), 2)
        self.assertTrue(all(row["n"] == 1 for row in result["descriptives"]))

    def test_planned_template_never_looks_like_a_measured_result(self):
        result = self.result([dict(attempt_id="futureA", model="gpt2", arm="A", block_id="block1",
                                   status="planned", comparison_slot=True)])
        self.assertFalse(result["attempts"][0]["eligible_for_descriptive_comparison"])
        self.assertIsNone(result["attempts"][0]["estimated_gpu_compute_cost_usd"])
        self.assertEqual(result["all_attempt_cost_accounting_by_model"]["gpt2"]["executed_attempts"], 0)

    def test_common_system_drift_between_a_and_b_is_not_hidden_by_treatment(self):
        a, b = self.attempt("A"), self.attempt("B")
        path = self.base/b["run_json"]
        run = json.loads(path.read_text()); run["system_lock"] = {"images": "changed"}
        path.write_text(json.dumps(run))
        result = self.result([a, b])
        self.assertEqual(result["comparisons"][0]["reason"], "common_system_risk_storage_contract_mismatch")


if __name__ == "__main__":
    unittest.main()
