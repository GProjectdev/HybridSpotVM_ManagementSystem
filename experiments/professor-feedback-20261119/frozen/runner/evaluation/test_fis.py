import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import fis


class FISTests(unittest.TestCase):
    def setUp(self):
        self.run = {"run_id":"r18f01", "phase":"running", "scenario":"interruption", "region":"ap-northeast-2",
                    "availability_zone":"ap-northeast-2c", "policy":"eval-r18f01", "policy_uid":"policy-uid"}
        self.node = {"metadata":{"name":"node-1","uid":"node-uid"},
                     "spec":{"marketType":"Spot"},"status":{"instanceId":"i-1234567890abcdef0"}}
        self.ec2 = {"Reservations":[{"Instances":[{"State":{"Name":"running"},"InstanceLifecycle":"spot",
                            "Placement":{"AvailabilityZone":"ap-northeast-2c"}}]}]}

    def test_target_is_exactly_one_owned_spot(self):
        with patch.object(fis.runner,"owned"), patch.object(fis.runner,"get",return_value={"items":[self.node]}) as query, patch.object(fis,"aws",return_value=self.ec2):
            target = fis.target(self.run)
            self.assertEqual(target["nodeprovision_uid"],"node-uid")
            self.assertIn("policy-uid=policy-uid",query.call_args.kwargs["selector"])
        with patch.object(fis.runner,"owned"), patch.object(fis.runner,"get",return_value={"items":[self.node,self.node]}), patch.object(fis,"aws") as command:
            with self.assertRaises(ValueError):fis.target(self.run)
            command.assert_not_called()

    def test_od_instance_or_wrong_az_refused(self):
        for mutate in (lambda i:i.pop("InstanceLifecycle"), lambda i:i["Placement"].update(AvailabilityZone="ap-northeast-2a")):
            ec2=json.loads(json.dumps(self.ec2));mutate(ec2["Reservations"][0]["Instances"][0])
            with patch.object(fis.runner,"owned"), patch.object(fis.runner,"get",return_value={"items":[self.node]}), patch.object(fis,"aws",return_value=ec2):
                with self.assertRaises(ValueError):fis.target(self.run)

    def test_plan_has_no_fault_side_effect_and_start_checks_confirmation(self):
        with tempfile.TemporaryDirectory() as d:
            evidence=Path(d);fis.runner.save(evidence/"run.json",self.run)
            t={"nodeprovision":"node-1","nodeprovision_uid":"node-uid","instance_id":"i-1234567890abcdef0"}
            with patch.object(fis,"target",return_value=t), patch.object(fis,"aws",return_value={"Account":"123456789012"}) as command:
                fis.plan(evidence,"arn:aws:iam::123456789012:role/fis-test",None)
                self.assertEqual(command.call_count,1)
                template=fis.runner.read(evidence/"fis-template.json")
                self.assertEqual(len(template["targets"]["oneSpot"]["resourceArns"]),1)
                with self.assertRaises(ValueError):fis.start(evidence,"wrong-run",True)
                with self.assertRaisesRegex(ValueError,"no stop alarm"):fis.start(evidence,"r18f01",False)
                self.assertFalse(any("start-experiment" in c.args for c in command.call_args_list))

    def test_changed_node_uid_cannot_start(self):
        with tempfile.TemporaryDirectory() as d:
            evidence=Path(d);fis.runner.save(evidence/"run.json",self.run)
            fis.runner.save(evidence/"fis-target.json",{"nodeprovision_uid":"old"})
            with patch.object(fis,"target",return_value={"nodeprovision_uid":"new"}), patch.object(fis,"aws") as command:
                with self.assertRaisesRegex(ValueError,"target changed"):fis.start(evidence,"r18f01",True)
                command.assert_not_called()

if __name__ == "__main__":
    unittest.main()
