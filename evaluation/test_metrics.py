"""Offline tests: python -B -m unittest discover -s evaluation -p test_metrics.py."""

import contextlib
import copy
import csv
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

try:
    from . import analyze, collect
except ImportError:
    import analyze
    import collect


class MetricsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.run = {"namespace": collect.NAMESPACE, "run_id": "run-1", "experiment": "cost", "arm": "A",
                    "model": "resnet18", "goal_steps": 3, "app": "eval-run-1", "policy": "eval-run-1",
                    "risk": "eval-run-1-risk", "region": collect.REGION, "workload_uid": "workload-1",
                    "started_at": "2026-10-08T00:00:00Z"}
        collect.atomic_json(self.path / "run.json", self.run)

    def log(self, step, at, event="step", rank=0, run_id="run-1"):
        return at + " " + collect.PREFIX + json.dumps({"timestamp": at, "run_id": run_id,
                   "event": event, "rank": rank, "global_step": step}) + "\n"

    def checkpoint(self, uid="checkpoint-1", phase="Completed"):
        return {"metadata": {"namespace": collect.NAMESPACE, "uid": uid, "generation": 1,
                              "labels": {collect.POLICY_LABEL: self.run["policy"]},
                              "annotations": {"training.dcnlab.com/checkpoint-id": "round-1"}},
                "spec": {"workloadRef": {"name": self.run["app"], "uid": "workload-1"}},
                "status": {"phase": phase, "observedGeneration": 1,
                           "startTime": "2026-10-08T00:00:02Z", "completionTime": "2026-10-08T00:00:05Z"}}

    def snapshot(self, objects, nodes=None, name="one"):
        resources = [{"cluster": "aws", "resource": "fluidcrmigrations", "items": objects}]
        if nodes is not None:
            resources.append({"cluster": "aws", "resource": "nodeprovisions", "items": nodes})
        collect.atomic_json(self.path / "snapshots" / (name + ".json"),
                            {"run_id": "run-1", "timestamp": "2026-10-08T00:01:00Z", "resources": resources})

    def ledger(self, rows):
        with (self.path / "billing.csv").open("w", encoding="utf-8", newline="") as handle:
            writer = csv.DictWriter(handle, fieldnames=["run_id", "cost_type", "instance_id", "start", "end",
                                                       "price_per_hour", "currency", "amount", "description"])
            writer.writeheader()
            writer.writerows(rows)

    def vm_row(self, **overrides):
        return {"run_id": "run-1", "cost_type": "vm", "instance_id": "i-12345678",
                "start": "2026-10-08T00:00:00Z", "end": "2026-10-08T01:30:00Z",
                "price_per_hour": "2", "currency": "USD", **overrides}

    def test_missing_metrics_are_blank(self):
        result = analyze.summarize(self.path)
        for key in ("observed_attempted_steps", "observed_completed_checkpoints", "vm_cost", "extra_cost",
                    "total_cost", "max_global_step", "goal_completed", "time_to_goal_seconds"):
            self.assertEqual(result[key], "", key)

    def test_config_binding_and_timezone(self):
        for field, value in (("namespace", "other"), ("app", "trainer-realign"), ("goal_steps", True),
                             ("started_at", "2026-10-08T00:00:00"), ("region", "us-east-1")):
            with self.subTest(field=field):
                collect.atomic_json(self.path / "run.json", {**self.run, field: value})
                with self.assertRaises(ValueError):
                    collect.load_run(self.path)

    def test_measured_step_times_dedup_and_throughput(self):
        events = [dict(timestamp="2026-10-08T00:00:01Z", run_id="run-1", rank=0,
                       global_step=1, event="step", step_seconds=0.25, attempt_id="first"),
                  dict(timestamp="2026-10-08T00:00:02Z", run_id="run-1", rank=0,
                       global_step=1, event="step", step_seconds=0.75, attempt_id="replay")]
        collector = collect.Collector(self.path)
        for event in events:
            collect.append_json(self.path / "monitor-events.jsonl", event)
            collector.log_batch("uid", "trainer", 0, event["timestamp"] + " " + collect.PREFIX + json.dumps(event))
        for rank in (0, 1):
            collect.append_json(self.path / "monitor-events.jsonl", dict(timestamp="2026-10-08T00:00:06Z",
                run_id="run-1", rank=rank, global_step=3, event="completed"))
        row = analyze.summarize(self.path)
        self.assertEqual(row["observed_step_time_samples"], 2)
        self.assertEqual(row["observed_step_seconds_total"], 1)
        self.assertEqual(row["observed_step_seconds_mean"], 0.5)
        self.assertEqual(row["observed_replayed_steps"], 1)
        self.assertEqual(row["goal_steps_per_second"], 0.5)
        for value in (-1, float("nan"), float("inf"), True, "1"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                collect.event_from_message(collect.PREFIX + json.dumps({**events[0], "step_seconds": value}))

    def test_checkpoint_phase_and_export_duration_not_double_counted(self):
        child = self.checkpoint()
        child["status"]["pods"] = [{"checkpointFiles": [
            {"checkpointTime": "2026-10-08T00:00:04Z", "exportedAt": "2026-10-08T00:00:07Z"}]}]
        parent = copy.deepcopy(child)
        parent["spec"]["schedule"] = {"enabled": True}
        parent["status"]["lastSuccessfulFullCheckpoint"] = {
            "uid": "checkpoint-1", "checkpointID": "round-1", "result": {"spec": child["spec"], "status": child["status"]}}
        self.snapshot([collect.safe_resource(child), parent])
        self.snapshot([child], name="two")
        row = analyze.summarize(self.path)
        self.assertEqual(row["observed_checkpoint_phase_seconds_total"], 3)
        self.assertEqual(row["observed_checkpoint_phase_seconds_mean"], 3)
        self.assertEqual(row["observed_checkpoint_export_samples"], 1)
        self.assertEqual(row["observed_checkpoint_export_seconds_total"], 2)
        self.assertEqual(row["observed_checkpoint_export_seconds_mean"], 2)

    def test_missing_checkpoint_export_is_not_zero(self):
        self.snapshot([self.checkpoint()])
        row = analyze.summarize(self.path)
        self.assertEqual(row["observed_checkpoint_phase_seconds_total"], 3)
        self.assertEqual(row["observed_checkpoint_export_seconds_total"], "")
        self.assertEqual(row["observed_step_seconds_total"], "")

    def comparison_rows(self):
        baseline = dict(run_id="baseline", experiment="cost", arm="A", phase="completed", excluded=False,
                        all_rank_completion_observed=True, dataset="cifar10",
                        billing_status="validated", currency="USD", total_cost="10", model="resnet18",
                        goal_steps=3, instance_type="g4dn.xlarge", batch_size=128, seed=0)
        return [baseline, {**baseline, "run_id": "candidate", "arm": "B", "total_cost": "7.5"}]

    def test_baseline_savings_and_cli(self):
        rows = self.comparison_rows()
        analyze.baseline_savings(rows, "baseline")
        self.assertEqual(rows[1]["cost_saving_percent"], "25.0")
        self.assertEqual(rows[0]["cost_comparison_status"], "baseline")
        self.assertEqual(rows[0]["cost_saving_percent"], "")
        rows[1]["total_cost"] = "12"
        analyze.baseline_savings(rows, "baseline")
        self.assertEqual(rows[1]["cost_saving_percent"], "-20")
        from unittest.mock import patch
        with patch.object(analyze, "summarize", side_effect=self.comparison_rows()):
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                self.assertEqual(analyze.main(["A", "B", "--baseline-run-id", "baseline"]), 0)
            self.assertEqual(list(csv.DictReader(io.StringIO(output.getvalue())))[1]["cost_saving_percent"], "25.0")

    def test_baseline_rejects_missing_invalid_or_ambiguous_baseline(self):
        for field, value in (("phase", "failed"), ("currency", "KRW"), ("total_cost", "0"),
                             ("total_cost", ""), ("excluded", True), ("seed", ""),
                             ("billing_status", "invalid"), ("arm", "B"), ("dataset", ""),
                             ("all_rank_completion_observed", False), ("all_rank_completion_observed", "")):
            rows = self.comparison_rows()
            rows[0][field] = value
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                analyze.baseline_savings(rows, "baseline")
        rows = self.comparison_rows()
        for values in ([], [rows[0], dict(rows[0])]):
            with self.assertRaises(ValueError):
                analyze.baseline_savings(values, "baseline")

    def test_baseline_comparison_guards(self):
        for field, value in (("model", "other"), ("goal_steps", 4), ("instance_type", "other"),
                             ("batch_size", 64), ("seed", 1), ("training_image", "different"),
                             ("phase", "failed"), ("currency", "KRW"), ("total_cost", ""),
                             ("excluded", True), ("billing_status", "invalid"), ("dataset", "other"),
                             ("dataset", ""), ("all_rank_completion_observed", False),
                             ("all_rank_completion_observed", "")):
            rows = self.comparison_rows()
            rows[1][field] = value
            analyze.baseline_savings(rows, "baseline")
            with self.subTest(field=field):
                self.assertEqual(rows[1]["cost_saving_percent"], "")
                self.assertNotEqual(rows[1]["cost_comparison_status"], "comparable")

    def test_cost_per_goal_step_both_arms(self):
        self.ledger([self.vm_row(), {"run_id": "run-1", "cost_type": "extra", "currency": "USD", "amount": "0"}])
        for rank in (0, 1):
            collect.append_json(self.path / "monitor-events.jsonl", {"timestamp": "2026-10-08T00:00:10Z",
                "global_step": 3, "rank": rank, "run_id": "run-1", "event": "completed"})
        for arm in ("A", "B"):
            collect.atomic_json(self.path / "run.json", {**self.run, "phase": "completed", "arm": arm})
            row = analyze.summarize(self.path)
            self.assertEqual(row["cost_per_goal_step"], "1")
            self.assertEqual(row["total_cost"], "3")
        collect.atomic_json(self.path / "run.json", {**self.run, "phase": "failed"})
        self.assertEqual(analyze.summarize(self.path)["cost_per_goal_step"], "")

    def test_dedup_survives_collector_restart(self):
        first = self.log(1, "2026-10-08T00:00:01.000000001Z")
        second = self.log(2, "2026-10-08T00:00:01.000000002Z")
        collector = collect.Collector(self.path)
        collector.log_batch("uid-1", "trainer", 0, first + second)
        collector.log_batch("uid-1", "trainer", 0, first + second)
        collect.Collector(self.path).log_batch("uid-1", "trainer", 0, first + second)
        lines = (self.path / "logs/uid-1/trainer.0.jsonl").read_text().splitlines()
        self.assertEqual(len(lines), 2)
        self.assertEqual(analyze.summarize(self.path)["observed_attempted_steps"], 2)

    def test_replay_rank_and_run_binding(self):
        collector = collect.Collector(self.path)
        collector.log_batch("old-uid", "trainer", 0,
                            self.log(1, "2026-10-08T00:00:01Z") + self.log(2, "2026-10-08T00:00:02Z"))
        collector.log_batch("new-uid", "trainer", 0,
                            self.log(1, "2026-10-08T00:00:03Z", "restored") +
                            self.log(2, "2026-10-08T00:00:04Z") + self.log(3, "2026-10-08T00:00:05Z") +
                            self.log(3, "2026-10-08T00:00:06Z", "completed") +
                            self.log(3, "2026-10-08T00:00:06Z", "completed", rank=1) +
                            self.log(4, "2026-10-08T00:00:07Z", rank=1) +
                            self.log(99, "2026-10-08T00:00:08Z", run_id="other"))
        result = analyze.summarize(self.path)
        self.assertEqual(result["observed_attempted_steps"], 4)
        self.assertEqual(result["observed_unique_steps"], 3)
        self.assertEqual(result["observed_replayed_steps"], 1)
        self.assertEqual(result["observed_restores"], 1)
        self.assertEqual(result["time_to_goal_seconds"], 6)
        self.assertTrue(result["goal_completed"])

    def test_no_raw_logs_or_unknown_event_fields_saved(self):
        collector = collect.Collector(self.path)
        payload = {"timestamp": "2026-10-08T00:00:01Z", "global_step": 1, "rank": 0,
                   "run_id": "run-1", "event": "step", "password": "sensitive-value"}
        collector.log_batch("uid", "trainer", 0, "2026-10-08T00:00:01Z password=sensitive-value\n" +
                            "2026-10-08T00:00:01Z EVAL_EVENT " + json.dumps(payload))
        contents = (self.path / "logs/uid/trainer.0.jsonl").read_text()
        self.assertNotIn("sensitive-value", contents)
        self.assertNotIn("password", contents)

    def test_resource_projection_removes_credentials(self):
        obj = self.checkpoint()
        obj["spec"].update({"env": [{"name": "TOKEN", "value": "secret"}], "credentials": "secret"})
        obj["metadata"]["annotations"]["kubectl.kubernetes.io/last-applied-configuration"] = "secret"
        obj["status"]["message"] = "secret"
        safe = collect.safe_resource(obj)
        self.assertNotIn("secret", json.dumps(safe))
        self.assertEqual(safe["spec"]["workloadRef"]["uid"], "workload-1")

    def test_only_completed_bound_rounds_count_and_mirrors_dedup(self):
        completed = self.checkpoint()
        stale = copy.deepcopy(completed)
        stale["spec"]["workloadRef"]["uid"] = "old-workload"
        old = copy.deepcopy(completed)
        old["status"]["startTime"] = "2026-10-07T23:00:00Z"
        mirror = copy.deepcopy(completed)
        mirror["metadata"]["uid"] = "mirror-uid"
        mirror["metadata"]["annotations"]["training.dcnlab.com/source-checkpoint-uid"] = "checkpoint-1"
        parent = copy.deepcopy(completed)
        parent["spec"]["schedule"] = {"enabled": True, "intervalSeconds": 300}
        self.snapshot([completed, stale, old, mirror, parent, self.checkpoint("failed", "Failed")])
        self.snapshot([completed], name="two")
        self.assertEqual(analyze.summarize(self.path)["observed_completed_checkpoints"], 1)

    def test_schedule_retained_success_uses_child_identity(self):
        child = self.checkpoint()
        parent = copy.deepcopy(child)
        parent["metadata"]["generation"] = 9
        parent["spec"]["schedule"] = {"enabled": True}
        parent["status"]["lastSuccessfulFullCheckpoint"] = {
            "uid": "checkpoint-1", "checkpointID": "round-1", "result": {"spec": child["spec"], "status": child["status"]}}
        self.snapshot([parent, child])
        self.assertEqual(analyze.summarize(self.path)["observed_completed_checkpoints"], 1)

    def test_explicit_cost_and_extra_zero(self):
        self.ledger([self.vm_row(), {"run_id": "run-1", "cost_type": "extra", "currency": "USD", "amount": "0"}])
        result = analyze.summarize(self.path)
        self.assertEqual(result["vm_cost"], "3")
        self.assertEqual(result["extra_cost"], "0")
        self.assertEqual(result["total_cost"], "3")

    def test_unlabeled_child_requires_run_parent_uid_and_workload(self):
        parent = self.checkpoint("parent-uid")
        parent["spec"]["schedule"] = {"enabled": True}
        child = self.checkpoint()
        child["metadata"]["labels"] = {collect.PARENT_LABEL: "parent-uid"}
        child["metadata"]["annotations"][collect.PARENT_UID] = "parent-uid"
        unrelated = copy.deepcopy(child)
        unrelated["metadata"]["uid"] = "unrelated"
        unrelated["metadata"]["annotations"][collect.PARENT_UID] = "old-parent-uid"
        old_workload = copy.deepcopy(child)
        old_workload["metadata"]["uid"] = "old-workload-child"
        old_workload["spec"]["workloadRef"]["uid"] = "old-workload"
        self.snapshot([child, unrelated, old_workload, parent])
        self.assertEqual(analyze.summarize(self.path)["observed_completed_checkpoints"], 1)

    def test_D_analytic_and_fixed_composition_flags(self):
        run = {**self.run, "experiment": "checkpoint", "arm": "D", "phase": "completed"}
        collect.atomic_json(self.path / "run.json", run)
        for rank in (0, 1):
            collect.append_json(self.path / "monitor-events.jsonl", {"timestamp": "2026-10-08T00:00:10Z",
                "global_step": 3, "rank": rank, "run_id": "run-1", "event": "completed"})
        policy = {"metadata": {"name": run["policy"], "namespace": collect.NAMESPACE},
                  "spec": {"workloadRef": {"uid": "workload-1"}, "targetWorkers": 2, "policy": {"fixedOnDemand": 1}},
                  "status": {"policy": {"onDemandWorkers": 1, "spotWorkers": 1},
                             "checkpoint": {"intervalSource": "risk-band-bootstrap", "intervalCostEvaluated": False}}}

        def save_policy():
            collect.atomic_json(self.path / "snapshots/policy.json", {"run_id": "run-1", "timestamp": "2026-10-08T00:01:00Z",
                "resources": [{"resource": "trainingpolicies", "items": [collect.safe_resource(policy)]}]})

        save_policy()
        result = analyze.summarize(self.path)
        self.assertTrue(result["excluded"])
        self.assertIn("D_missing_analytic_evidence", result["exclusion_reasons"])
        policy["status"]["checkpoint"].update(intervalSource="checkpoint-cost-analytic", intervalCostEvaluated=True)
        save_policy()
        self.assertFalse(analyze.summarize(self.path)["excluded"])
        policy["status"]["policy"].update(onDemandWorkers=2, spotWorkers=0)
        save_policy()
        result = analyze.summarize(self.path)
        self.assertEqual(result["fixed_composition_violations"], 1)
        self.assertIn("fixed_composition_violation", result["exclusion_reasons"])

    def test_rank_zero_alone_is_not_completion(self):
        collect.append_json(self.path / "monitor-events.jsonl", {"timestamp": "2026-10-08T00:00:10Z", "global_step": 3,
                             "rank": 0, "run_id": "run-1", "event": "completed"})
        result = analyze.summarize(self.path)
        self.assertFalse(result["goal_completed"])
        self.assertFalse(result["all_rank_completion_observed"])
        self.assertEqual(result["time_to_goal_seconds"], "")

    def test_completed_phase_cannot_replace_rank_evidence(self):
        collect.atomic_json(self.path / "run.json", {**self.run, "arm": "B", "phase": "completed",
                                                   "completed_at": "2026-10-08T00:00:20Z"})
        self.ledger([self.vm_row(), {"run_id": "run-1", "cost_type": "extra", "currency": "USD", "amount": "0"}])
        for ranks in ((), (0,), (1,)):
            for rank in ranks:
                collect.append_json(self.path / "monitor-events.jsonl", {"timestamp": "2026-10-08T00:00:10Z",
                    "global_step": 3, "rank": rank, "run_id": "run-1", "event": "completed"})
            result = analyze.summarize(self.path)
            if ranks == (1,):
                self.assertTrue(result["goal_completed"])
                self.assertFalse(result["excluded"])
                self.assertEqual(result["time_to_goal_seconds"], 10)
            else:
                self.assertIsNot(result["goal_completed"], True)
                self.assertIsNot(result["all_rank_completion_observed"], True)
                self.assertTrue(result["excluded"])
                self.assertIn("all_rank_completion_evidence_missing", result["exclusion_reasons"])
                self.assertEqual(result["completion_time"], "")
                self.assertEqual(result["time_to_goal_seconds"], "")
                self.assertEqual(result["cost_per_goal_step"], "")

    def test_failed_run_with_both_ranks_remains_excluded(self):
        collect.atomic_json(self.path / "run.json", {**self.run, "arm": "B", "phase": "failed",
                                                   "failure_reason": "changed risk unconsumed"})
        for rank in (0, 1):
            collect.append_json(self.path / "monitor-events.jsonl", {"timestamp": "2026-10-08T00:00:10Z",
                "global_step": 3, "rank": rank, "run_id": "run-1", "event": "completed"})
        result = analyze.summarize(self.path)
        self.assertTrue(result["goal_completed"])
        self.assertTrue(result["all_rank_completion_observed"])
        self.assertTrue(result["excluded"])
        self.assertIn("run_failed", result["exclusion_reasons"])
        self.assertEqual(result["failure_reason"], "changed risk unconsumed")

    def test_no_invented_extra_cost(self):
        self.ledger([self.vm_row()])
        result = analyze.summarize(self.path)
        self.assertEqual(result["vm_cost"], "3")
        self.assertEqual(result["extra_cost"], "")
        self.assertEqual(result["total_cost"], "")

    def test_invalid_ledgers_blank_cost(self):
        cases = [[self.vm_row(price_per_hour="")], [self.vm_row(price_per_hour="NaN")],
                 [self.vm_row(price_per_hour="-1")], [self.vm_row(), self.vm_row()],
                 [self.vm_row(end="2026-10-07T00:00:00Z")],
                 [self.vm_row(), {"run_id": "run-1", "cost_type": "extra", "currency": "KRW", "amount": "1"}]]
        for rows in cases:
            with self.subTest(rows=rows):
                self.ledger(rows)
                result = analyze.summarize(self.path)
                self.assertEqual(result["billing_status"], "invalid")
                self.assertEqual(result["vm_cost"], "")
                self.assertEqual(result["total_cost"], "")

    def test_ledger_run_binding_and_adjacent_intervals(self):
        self.ledger([self.vm_row(end="2026-10-08T01:00:00Z"),
                     self.vm_row(start="2026-10-08T01:00:00Z"), self.vm_row(run_id="other", price_per_hour="999")])
        result = analyze.summarize(self.path)
        self.assertEqual(result["vm_cost"], "3")
        self.assertEqual(result["billing_status"], "validated")

    def test_missing_instance_ledger_rejected(self):
        nodes = [{"metadata": {"namespace": collect.NAMESPACE, "labels": {collect.POLICY_LABEL: "eval-run-1"}},
                  "status": {"instanceId": identity}} for identity in ("i-12345678", "i-87654321")]
        self.snapshot([], nodes)
        self.ledger([self.vm_row()])
        result = analyze.summarize(self.path)
        self.assertEqual(result["billing_status"], "invalid")
        self.assertIn("missing_instance_billing", result["issues"])

    def test_log_poll_uses_since_time_and_rejects_uid_race(self):
        pod = {"metadata": {"uid": "old-uid", "name": "trainer-0", "labels": {"app": "eval-run-1", collect.RUN_LABEL: "run-1"}},
               "status": {"containerStatuses": [{"name": "trainer", "restartCount": 1}]}}
        calls = []

        def runner(args, **kwargs):
            calls.append(args)
            if "logs" in args:
                return subprocess.CompletedProcess(args, 0, self.log(1, "2026-10-08T00:00:01Z"), "")
            changed = copy.deepcopy(pod)
            changed["metadata"]["uid"] = "new-uid"
            return subprocess.CompletedProcess(args, 0, json.dumps(changed), "")

        collector = collect.Collector(self.path, runner=runner)
        collector.collect_logs(["kubectl"], pod)
        self.assertFalse((self.path / "logs").exists())
        self.assertTrue(any("--previous=true" in args for args in calls))
        self.assertTrue(all(any(arg.startswith("--since-time=") for arg in args) for args in calls if "logs" in args))
        self.assertIn("pod_changed_during_poll", (self.path / "errors.jsonl").read_text())

    def test_missing_environment_and_command_errors_do_not_leak(self):
        collector = collect.Collector(self.path, environ={}, runner=lambda *args, **kwargs:
                                      subprocess.CompletedProcess([], 1, "", "secret-token"))
        collector.poll(aws_status=True)
        collector.command(["unused"], "test")
        errors = (self.path / "errors.jsonl").read_text()
        self.assertIn("missing_kubeconfig", errors)
        self.assertNotIn("secret-token", errors)
        self.assertEqual(len(list((self.path / "snapshots").glob("*.json"))), 1)

    def test_optional_ec2_queries_only_run_nodeprovisions(self):
        calls = []
        node = {"metadata": {"namespace": collect.NAMESPACE, "labels": {collect.POLICY_LABEL: "eval-run-1"}},
                "status": {"instanceId": "i-12345678"}}
        other = copy.deepcopy(node)
        other["metadata"]["labels"][collect.POLICY_LABEL] = "other-policy"
        other["status"]["instanceId"] = "i-87654321"

        def runner(args, **kwargs):
            calls.append(args)
            data = {"items": [node, other]} if "nodeprovisions" in args else {"items": []}
            if args[0] == "aws":
                data = {"InstanceStatuses": [{"InstanceId": "i-12345678", "InstanceState": {"Name": "running"}},
                                             {"InstanceId": "i-87654321", "InstanceState": {"Name": "running"}}]}
            return subprocess.CompletedProcess(args, 0, json.dumps(data), "")

        collector = collect.Collector(self.path, runner=runner, environ={"AWS_KUBECONFIG": "private-path"})
        collector.poll(aws_status=True)
        aws = next(args for args in calls if args[0] == "aws")
        self.assertIn("i-12345678", aws)
        self.assertNotIn("i-87654321", aws)
        self.assertEqual(aws[aws.index("--region") + 1], "ap-northeast-2")
        snap = json.loads(next((self.path / "snapshots").glob("*.json")).read_text())
        self.assertEqual(len(snap["ec2"]["statuses"]), 1)
        self.assertNotIn("private-path", json.dumps(snap))

    def test_monitor_dedup_and_authoritative_phase(self):
        event = {"timestamp": "2026-10-08T00:00:01Z", "run_id": "run-1", "event": "step",
                 "global_step": 1, "rank": 0, "attempt_id": "session-1"}
        collect.Collector(self.path).log_batch("uid", "trainer", 0,
            event["timestamp"] + " EVAL_EVENT " + json.dumps(event))
        monitor = self.path / "monitor-events.jsonl"
        collect.append_json(monitor, event)
        collect.append_json(monitor, event)
        collect.append_json(monitor, {**event, "attempt_id": "session-2"})
        collect.append_json(monitor, {**event, "event": "completed", "global_step": 3, "timestamp": "2026-10-08T00:00:10Z"})
        collect.atomic_json(self.path / "run.json", {**self.run, "phase": "failed", "failure_reason": "timed out"})
        result = analyze.summarize(self.path)
        self.assertEqual(result["observed_attempted_steps"], 2)
        self.assertEqual(result["observed_replayed_steps"], 1)
        self.assertFalse(result["goal_completed"])
        self.assertEqual(result["time_to_goal_seconds"], "")
        self.assertEqual(result["failure_reason"], "timed out")

    def test_monitor_all_ranks_complete(self):
        collect.atomic_json(self.path / "run.json", {**self.run, "world_size": 2, "phase": "completed",
                                                   "completed_at": "2026-10-08T00:00:20Z"})
        for rank, at in ((0, "2026-10-08T00:00:10Z"), (1, "2026-10-08T00:00:12Z")):
            collect.append_json(self.path / "monitor-events.jsonl", {"timestamp": at, "global_step": 3,
                                 "rank": rank, "run_id": "run-1", "event": "completed", "attempt_id": "worker-" + str(rank)})
        result = analyze.summarize(self.path)
        self.assertTrue(result["goal_completed"])
        self.assertEqual(result["time_to_goal_seconds"], 12)
        self.assertEqual(result["observed_attempted_steps"], "")

    def test_collector_waits_for_runner_start(self):
        prepared = {**self.run, "phase": "prepared"}
        prepared.pop("started_at")
        collect.atomic_json(self.path / "run.json", prepared)
        collector = collect.Collector(self.path, environ={})
        collector.poll()
        self.assertFalse((self.path / "snapshots").exists())
        collect.atomic_json(self.path / "run.json", {**self.run, "phase": "running"})
        collector.poll()
        self.assertEqual(len(list((self.path / "snapshots").glob("*.json"))), 1)

    def test_csv_cli_and_malformed_log(self):
        path = self.path / "logs/uid/trainer.0.jsonl"
        path.parent.mkdir(parents=True)
        path.write_text("not json\n", encoding="utf-8")
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            self.assertEqual(analyze.main([str(self.path)]), 0)
        row = next(csv.DictReader(io.StringIO(output.getvalue())))
        self.assertEqual(row["vm_cost"], "")
        self.assertIn("invalid_jsonl", row["issues"])


if __name__ == "__main__":
    unittest.main()
