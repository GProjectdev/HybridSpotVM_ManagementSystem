"""Configure only this run's FIS IAM policy and OD health alarm; never starts FIS."""
import argparse
import json
import subprocess
import re
from pathlib import Path
import e3_experiment as e3


def mutate(region, *args):
    subprocess.run(["aws", "--region", region, *args], check=True, timeout=60)


def configure(out, confirm, role_name):
    run = e3.info(out, confirm)
    if run["e3"]["arm"] == "A" or run["e3"].get("pilot"):
        raise ValueError("FIS configuration is only for measured B/C runs")
    if not re.fullmatch(r"[A-Za-z0-9+=,.@_-]{1,64}", role_name):
        raise ValueError("invalid IAM role name")
    target = e3.fis.target(run)
    account = e3.fis.aws(run["region"], "sts", "get-caller-identity")["Account"]
    nodes = e3.runner.get("aws", "nodeprovisions", selector=e3.runner.POLICY_UID + "=" + run["policy_uid"])["items"]
    od = [n for n in nodes if n.get("spec", {}).get("marketType") == "OnDemand"
          and not n.get("metadata", {}).get("deletionTimestamp")]
    if len(od) != 1 or not od[0].get("status", {}).get("instanceId"):
        raise ValueError("configure while initial one-OD/one-Spot pair exists")
    role = e3.fis.aws(run["region"], "iam", "get-role", "--role-name", role_name)["Role"]["Arn"]
    if not role.startswith("arn:aws:iam::" + account + ":role/"):
        raise ValueError("role account mismatch")
    resource = "arn:aws:ec2:" + run["region"] + ":" + account + ":instance/" + target["instance_id"]
    policy_name = "E3-" + run["run_id"]
    policy = {"Version": "2012-10-17", "Statement": [
        {"Effect": "Allow", "Action": "ec2:SendSpotInstanceInterruptions", "Resource": resource},
        {"Effect": "Allow", "Action": "ec2:DescribeInstances", "Resource": "*"}]}
    alarm_name = "HybridSpotFIS-" + run["run_id"] + "-od-health"
    alarm = "arn:aws:cloudwatch:" + run["region"] + ":" + account + ":alarm:" + alarm_name
    receipt = {"role": role, "role_name": role_name, "policy_name": policy_name,
               "resource": resource, "alarm": alarm, "alarm_name": alarm_name,
               "od_instance_id": od[0]["status"]["instanceId"], "target": target}
    e3.runner.save(out / "e3-fis-setup.json", receipt)
    mutate(run["region"], "iam", "put-role-policy", "--role-name", role_name,
           "--policy-name", policy_name, "--policy-document", json.dumps(policy))
    mutate(run["region"], "cloudwatch", "put-metric-alarm", "--alarm-name", alarm_name,
           "--namespace", "AWS/EC2", "--metric-name", "StatusCheckFailed",
           "--dimensions", "Name=InstanceId,Value=" + receipt["od_instance_id"],
           "--statistic", "Maximum", "--period", "60", "--evaluation-periods", "1",
           "--datapoints-to-alarm", "1", "--threshold", "1",
           "--comparison-operator", "GreaterThanOrEqualToThreshold", "--treat-missing-data", "breaching")
    e3.authorize(out, role, alarm)
    print("Configured; alarm must become OK before scheduled request. No FIS sent.")


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--evidence", type=Path, required=True)
    p.add_argument("--confirm", required=True)
    p.add_argument("--role-name", required=True)
    a = p.parse_args()
    configure(a.evidence, a.confirm, a.role_name)
