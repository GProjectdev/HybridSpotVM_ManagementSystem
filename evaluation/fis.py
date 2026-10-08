#!/usr/bin/env python3
"""Explicit, UID-scoped single-Spot-instance FIS experiment. No implicit fault injection."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
import run as runner

def aws(region, *args):
    p = subprocess.run(["aws", "--region", region, *args, "--output", "json"],
                       text=True, capture_output=True, timeout=60)
    if p.returncode:
        raise RuntimeError(p.stderr[-2000:])
    return json.loads(p.stdout)

def target(run):
    runner.owned(run, "karmada", "trainingpolicy", run["policy"], run["policy_uid"])
    nodes = runner.get("aws", "nodeprovisions", selector=runner.POLICY_UID+"="+run["policy_uid"])["items"]
    spots = [n for n in nodes if n.get("spec", {}).get("marketType")=="Spot" and not n["metadata"].get("deletionTimestamp")]
    if len(spots)!=1:
        raise ValueError("expected exactly one owned Spot NodeProvision")
    n=spots[0]
    instance_id=n.get("status",{}).get("instanceId") or n.get("status",{}).get("spot",{}).get("instanceID")
    if not instance_id or not re.fullmatch(r"i-[0-9a-f]+",instance_id):
        raise ValueError("missing instance ID")
    result=aws(run["region"],"ec2","describe-instances","--instance-ids",instance_id)
    instances=[i for r in result["Reservations"] for i in r["Instances"]]
    if len(instances)!=1 or instances[0]["State"]["Name"]!="running" or instances[0].get("InstanceLifecycle")!="spot":
        raise ValueError("target must be a running Spot instance")
    if instances[0]["Placement"]["AvailabilityZone"]!=run["availability_zone"]:
        raise ValueError("target AZ mismatch")
    return {"nodeprovision":n["metadata"]["name"],"nodeprovision_uid":n["metadata"]["uid"],"instance_id":instance_id}

def plan(evidence, role, alarm):
    run=runner.read(evidence/"run.json")
    if run.get("phase")!="running" or run["scenario"]!="interruption":
        raise ValueError("requires running scenario=interruption")
    identity=aws(run["region"],"sts","get-caller-identity")
    account=identity["Account"]
    if not role.startswith("arn:aws:iam::"+account+":role/"):
        raise ValueError("role must belong to current AWS account")
    t=target(run)
    template={"description":"Evaluation "+run["run_id"]+" single Spot interruption",
              "roleArn":role,
              "targets":{"oneSpot":{"resourceType":"aws:ec2:spot-instance",
                                   "resourceArns":["arn:aws:ec2:"+run["region"]+":"+account+":instance/"+t["instance_id"]],
                                   "selectionMode":"ALL"}},
              "actions":{"interrupt":{"actionId":"aws:ec2:send-spot-instance-interruptions",
                                      "parameters":{"durationBeforeInterruption":"PT2M"},
                                      "targets":{"SpotInstances":"oneSpot"}}},
              "stopConditions":[{"source":"aws:cloudwatch:alarm","value":alarm}] if alarm else [{"source":"none"}],
              "tags":{"evaluation-run":run["run_id"]}}
    if (evidence/"fis-started.json").exists():
        raise ValueError("one interruption per run; already started")
    runner.save(evidence/"fis-template.json",template)
    runner.save(evidence/"fis-target.json",dict(t,account=account,run_id=run["run_id"]))
    print("Plan only; no interruption sent. Inspect",evidence/"fis-template.json")

def start(evidence, confirm, allow_no_alarm):
    run=runner.read(evidence/"run.json")
    if run["run_id"]!=confirm or run.get("phase")!="running" or run["scenario"]!="interruption":
        raise ValueError("running interruption scenario and exact --confirm run_id required")
    if (evidence/"fis-started.json").exists():
        raise ValueError("already started; inspect existing FIS experiment")
    t=target(run)
    planned=runner.read(evidence/"fis-target.json")
    if any(t[k]!=planned[k] for k in t):
        raise ValueError("target changed since plan; refuse")
    identity=aws(run["region"],"sts","get-caller-identity")
    if identity["Account"]!=planned["account"]:
        raise ValueError("AWS account changed")
    template=runner.read(evidence/"fis-template.json")
    expected="arn:aws:ec2:"+run["region"]+":"+planned["account"]+":instance/"+t["instance_id"]
    if template.get("targets")!={"oneSpot":{"resourceType":"aws:ec2:spot-instance","resourceArns":[expected],"selectionMode":"ALL"}}:
        raise ValueError("template target changed")
    action={"interrupt":{"actionId":"aws:ec2:send-spot-instance-interruptions",
                         "parameters":{"durationBeforeInterruption":"PT2M"},"targets":{"SpotInstances":"oneSpot"}}}
    if template.get("actions")!=action:
        raise ValueError("unexpected FIS actions")
    if template.get("stopConditions")==[{"source":"none"}] and not allow_no_alarm:
        raise ValueError("no stop alarm: explicitly pass --allow-no-alarm after reviewing risk")
    created=aws(run["region"],"fis","create-experiment-template","--cli-input-json",json.dumps(template))
    runner.save(evidence/"fis-created.json",created)
    # Persist intent before start; do not blindly retry an ambiguous network failure.
    token="eval-"+run["run_id"]
    runner.save(evidence/"fis-started.json",{"state":"start_requested","client_token":token,"target":t})
    result=aws(run["region"],"fis","start-experiment","--experiment-template-id",created["experimentTemplate"]["id"],
               "--client-token",token,"--tags","evaluation-run="+run["run_id"])
    runner.save(evidence/"fis-started.json",result)
    runner.event(evidence,"interruption_requested",instance_id=t["instance_id"],experiment_id=result["experiment"]["id"])
    print("REAL Spot interruption started:",result["experiment"]["id"])
    print("Track FIS/EC2 completion; stop-experiment does not undo an interruption already sent.")

def main():
    p=argparse.ArgumentParser(description=__doc__)
    sub=p.add_subparsers(dest="cmd",required=True)
    a=sub.add_parser("plan"); a.add_argument("--role-arn",required=True); a.add_argument("--alarm-arn")
    a.add_argument("--evidence",required=True,type=Path)
    a=sub.add_parser("start"); a.add_argument("--evidence",required=True,type=Path)
    a.add_argument("--confirm",required=True); a.add_argument("--allow-no-alarm",action="store_true")
    a=p.parse_args()
    if a.cmd=="plan": plan(a.evidence,a.role_arn,a.alarm_arn)
    else: start(a.evidence,a.confirm,a.allow_no_alarm)

if __name__=="__main__":
    try: main()
    except (ValueError,RuntimeError,subprocess.SubprocessError) as e:
        print("ERROR:",e,file=sys.stderr);sys.exit(1)
