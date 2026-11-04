import copy
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

import e3_report as e3


class E3ReportTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.run = {"run_id": "e3-test", "phase": "measured", "experiment": "cost", "arm": "B",
                    "e3": {"arm": "C"}, "measurement_started_at": self.at(10),
                    "measurement_ended_at": self.at(50), "analytic_interval_observed": True,
                    "risk_schedule_observations": [{"after_seconds": 20, "lambda_per_hour": 0.5,
                        "requested_at": self.at(30), "consumed_at": self.at(31), "secret": "CREDENTIAL"}]}
        self.save("run.json", self.run)
        self.save("e3-fault.json", {"outcome": "requested_not_yet_verified", "secret": "CREDENTIAL"})
        self.events = [self.event(rank, second, step) for rank in (0, 1)
                       for second, step in ((10, 100), (20, 200), (40, 130), (50, 150), (55, 500))]
        self.write_events()

    @staticmethod
    def at(second):
        return "2026-11-04T00:00:%02dZ" % second

    def event(self, rank, second, step):
        return {"timestamp": self.at(second), "rank": rank, "global_step": step, "run_id": "e3-test", "event": "step",
                "tokens_seen": step * 100, "attempt_id": "old" if second < 40 else "new"}

    def save(self, name, value):
        (self.root / name).write_text(json.dumps(value), encoding="utf-8")

    def write_events(self):
        (self.root / "monitor-events.jsonl").write_text(
            "\n".join(json.dumps(event) for event in self.events), encoding="utf-8")

    def test_latest_not_maximum_and_window_bounds(self):
        self.events.reverse()
        self.write_events()
        result = e3.report(self.root)
        self.assertEqual(result["arm"], "C")
        self.assertEqual(result["useful_progress"], {"global_step": 50, "tokens_seen": 5000})
        self.assertEqual(result["rank_progress"]["0"]["latest"]["attempt_id"], "new")
        self.assertIsNone(result["cost"])
        self.assertEqual(result["verdict"], "not_evaluated")
        self.assertEqual(result["comparison_validity"], "not_evaluated")
        self.assertEqual(result["runner_phase"], "measured")

    def test_baseline_is_latest_not_historical_maximum(self):
        self.events += [self.event(rank, 1, 1000) for rank in (0, 1)]
        self.write_events()
        self.assertEqual(e3.report(self.root)["useful_progress"]["global_step"], 50)

    def test_missing_baseline_and_rank_never_assume_zero(self):
        for missing in ("baseline", "rank"):
            with self.subTest(missing=missing):
                self.events = [self.event(rank, second, 150) for rank in (0, 1)
                               for second in (10, 50) if not
                               (missing == "baseline" and second == 10 or missing == "rank" and rank == 1)]
                self.write_events()
                result = e3.report(self.root)
                self.assertIsNone(result["useful_progress"]["global_step"])
                self.assertTrue(any("missing baseline" in w for w in result["warnings"]))

    def test_negative_progress_retained(self):
        self.events = [self.event(rank, second, step) for rank in (0, 1)
                       for second, step in ((10, 100), (50, 50))]
        self.write_events()
        self.assertEqual(e3.report(self.root)["useful_progress"]["global_step"], -50)

    def test_rank_minimum_and_boundary_gaps(self):
        self.events = [self.event(rank, second, step) for rank in (0, 1)
                       for second, step in ((9, 100), (49, 150 + rank))]
        self.write_events()
        result = e3.report(self.root)
        self.assertEqual(result["useful_progress"]["global_step"], 50)
        self.assertTrue(any("boundary log gap" in w for w in result["warnings"]))
        self.assertTrue(any("counters differ" in w for w in result["warnings"]))

    def test_malformed_foreign_and_conflicting_events(self):
        self.events += [dict(self.event(0, 50, 900), run_id="other")]
        self.write_events()
        with (self.root / "monitor-events.jsonl").open("a", encoding="utf-8") as stream:
            stream.write('\n{bad\n[]\n')
        result = e3.report(self.root)
        self.assertEqual(result["useful_progress"]["global_step"], 50)
        self.assertTrue(any("invalid monitor event" in w for w in result["warnings"]))
        self.events.append(self.event(0, 50, 151))
        self.write_events()
        self.assertIsNone(e3.report(self.root)["useful_progress"]["global_step"])

    def test_missing_files_still_report_no_pass(self):
        (self.root / "monitor-events.jsonl").unlink()
        (self.root / "e3-fault.json").unlink()
        result = e3.report(self.root)
        self.assertIsNone(result["useful_progress"]["tokens_seen"])
        self.assertIsNone(result["e3_fault"])
        self.assertEqual(result["verdict"], "not_evaluated")

    def test_cost_integrates_clipped_lifecycles(self):
        self.save("ledger.json", [
            {"instance_id": "i-one", "market": "spot", "start": self.at(0), "end": self.at(30), "usd_per_hour": 3.6},
            {"instance_id": "i-two", "market": "on-demand", "start": self.at(30), "end": self.at(59), "usd_per_hour": 7.2},
            {"instance_id": "i-three", "market": "spot", "start": self.at(0), "end": self.at(5), "usd_per_hour": 100}])
        result = e3.report(self.root, self.root / "ledger.json")["cost"]
        self.assertAlmostEqual(result["estimated_usd"], 0.06)
        self.assertAlmostEqual(result["window_estimated_usd"], 0.06)
        lifecycle = 30 * 3.6 / 3600 + 29 * 7.2 / 3600 + 5 * 100 / 3600
        self.assertAlmostEqual(result["lifecycle_estimated_usd"], lifecycle)
        self.assertAlmostEqual(result["usd_per_useful_million_tokens"], lifecycle * 1000000 / 5000)
        self.assertAlmostEqual(result["window_usd_per_useful_million_tokens"], 12)
        self.assertEqual(result["useful_tokens_denominator"], 5000)
        self.assertEqual([row["lifecycle_seconds"] for row in result["instances"]], [30, 29, 5])
        self.assertEqual([row["window_seconds"] for row in result["instances"]], [20, 20, 0])
        self.assertFalse(result["ledger_completeness_verified"])
        self.assertIn("Estimate", result["label"])

    def test_cost_rejects_duplicates_and_invalid_fields(self):
        valid = {"instance_id": "i-one", "market": "spot", "start": self.at(0),
                 "end": self.at(59), "usd_per_hour": 1}
        cases = [[valid, valid], {}, [None]]
        for rate in (-1, float("nan"), float("inf"), True, "1", None):
            cases.append([dict(valid, usd_per_hour=rate)])
        for field, value in (("end", self.at(0)), ("start", "invalid"), ("market", "unknown"),
                             ("instance_id", "../secret")):
            cases.append([dict(valid, **{field: value})])
        for rows in cases:
            with self.subTest(rows=rows):
                self.save("ledger.json", rows)
                with self.assertRaises(ValueError):
                    e3.report(self.root, self.root / "ledger.json")

    def test_empty_ledger_is_explicit_unverified_zero(self):
        self.save("ledger.json", [])
        cost = e3.report(self.root, self.root / "ledger.json")["cost"]
        self.assertEqual(cost["estimated_usd"], 0)
        self.assertFalse(cost["ledger_completeness_verified"])

    def test_archive_allowlist_hashes_and_final_projection(self):
        self.run["config"] = {"credentials": "CREDENTIAL"}
        self.save("run.json", self.run)
        self.save("policy.yaml", {"secret": "CREDENTIAL"})
        final = {"observed_at": self.at(50), "fis": {"state": "running", "secret": "CREDENTIAL"},
                 "pods": [{"uid": "pod-1", "rank": 0, "env": {"TOKEN": "CREDENTIAL"}}],
                 "resources": [{"kind": "RestoreRequest", "metadata": {"uid": "restore-1",
                     "annotations": {"token": "CREDENTIAL"}}, "spec": {"credentials": "CREDENTIAL"},
                     "status": {"phase": "Verified"}}]}
        self.save("e3-final.json", final)
        original = (self.root / "e3-final.json").read_bytes()
        result = e3.report(self.root)
        self.assertTrue(result["final_snapshot_available"])
        self.assertEqual((self.root / "e3-final.json").read_bytes(), original)
        with tarfile.open(self.root / "e3-evidence.tar.gz") as tar:
            self.assertEqual(set(tar.getnames()), {"e3-summary.json", "e3-summary.md", "e3-manifest.json", "e3-final.json",
                                                  "e3-plan.json", "e3-monitor-events.jsonl"})
            manifest = json.load(tar.extractfile("e3-manifest.json"))
            for name, digest in manifest["files"].items():
                data = tar.extractfile(name).read()
                self.assertEqual(hashlib.sha256(data).hexdigest(), digest)
                self.assertNotIn(b"CREDENTIAL", data)
            projected = json.load(tar.extractfile("e3-final.json"))
            self.assertEqual(projected["pods"][0]["uid"], "pod-1")
        digest = (self.root / "e3-evidence.tar.gz.sha256").read_text().split()[0]
        self.assertEqual(digest, hashlib.sha256((self.root / "e3-evidence.tar.gz").read_bytes()).hexdigest())
        self.assertTrue(result["analytic_interval_observed"])
        self.assertNotIn("secret", result["risk_schedule_observations"][0])

    def test_fault_outcomes_are_observations_not_success(self):
        for outcome in ("od_control_no_fault", "no_spot_exposure", "requested_not_yet_verified", "request_intent"):
            self.save("e3-fault.json", {"outcome": outcome})
            result = e3.report(self.root)
            self.assertEqual(result["e3_fault"]["outcome"], outcome)
            self.assertEqual(result["comparison_validity"], "not_evaluated")

    def test_final_snapshot_matches_runner_resource_map(self):
        self.save("e3-final.json", {"resources": {"aws/pods": [{"metadata": {"uid": "pod-1"},
            "spec": {"nodeName": "node-1", "containers": [{"env": "CREDENTIAL"}]}}],
            "karmada/restorerequests": [{"status": {"phase": "Verified"}}],
            "private/config": {"secret": "CREDENTIAL"}},
            "fis": {"id": "EXP1", "state": {"status": "completed"}, "actions": {
                "interrupt": {"actionId": "aws:ec2:send-spot-instance-interruptions", "state": {"status": "completed"},
                              "parameters": {"token": "CREDENTIAL"}}}},
            "target_ec2": [{"id": "i-test", "state": "terminated"}], "errors": ["aws/pods:RuntimeError"]})
        result = e3.report(self.root)
        self.assertTrue(any("collection errors" in w for w in result["warnings"]))
        with tarfile.open(self.root / "e3-evidence.tar.gz") as tar:
            data = tar.extractfile("e3-final.json").read()
        self.assertNotIn(b"CREDENTIAL", data)
        final = json.loads(data)
        self.assertEqual(final["resources"]["aws/pods"][0]["spec"], {"nodeName": "node-1"})
        self.assertEqual(final["fis"]["actions"][0]["state"]["status"], "completed")
        self.assertEqual(final["target_ec2"][0]["state"], "terminated")

    def test_invalid_window_fails_and_nested_iso_offsets_work(self):
        for end in (self.at(10), self.at(5), "2026-11-04T00:00:50", None):
            run = copy.deepcopy(self.run)
            run["measurement_ended_at"] = end
            self.save("run.json", run)
            with self.assertRaises(ValueError):
                e3.report(self.root)
        self.run["e3"].update(measurement_started_at="2026-11-04T09:00:10+09:00",
                              measurement_ended_at="2026-11-04T09:00:50+09:00")
        self.save("run.json", self.run)
        self.assertEqual(e3.report(self.root)["measurement_seconds"], 40)

    def test_cli_outputs_and_rerun(self):
        command = [sys.executable, str(Path(e3.__file__)), "--evidence", str(self.root)]
        result = subprocess.run(command, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / "e3-summary.md").exists())
        e3.report(self.root)
        self.save("ledger.json", [{"bad": "CREDENTIAL"}])
        result = subprocess.run(command + ["--cost-ledger", str(self.root / "ledger.json")], capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertNotIn("CREDENTIAL", result.stderr)

    def test_cost_ratio_requires_positive_useful_tokens(self):
        self.save("ledger.json", [{"instance_id": "i-one", "market": "spot", "start": self.at(0),
                                   "end": self.at(59), "usd_per_hour": 1}])
        for tokens in (0, -100, None):
            cost = e3.estimate_cost(self.root / "ledger.json", e3.timestamp(self.at(10)),
                                    e3.timestamp(self.at(50)), tokens)
            self.assertIsNone(cost["usd_per_useful_million_tokens"])
            self.assertIsNone(cost["window_usd_per_useful_million_tokens"])
            self.assertGreater(cost["lifecycle_estimated_usd"], cost["window_estimated_usd"])

    def test_plan_and_risk_projection_retains_controls_not_strings(self):
        self.save("e3-plan.json", {"timing_rationale": "CREDENTIAL", "window_seconds": 40})
        original_plan = (self.root / "e3-plan.json").read_bytes()
        self.run["e3"].update(window_seconds=40, fixed_interval_seconds=300, alpha=0.05,
                              pilot=False, plan_sha256="a" * 64, timing_rationale="CREDENTIAL",
                              initial_risk=float("nan"), changed_risk="CREDENTIAL", fault_after_seconds=True)
        observation = self.run["risk_schedule_observations"][0]
        observation["applied_at"] = "CREDENTIAL"
        observation["actual_after_seconds"] = 20.5
        observation["decision"] = {"onDemandWorkers": 2, "spotWorkers": 0, "intervalSeconds": 60,
                                   "lambdaPerHour": 0.5, "message": "CREDENTIAL", "alpha": True,
                                   "targetWorkers": "CREDENTIAL", "minIntervalSeconds": float("inf")}
        self.save("run.json", self.run)
        result = e3.report(self.root)
        self.assertEqual(result["plan"], {"window_seconds": 40, "fixed_interval_seconds": 300,
                                         "alpha": 0.05, "pilot": False, "plan_sha256": "a" * 64})
        self.assertEqual((self.root / "e3-plan.json").read_bytes(), original_plan)
        observed = result["risk_schedule_observations"][0]
        self.assertEqual(observed["requested_at"], "2026-11-04T00:00:30+00:00")
        self.assertNotIn("applied_at", observed)
        self.assertEqual(observed["actual_after_seconds"], 20.5)
        self.assertEqual(observed["decision"], {"onDemandWorkers": 2, "spotWorkers": 0,
                                               "intervalSeconds": 60, "lambdaPerHour": 0.5})
        with tarfile.open(self.root / "e3-evidence.tar.gz") as tar:
            self.assertEqual(json.load(tar.extractfile("e3-plan.json")), result["plan"])
        self.assertEqual(e3.plan_projection({"pilot": "true", "plan_sha256": "bad"}), {})

    def test_monitor_audit_filters_and_keeps_restore_receipts(self):
        valid = dict(self.event(0, 40, 130), event="restore_audit", passed=True,
                     gradients_finite=True, input_sha256="a" * 64, secret="CREDENTIAL")
        self.events += [valid, dict(valid, run_id="foreign"), dict(valid, event="unknown"),
                        dict(valid, passed="CREDENTIAL"), {"secret": "CREDENTIAL"}, None]
        self.write_events()
        result = e3.report(self.root)
        with tarfile.open(self.root / "e3-evidence.tar.gz") as tar:
            raw = tar.extractfile("e3-monitor-events.jsonl").read()
        self.assertNotIn(b"CREDENTIAL", raw)
        events = [json.loads(line) for line in raw.splitlines()]
        self.assertEqual(len(events), 11)
        self.assertEqual(result["monitor_audit_event_count"], 11)
        audit = next(event for event in events if event["event"] == "restore_audit")
        self.assertTrue(audit["passed"])
        self.assertEqual(audit["input_sha256"], "a" * 64)
        self.assertTrue(any("monitor audit skipped" in warning for warning in result["warnings"]))

    def test_collector_snapshot_json_and_jsonl_projection(self):
        snapshot = {"timestamp": self.at(30), "run_id": "e3-test", "secret": "CREDENTIAL", "resources": [
            {"cluster": "karmada", "resource": "trainingpolicies", "items": [{"kind": "TrainingPolicy",
             "metadata": {"uid": "policy-1", "annotations": {"private": "CREDENTIAL"}},
             "spec": {"credentialsRef": {"name": "CREDENTIAL"}},
             "status": {"policy": {"onDemandWorkers": 2, "spotWorkers": 0},
                        "checkpoint": {"intervalSeconds": 60}, "message": "CREDENTIAL"}}]}]}
        (self.root / "snapshots").mkdir()
        self.save("snapshots/one.json", snapshot)
        (self.root / "snapshots.jsonl").write_text("\n".join([
            json.dumps(snapshot), json.dumps(dict(snapshot, run_id="other")), "{bad", "[]",
            json.dumps(dict(snapshot, resources={"unknown": "CREDENTIAL"}))]), encoding="utf-8")
        original = (self.root / "snapshots.jsonl").read_bytes()
        result = e3.report(self.root)
        self.assertEqual(result["snapshot_audit_count"], 2)
        self.assertEqual((self.root / "snapshots.jsonl").read_bytes(), original)
        self.assertTrue(any("snapshot skipped" in w for w in result["warnings"]))
        with tarfile.open(self.root / "e3-evidence.tar.gz") as tar:
            raw = tar.extractfile("e3-snapshots.jsonl").read()
            manifest = json.load(tar.extractfile("e3-manifest.json"))
            self.assertEqual(hashlib.sha256(raw).hexdigest(), manifest["files"]["e3-snapshots.jsonl"])
        self.assertNotIn(b"CREDENTIAL", raw)
        rows = [json.loads(line) for line in raw.splitlines()]
        self.assertEqual(rows[0]["resources"][0]["items"][0]["status"]["checkpoint"]["intervalSeconds"], 60)

    def test_unknown_snapshot_format_is_skipped_explicitly(self):
        self.save("snapshots.jsonl", {"unknown": "CREDENTIAL"})
        result = e3.report(self.root)
        self.assertEqual(result["snapshot_audit_count"], 0)
        self.assertTrue(any("unknown/malformed schema" in w for w in result["warnings"]))
        with tarfile.open(self.root / "e3-evidence.tar.gz") as tar:
            self.assertNotIn("e3-snapshots.jsonl", tar.getnames())


if __name__ == "__main__":
    unittest.main()
