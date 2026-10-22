import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from datetime import datetime, timezone
import checkpoint_suite as suite
import test_run

run = test_run.run


class SuiteTests(unittest.TestCase):
    def setUp(self):
        test_run.RenderTests.setUp(self)

    def test_fixed_and_dynamic(self):
        for arm, lo, hi in [("F300",300,300),("F1200",1200,1200),("D",60,3600)]:
            self.c.update(model="resnet50", scenario="interruption",
                          risk_schedule=[{"after_seconds":600,"lambda_per_hour":0.8}])
            info, member, karmada, policy = run.render(self.c,self.source,"r50test","checkpoint",arm)
            self.assertEqual(policy["spec"]["checkpoint"]["minIntervalSeconds"],lo)
            self.assertEqual(policy["spec"]["checkpoint"]["maxIntervalSeconds"],hi)
            self.assertEqual(policy["spec"]["policy"]["fixedOnDemand"],1)
            self.assertTrue(run.has_risk_schedule(info))

    def test_combined_validation(self):
        self.c.update(scenario="interruption",risk_schedule=[])
        with self.assertRaises(ValueError):
            run.validate(self.c,"test","checkpoint","F1200")
        self.c.pop("risk_schedule")
        run.validate(self.c,"test","checkpoint","F1200")
        self.assertFalse(run.has_risk_schedule(self.c))

    def test_configure_preserves_source_and_refuses_overwrite(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)
            base = p / "base.json"
            base.write_text(json.dumps(dict(self.c, policy_source="policy.json")))
            output = p / "out.json"
            suite.configure(base, output, 22000, 14400, "interruption")
            c = json.loads(output.read_text())
            self.assertEqual(c["policy_source"], str((p / "policy.json").resolve()))
            self.assertEqual(c["model"], "resnet50")
            self.assertEqual(c["training_image"], self.c["training_image"])
            self.assertEqual([x["after_seconds"] for x in c["risk_schedule"]], [600, 1800])
            with self.assertRaises(FileExistsError):
                suite.configure(base, output, 22000, 14400, "interruption")

    def test_combined_requires_consumption(self):
        info=dict(scenario="interruption",arm="F1200",risk_schedule=[{}])
        with self.assertRaisesRegex(RuntimeError,"schedule incomplete"):
            run.verify_scenario(Path("."),info)

    def test_completed_schedule_still_requires_fis(self):
        info=dict(scenario="interruption",arm="F1200",risk_schedule=[{}],
                  risk_schedule_observations=[{"consumed_at":"now"}])
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaisesRegex(RuntimeError,"no recorded FIS"):
                run.verify_scenario(Path(d),info)

    def test_gate(self):
        r=dict(run_id="test",experiment="checkpoint",scenario="interruption",phase="running",max_seconds=7200)
        self.assertFalse(suite.gate(r,"test",1500,30,None))
        self.assertFalse(suite.gate(r,"test",1500,30,1499))
        self.assertTrue(suite.gate(r,"test",1500,30,1501))
        for confirm, elapsed in [("other",1500),("test",1531)]:
            with self.assertRaises(ValueError):
                suite.gate(r,confirm,1500,30,elapsed)
        for phase in ["completed","failed","prepared"]:
            with self.assertRaises(ValueError):
                suite.gate(dict(r,phase=phase),"test",1500,30,1500)

    def test_elapsed(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)
            self.assertIsNone(suite.elapsed_since_progress(p,datetime.now(timezone.utc)))
            (p/"runner-events.jsonl").write_text(json.dumps({"event":"all_ranks_progress_observed","timestamp":"2026-10-22T00:00:00Z"})+"\n{")
            self.assertEqual(suite.elapsed_since_progress(p,datetime(2026,10,22,0,25,tzinfo=timezone.utc)),1500)

    def test_start_only_once_and_never_retry(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)
            r=dict(run_id="test",experiment="checkpoint",scenario="interruption",phase="running",max_seconds=7200)
            for name,value in [("run.json",r),("fis-template.json",{}),("fis-target.json",{})]:
                (p/name).write_text(json.dumps(value))
            with patch.object(suite,"elapsed_since_progress",return_value=1501), patch.object(suite.fis,"start") as start:
                suite.interrupt(p,"test",1500,30,10,False)
                start.assert_called_once_with(p,"test",False)
            (p/"fis-started.json").write_text("{}")
            with patch.object(suite.fis,"start") as start:
                with self.assertRaisesRegex(ValueError,"already requested"):
                    suite.interrupt(p,"test",1500,30,10,False)
                start.assert_not_called()

    def test_missed_window_never_calls_aws(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)
            r=dict(run_id="test",experiment="checkpoint",scenario="interruption",phase="running",max_seconds=7200)
            for name,value in [("run.json",r),("fis-template.json",{}),("fis-target.json",{})]:
                (p/name).write_text(json.dumps(value))
            with patch.object(suite,"elapsed_since_progress",return_value=1600), patch.object(suite.fis,"start") as start:
                with self.assertRaisesRegex(ValueError,"missed injection"):
                    suite.interrupt(p,"test",1500,30,10,False)
                start.assert_not_called()
