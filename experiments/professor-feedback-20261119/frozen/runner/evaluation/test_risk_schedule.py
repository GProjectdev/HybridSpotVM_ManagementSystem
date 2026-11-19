import copy
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
import test_run
run = test_run.run


class RiskScheduleTests(unittest.TestCase):
    def setUp(self):
        test_run.RenderTests.setUp(self)
        self.c.update(scenario="risk-schedule", risk_schedule=[
            {"after_seconds": 100, "lambda_per_hour": 0.8},
            {"after_seconds": 200, "lambda_per_hour": 0.05}])

    def test_invalid_schedules(self):
        for schedule in ([], [{"after_seconds": 0, "lambda_per_hour": 0.8}],
                         [{"after_seconds": 100, "lambda_per_hour": -1}],
                         list(reversed(self.c["risk_schedule"]))):
            config = copy.deepcopy(self.c)
            config["risk_schedule"] = schedule
            with self.assertRaises(ValueError):
                run.validate(config, "test", "cost", "B")

    def test_schedule_consumption_and_return_to_low(self):
        info = dict(self.c, risk="risk", policy_uid="uid")
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory)
            with patch.object(run, "set_risk") as setter:
                run.advance_risk_schedule(evidence, info, 99)
                setter.assert_not_called()
                run.advance_risk_schedule(evidence, info, 100)
                setter.assert_called_once_with(evidence, 0.8)
            run.save(evidence/"policy-latest.json", {"status":{"policy":{"lambdaPerHour":0.8}}})
            risk = {"metadata":{"generation":2}, "spec":{"staticLambdaPerHour":0.8},
                    "status":{"ready":True,"observedGeneration":2,"lambdaPerHour":0.8}}
            with patch.object(run, "owned", return_value=risk), patch.object(run, "set_risk") as setter:
                run.advance_risk_schedule(evidence, info, 200)
                setter.assert_called_once_with(evidence, 0.05)
                self.assertTrue(info["risk_schedule_observations"][0]["consumed_at"])
            with self.assertRaisesRegex(RuntimeError, "incomplete"):
                run.verify_scenario(evidence, info)

    def test_unconsumed_event_does_not_get_overwritten(self):
        info = dict(self.c, risk="risk")
        with tempfile.TemporaryDirectory() as directory:
            evidence = Path(directory)
            with patch.object(run, "set_risk"):
                run.advance_risk_schedule(evidence, info, 100)
            run.save(evidence/"policy-latest.json", {})
            with patch.object(run, "owned", return_value={}), patch.object(run, "set_risk") as setter:
                with self.assertRaisesRegex(RuntimeError, "not consumed"):
                    run.advance_risk_schedule(evidence, info, 200)
                setter.assert_not_called()
