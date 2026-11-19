import copy
from contextlib import ExitStack
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import fis
import e3_experiment as e3


class FISE3GuardTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.out = Path(directory.name)
        self.run = dict(run_id="guard", phase="running", scenario="interruption",
                        region="ap-northeast-2", policy_uid="policy",
                        e3=dict(arm="B", fault_after_seconds=300,
                                fault_tolerance_seconds=20, window_seconds=600))
        self.original = dict(nodeprovision="spot", nodeprovision_uid="uid", instance_id="i-abc")
        self.current = copy.deepcopy(self.original)
        self.account = "123456789012"
        self.role = "arn:aws:iam::" + self.account + ":role/fis"
        self.alarm = "arn:aws:cloudwatch:ap-northeast-2:" + self.account + ":alarm:guard"
        self.clock = 1000
        self.create_delay = 0
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.object(fis, "target", side_effect=lambda value: dict(self.current)))
        self.stack.enter_context(patch.object(fis.time, "monotonic", side_effect=lambda: self.clock))
        self.aws = self.stack.enter_context(patch.object(fis, "aws", side_effect=self.command))
        fis.runner.save(self.out / "run.json", self.run)
        fis.runner.save(self.out / "e3-authorization.json",
                        dict(target=self.original, role=self.role, alarm=self.alarm))
        fis.plan(self.out, self.role, self.alarm)
        self.aws.reset_mock()

    def command(self, region, service, action, *args):
        if (service, action) == ("sts", "get-caller-identity"):
            return {"Account": self.account}
        if (service, action) == ("cloudwatch", "describe-alarms"):
            return {"MetricAlarms": [{"AlarmArn": self.alarm, "StateValue": "OK"}]}
        if (service, action) == ("fis", "create-experiment-template"):
            self.clock += self.create_delay
            return {"experimentTemplate": {"id": "template"}}
        if (service, action) == ("fis", "start-experiment"):
            return {"experiment": {"id": "experiment"}}
        raise AssertionError((service, action))

    def assert_not_started(self):
        self.assertFalse(any("start-experiment" in c.args for c in self.aws.call_args_list))

    def test_replanned_target_cannot_replace_original_authorization(self):
        for key in self.original:
            with self.subTest(key=key):
                self.current = dict(self.original, **{key: "different"})
                fis.plan(self.out, self.role, self.alarm)
                with self.assertRaisesRegex(ValueError, "since authorization"):
                    fis.start(self.out, "guard", False, expected_target=self.original)
                self.assert_not_started()

    def test_plan_must_still_match_when_live_target_matches_authorization(self):
        self.current = dict(self.original, nodeprovision_uid="different")
        fis.plan(self.out, self.role, self.alarm)
        self.current = dict(self.original)
        with self.assertRaisesRegex(ValueError, "since plan"):
            fis.start(self.out, "guard", False, expected_target=self.original)
        self.assert_not_started()

    def test_template_api_delay_crossing_deadline_blocks_start(self):
        self.create_delay = 21
        with self.assertRaisesRegex(RuntimeError, "deadline exceeded"):
            fis.start(self.out, "guard", False, expected_target=self.original,
                      deadline_monotonic=1020)
        self.assert_not_started()
        self.assertTrue((self.out / "fis-created.json").exists())
        self.assertEqual(fis.runner.read(self.out / "fis-started.json")["state"],
                         "deadline_exceeded_not_started")

    def test_receipt_io_at_deadline_blocks_start(self):
        save = fis.runner.save
        def delayed_save(path, value):
            save(path, value)
            if path.name == "fis-started.json":
                self.clock = 1020
        with patch.object(fis.runner, "save", side_effect=delayed_save):
            with self.assertRaisesRegex(RuntimeError, "deadline exceeded"):
                fis.start(self.out, "guard", False, deadline_monotonic=1020)
        self.assert_not_started()

    def test_guarded_start_before_deadline_succeeds(self):
        fis.start(self.out, "guard", False, expected_target=self.original,
                  deadline_monotonic=1020)
        starts = [c for c in self.aws.call_args_list if "start-experiment" in c.args]
        self.assertEqual(len(starts), 1)
        self.assertEqual(fis.runner.read(self.out / "fis-started.json")["experiment"]["id"],
                         "experiment")

    def test_legacy_start_without_optional_guards_succeeds(self):
        self.create_delay = 1000
        fis.start(self.out, "guard", False)
        self.assertEqual(sum("start-experiment" in c.args for c in self.aws.call_args_list), 1)

    def test_e3_binds_authorization_even_if_plan_reselects_target(self):
        plan = fis.plan
        def drifted_plan(*args):
            self.current = dict(self.original, instance_id="i-def")
            plan(*args)
        with patch.object(e3.runner, "get", return_value={"items": [
                {"spec": {"marketType": "Spot"}}]}), patch.object(fis, "plan", side_effect=drifted_plan):
            with self.assertRaisesRegex(ValueError, "since authorization"):
                e3.inject(self.out, self.run, 300)
        self.assert_not_started()

    def test_e3_deadline_reaches_final_api_guard(self):
        self.create_delay = 21
        with patch.object(e3.runner, "get", return_value={"items": [
                {"spec": {"marketType": "Spot"}}]}):
            with self.assertRaisesRegex(RuntimeError, "deadline exceeded"):
                e3.inject(self.out, self.run, 300)
        self.assert_not_started()

    def test_e3_reserves_warning_time_before_window_end(self):
        self.run["e3"].update(window_seconds=425, fault_tolerance_seconds=50)
        self.create_delay = 6
        with patch.object(e3.runner, "get", return_value={"items": [
                {"spec": {"marketType": "Spot"}}]}):
            with self.assertRaisesRegex(RuntimeError, "deadline exceeded"):
                e3.inject(self.out, self.run, 300)
        self.assert_not_started()

    def test_schedule_processing_time_counts_toward_injection_elapsed(self):
        self.run["initial_composition_observed"] = True
        def advance(*args):
            self.clock += 25
        with patch.object(e3.runner, "advance_risk_schedule", side_effect=advance), \
                patch.object(e3, "inject") as inject:
            e3.tick(self.out, self.run, 300)
        inject.assert_called_once_with(self.out, self.run, 325)


if __name__ == "__main__":
    unittest.main()
