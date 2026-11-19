"""Offline contract tests for configure; every external boundary is mocked."""

import json
from pathlib import Path
import unittest
from unittest.mock import call, patch

import e3_fis_setup as setup


class E3FISSetupTests(unittest.TestCase):
    def start_patch(self, patcher):
        mocked = patcher.start()
        self.addCleanup(patcher.stop)
        return mocked

    def setUp(self):
        self.e3 = self.start_patch(patch.object(setup, "e3", autospec=True))
        self.mutate = self.start_patch(patch.object(setup, "mutate", autospec=True))
        self.process = self.start_patch(patch.object(
            setup.subprocess, "run", side_effect=AssertionError("external process forbidden")))
        self.start_patch(patch("builtins.print"))
        self.out = Path("unused-evidence")
        self.region = "ap-northeast-2"
        self.account = "123456789012"
        self.role_name = "test-fis-role"
        self.role = "arn:aws:iam::123456789012:role/test-fis-role"
        self.spot = "i-0123456789abcdef0"
        self.od = "i-0123456789abcdef1"
        self.run = {"run_id": "e3-test", "region": self.region, "policy_uid": "policy-1",
                    "e3": {"arm": "B", "pilot": False}}
        self.target = {"instance_id": self.spot}
        self.e3.info.return_value = self.run
        self.e3.fis.target.return_value = self.target
        self.e3.fis.aws.side_effect = self.aws_response
        self.e3.runner.POLICY_UID = "training.dcnlab.com/policy-uid"
        self.e3.runner.get.return_value = {"items": [self.node("OnDemand", self.od),
            self.node("Spot", self.spot), self.node("OnDemand", "i-deleting", deleting=True)]}

    @staticmethod
    def node(market, instance, deleting=False):
        return {"metadata": {"deletionTimestamp": "2026-11-04T00:00:00Z"} if deleting else {},
                "spec": {"marketType": market}, "status": {"instanceId": instance}}

    def aws_response(self, *args):
        if args == (self.region, "sts", "get-caller-identity"):
            return {"Account": self.account}
        if args == (self.region, "iam", "get-role", "--role-name", self.role_name):
            return {"Role": {"Arn": self.role}}
        raise AssertionError("unexpected AWS API: %r" % (args,))

    def configure(self):
        setup.configure(self.out, "e3-test", self.role_name)

    def assert_no_changes(self):
        self.mutate.assert_not_called()
        self.e3.runner.save.assert_not_called()
        self.e3.authorize.assert_not_called()
        self.e3.fis.start.assert_not_called()
        self.process.assert_not_called()

    def test_rejects_A_and_pilot_before_any_external_lookup(self):
        for arm, pilot in (("A", False), ("A", True), ("B", True), ("C", True)):
            with self.subTest(arm=arm, pilot=pilot):
                self.run["e3"].update(arm=arm, pilot=pilot)
                with self.assertRaisesRegex(ValueError, "only for measured B/C"):
                    self.configure()
                self.e3.fis.target.assert_not_called()
                self.e3.fis.aws.assert_not_called()
                self.e3.runner.get.assert_not_called()
                self.assert_no_changes()

    def test_measured_B_C_scope_IAM_and_alarm_without_fault_API(self):
        for arm in ("B", "C"):
            with self.subTest(arm=arm):
                self.e3.reset_mock()
                self.mutate.reset_mock()
                self.run["e3"]["arm"] = arm
                self.configure()
                resource = "arn:aws:ec2:ap-northeast-2:123456789012:instance/" + self.spot
                alarm_name = "HybridSpotFIS-e3-test-od-health"
                alarm = "arn:aws:cloudwatch:ap-northeast-2:123456789012:alarm:" + alarm_name
                receipt = {"role": self.role, "role_name": self.role_name, "policy_name": "E3-e3-test",
                           "resource": resource, "alarm": alarm, "alarm_name": alarm_name,
                           "od_instance_id": self.od, "target": self.target}
                # Exact call lists exclude FIS start/plan and all other unapproved APIs.
                self.assertEqual(self.e3.mock_calls, [
                    call.info(self.out, "e3-test"), call.fis.target(self.run),
                    call.fis.aws(self.region, "sts", "get-caller-identity"),
                    call.runner.get("aws", "nodeprovisions", selector="training.dcnlab.com/policy-uid=policy-1"),
                    call.fis.aws(self.region, "iam", "get-role", "--role-name", self.role_name),
                    call.runner.save(self.out / "e3-fis-setup.json", receipt),
                    call.authorize(self.out, self.role, alarm)])
                self.assertEqual(self.mutate.call_count, 2)
                iam_args = self.mutate.call_args_list[0].args
                self.assertEqual(iam_args[:-1], (self.region, "iam", "put-role-policy", "--role-name",
                    self.role_name, "--policy-name", "E3-e3-test", "--policy-document"))
                policy = json.loads(iam_args[-1])
                self.assertEqual(policy, {"Version": "2012-10-17", "Statement": [
                    {"Effect": "Allow", "Action": "ec2:SendSpotInstanceInterruptions", "Resource": resource},
                    {"Effect": "Allow", "Action": "ec2:DescribeInstances", "Resource": "*"}]})
                self.assertEqual(self.mutate.call_args_list[1], call(self.region, "cloudwatch", "put-metric-alarm",
                    "--alarm-name", alarm_name, "--namespace", "AWS/EC2", "--metric-name", "StatusCheckFailed",
                    "--dimensions", "Name=InstanceId,Value=" + self.od, "--statistic", "Maximum", "--period", "60",
                    "--evaluation-periods", "1", "--datapoints-to-alarm", "1", "--threshold", "1",
                    "--comparison-operator", "GreaterThanOrEqualToThreshold", "--treat-missing-data", "breaching"))
                self.e3.fis.start.assert_not_called()
                self.e3.fis.plan.assert_not_called()
                self.e3.inject.assert_not_called()
                self.process.assert_not_called()

    def test_confirmation_failure_stops_before_target_lookup(self):
        self.e3.info.side_effect = ValueError("confirmation mismatch")
        with self.assertRaisesRegex(ValueError, "confirmation mismatch"):
            self.configure()
        self.e3.info.assert_called_once_with(self.out, "e3-test")
        self.e3.fis.target.assert_not_called()
        self.assert_no_changes()

    def test_invalid_role_name_rejected_before_external_lookup(self):
        self.role_name = "invalid/role"
        with self.assertRaisesRegex(ValueError, "invalid IAM role"):
            self.configure()
        self.e3.fis.target.assert_not_called()
        self.e3.fis.aws.assert_not_called()
        self.assert_no_changes()

    def test_requires_one_live_OD_with_instance_identity(self):
        for nodes in ([], [self.node("Spot", self.spot)], [self.node("OnDemand", "")],
                      [self.node("OnDemand", self.od), self.node("OnDemand", "i-second")],
                      [self.node("OnDemand", self.od, deleting=True)]):
            with self.subTest(nodes=nodes):
                self.e3.runner.get.return_value = {"items": nodes}
                with self.assertRaisesRegex(ValueError, "initial one-OD/one-Spot"):
                    self.configure()
                self.assert_no_changes()

    def test_role_account_mismatch_rejected_before_mutation(self):
        self.role = "arn:aws:iam::999999999999:role/test-fis-role"
        with self.assertRaisesRegex(ValueError, "role account mismatch"):
            self.configure()
        self.assert_no_changes()


if __name__ == "__main__":
    unittest.main()
