import copy
import importlib.util
import json
from contextlib import ExitStack
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location("e3_window_runner", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


class WindowTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.evidence = Path(self.directory.name)
        self.clock = 0
        self.info = dict(phase="prepared", run_id="test", app="eval-test",
                         workload_uid="workload", policy="policy", policy_uid="uid",
                         experiment="cost", arm="A", scenario="constant",
                         goal_steps=100, max_seconds=3,
                         e3=dict(arm="A", window_seconds=12, provisioning_timeout_seconds=10))
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.collector = Mock()
        self.collector.poll.return_value = None
        self.mocks = {}
        for name in ("owned", "patch_policy", "observe_policy", "stop", "verify_scenario",
                     "advance_risk_schedule", "set_risk"):
            self.mocks[name] = self.stack.enter_context(patch.object(runner, name))
        self.stack.enter_context(patch.object(runner.subprocess, "Popen", return_value=self.collector))
        self.stack.enter_context(patch.object(runner.time, "monotonic", side_effect=lambda: self.clock))
        self.stack.enter_context(patch.object(runner.time, "sleep", side_effect=self.sleep))
        self.stack.enter_context(patch.object(runner, "now", side_effect=lambda: str(self.clock)))
        self.stack.enter_context(patch.object(runner, "get", return_value={"items": [
            {"metadata": {"name": "pod"}, "status": {"phase": "Running"}}]}))
        self.logs = lambda: self.progress()
        self.command = self.stack.enter_context(patch.object(
            runner, "kubectl", side_effect=lambda *args, **kwargs: self.logs() if "logs" in args else ""))

    def sleep(self, seconds):
        self.clock += seconds

    def progress(self, ranks=(0, 1), step=1, event="step"):
        return "\n".join("EVAL_EVENT " + json.dumps(dict(
            run_id="test", event=event, rank=rank, global_step=step)) for rank in ranks)

    def start(self, **kwargs):
        runner.save(self.evidence / "run.json", self.info)
        runner.start(self.evidence, **kwargs)
        return runner.read(self.evidence / "run.json")

    def test_window_starts_only_after_both_positive_ranks_and_ignores_max_seconds(self):
        self.logs = lambda: self.progress((0,)) if self.clock < 5 else self.progress()
        ticks = []
        def tick(evidence, info, elapsed):
            persisted = runner.read(evidence / "run.json")
            self.assertEqual(persisted["measurement_started_at"], "5")
            ticks.append(elapsed)
            info["callback_receipt"] = elapsed
        check = Mock(return_value=True)
        info = self.start(tick_callback=tick, completion_check=check)
        self.assertEqual(info["phase"], "measured")
        self.assertEqual(info["measurement_started_at"], "5")
        self.assertEqual(info["measurement_ended_at"], "17")
        self.assertEqual(ticks, [0, 5, 10])
        self.assertEqual(info["callback_receipt"], 10)
        self.assertNotIn("completed_at", info)
        check.assert_not_called()
        self.mocks["verify_scenario"].assert_not_called()
        events = [json.loads(line)["event"] for line in
                  (self.evidence / "runner-events.jsonl").read_text().splitlines()]
        self.assertEqual(events.count("measurement_window_completed"), 1)
        self.assertNotIn("all_ranks_completed", events)
        self.mocks["stop"].assert_called_once()
        self.collector.terminate.assert_called_once()

    def test_zero_progress_or_missing_rank_times_out_before_measurement(self):
        for ranks, step in [((0, 1), 0), ((0,), 1)]:
            with self.subTest(ranks=ranks, step=step):
                self.clock = 0
                self.logs = lambda: self.progress(ranks, step)
                callback = Mock()
                with self.assertRaisesRegex(TimeoutError, "provisioning_timeout_seconds"):
                    self.start(tick_callback=callback)
                info = runner.read(self.evidence / "run.json")
                self.assertEqual(info["phase"], "failed")
                self.assertNotIn("measurement_started_at", info)
                callback.assert_not_called()

    def test_any_rank_goal_before_window_is_failure(self):
        for event in ("step", "completed"):
            with self.subTest(event=event):
                self.clock = 0
                self.logs = lambda: (self.progress() if self.clock == 0
                                     else self.progress((0,), 100, event))
                with self.assertRaisesRegex(RuntimeError, "goal reached before"):
                    self.start()
                info = runner.read(self.evidence / "run.json")
                self.assertEqual(info["phase"], "failed")
                self.assertEqual(info["measurement_started_at"], "0")
                self.assertEqual(info["measurement_ended_at"], "5")
                self.mocks["verify_scenario"].assert_not_called()

    def test_callback_failure_is_persisted_and_stopped(self):
        callback = Mock(side_effect=RuntimeError("dispatch failed"))
        with self.assertRaisesRegex(RuntimeError, "dispatch failed"):
            self.start(tick_callback=callback)
        info = runner.read(self.evidence / "run.json")
        self.assertEqual(info["phase"], "failed")
        self.assertEqual(info["failure_reason"], "dispatch failed")
        self.assertIn("measurement_ended_at", info)
        self.mocks["stop"].assert_called_once()
        self.collector.terminate.assert_called_once()

    def test_slow_callback_does_not_dispatch_again_after_window(self):
        def tick(evidence, info, elapsed):
            self.clock += 20
        callback = Mock(side_effect=tick)
        self.assertEqual(self.start(tick_callback=callback)["phase"], "measured")
        callback.assert_called_once()

    def test_e3_uses_constant_scenario_and_no_legacy_schedule(self):
        self.info.update(scenario="risk-rise", risk_change_after_seconds=1, changed_risk=1,
                         risk_schedule=[{"after_seconds": 1, "lambda_per_hour": 1}])
        self.assertEqual(self.start()["scenario"], "constant")
        self.mocks["advance_risk_schedule"].assert_not_called()
        self.mocks["set_risk"].assert_not_called()

    def test_invalid_e3_rejected_before_side_effects(self):
        valid = copy.deepcopy(self.info["e3"])
        invalid = [None, [], {}, dict(valid, arm="D")]
        for key in ("window_seconds", "provisioning_timeout_seconds"):
            invalid.extend(dict(valid, **{key: value}) for value in
                           (0, -1, True, "10", float("nan"), float("inf")))
        for config in invalid:
            with self.subTest(config=config):
                self.info["e3"] = config
                with self.assertRaises(ValueError):
                    self.start()
        self.mocks["owned"].assert_not_called()
        self.command.assert_not_called()

    def test_legacy_goals_and_functional_completion_still_verify(self):
        del self.info["e3"]
        for functional in (False, True):
            with self.subTest(functional=functional):
                self.clock = 0
                self.logs = lambda: self.progress(step=1 if functional else 100, event="completed")
                check = Mock(return_value=True) if functional else None
                self.assertEqual(self.start(completion_check=check)["phase"], "completed")
        self.assertEqual(self.mocks["verify_scenario"].call_count, 2)

    def test_legacy_timeout_unchanged(self):
        del self.info["e3"]
        with self.assertRaisesRegex(TimeoutError, "max_seconds reached"):
            self.start()
        self.assertEqual(runner.read(self.evidence / "run.json")["phase"], "failed")


class PolicyTests(unittest.TestCase):
    def test_e3_composition_and_checkpoint_exemptions(self):
        with tempfile.TemporaryDirectory() as directory:
            for experiment in ("cost", "checkpoint"):
                for arm, composition in (("A", (2, 0)), ("B", (1, 1)),
                                         ("C", (2, 0)), ("C", (0, 2))):
                    with self.subTest(experiment=experiment, arm=arm, composition=composition):
                        info = dict(experiment=experiment, arm="A", scenario="constant",
                                    policy="policy", policy_uid="uid", app="app", e3={"arm": arm})
                        obj = {"status": {"policy": dict(decisionStage="ready",
                                onDemandWorkers=composition[0], spotWorkers=composition[1])}}
                        with patch.object(runner, "owned", return_value=obj), patch.object(runner, "get") as get:
                            runner.observe_policy(Path(directory), info)
                            get.assert_not_called()
                            if arm != "C":
                                obj["status"]["policy"]["onDemandWorkers"] = 0
                                with self.assertRaisesRegex(RuntimeError, "E3 arm composition"):
                                    runner.observe_policy(Path(directory), info)
                            obj["status"]["policy"] = {}
                            runner.observe_policy(Path(directory), info)

    def test_legacy_checkpoint_composition_still_checked(self):
        info = dict(experiment="checkpoint", arm="D", scenario="constant",
                    policy="policy", policy_uid="uid")
        obj = {"status": {"policy": dict(decisionStage="ready", onDemandWorkers=2, spotWorkers=0)}}
        with patch.object(runner, "owned", return_value=obj):
            with self.assertRaisesRegex(RuntimeError, "fixed composition"):
                runner.observe_policy(Path("."), info)


if __name__ == "__main__":
    unittest.main()
