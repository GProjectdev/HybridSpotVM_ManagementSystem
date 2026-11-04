import copy
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
import e3_experiment as e3


class E3Tests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.plan = e3.runner.read(Path(__file__).parent / "e3-plan.example.json")
        self.plan["approved_for_measurement"] = True
        self.plan["pilot_evidence"] = "unit-test pilot fixture evidence only"
        e3.runner.save(self.root / "plan.json", self.plan)
        source = {"spec": {"capacity": {"aws": {"ami": "ami-test", "subnetId": "subnet-test",
            "vpcId": "vpc-test", "securityGroupIds": ["sg-test"], "credentialsRef": {"name": "aws"}}}}}
        e3.runner.save(self.root / "source.json", source)

    def render(self, arm, pilot=False):
        out = self.root / arm
        a = SimpleNamespace(plan=self.root / "plan.json", config=Path(__file__).parent / "config.gpt2.example.json",
                            policy_source=self.root / "source.json", run_id="e3-test", arm=arm, out=out, pilot=pilot)
        e3.render(a)
        return out, e3.runner.read(out / "run.json"), e3.runner.read(out / "policy.yaml")

    def test_three_arms(self):
        for arm in "ABC":
            out, run, policy = self.render(arm)
            self.assertEqual(run["e3"]["arm"], arm)
            self.assertTrue(run["restore_audit"])
            control = policy["spec"]["policy"]
            self.assertEqual(control["alpha"], .8)
            if arm == "C":
                self.assertNotIn("fixedOnDemand", control)
            else:
                self.assertEqual(control["fixedOnDemand"], 2 if arm == "A" else 1)
                self.assertEqual(policy["spec"]["checkpoint"]["minIntervalSeconds"], 300)
                self.assertEqual(policy["spec"]["checkpoint"]["maxIntervalSeconds"], 300)
            self.assertNotIn("training.dcnlab.com/periodic-checkpoint", policy["metadata"]["annotations"])

    def test_unapproved_and_invalid_risk_rejected(self):
        p = copy.deepcopy(self.plan)
        p["approved_for_measurement"] = False
        with self.assertRaises(ValueError):
            e3.validate_plan(p)
        e3.validate_plan(p, pilot=True)
        p["alpha"] = .05
        with self.assertRaises(ValueError):
            e3.validate_plan(p, pilot=True)

    def test_pilot_never_injects(self):
        out, value, _ = self.render("C", True)
        with patch.object(e3.fis, "start") as start:
            e3.inject(out, value, 1200)
            start.assert_not_called()
        self.assertEqual(e3.runner.read(out / "e3-fault.json")["outcome"], "pilot_no_fault")

    def test_A_never_injects(self):
        out, value, _ = self.render("A")
        with patch.object(e3.fis, "start") as start:
            e3.inject(out, value, 3000)
            start.assert_not_called()

    def test_missed_time_fails_closed(self):
        out, value, _ = self.render("B")
        with patch.object(e3.fis, "start") as start:
            with self.assertRaises(RuntimeError):
                e3.inject(out, value, 3300)
            start.assert_not_called()

    def test_changed_target_rejected(self):
        out, value, _ = self.render("B")
        value["policy_uid"] = "uid"
        e3.runner.save(out / "e3-authorization.json", {"target": {"instance_id": "i-old"}})
        with patch.object(e3.runner, "get", return_value={"items": [{"spec": {"marketType": "Spot"}}]}), \
             patch.object(e3.fis, "target", return_value={"instance_id": "i-new"}), \
             patch.object(e3.fis, "start") as start:
            with self.assertRaises(RuntimeError):
                e3.inject(out, value, 3000)
            start.assert_not_called()


if __name__ == "__main__":
    unittest.main()
