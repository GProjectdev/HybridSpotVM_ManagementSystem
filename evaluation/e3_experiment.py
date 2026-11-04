#!/usr/bin/env python3
"""E3 controlled comparison. Rendering never changes clusters."""
import argparse
import hashlib
import math
import json
import time
from pathlib import Path
import fis
import run as runner
from collect import safe_resource


def validate_plan(p, pilot=False):
    for key in ("window_seconds", "provisioning_timeout_seconds", "fixed_interval_seconds",
                "risk_after_seconds", "fault_after_seconds", "fault_tolerance_seconds"):
        if type(p.get(key)) is not int or p[key] <= 0:
            raise ValueError(key + " must be a positive integer")
    if not 0 < p["risk_after_seconds"] < p["fault_after_seconds"] < p["window_seconds"] - 120:
        raise ValueError("require risk < FIS request < window end minus warning")
    for key in ("initial_risk", "changed_risk"):
        if type(p.get(key)) not in (int, float) or not math.isfinite(p[key]) or p[key] < 0:
            raise ValueError(key + " must be finite and nonnegative")
    if p["changed_risk"] <= p["initial_risk"]:
        raise ValueError("require increasing risk")
    for key in ("timing_rationale", "risk_rationale", "fixed_interval_rationale"):
        if not isinstance(p.get(key), str) or len(p[key].strip()) < 20 or "REPLACE" in p[key]:
            raise ValueError("preregister " + key)
    if type(p.get("alpha")) not in (int, float) or not 0 < p["alpha"] < 1:
        raise ValueError("alpha must be between zero and one")
    if type(p.get("forecast_horizon_seconds")) is not int or p["forecast_horizon_seconds"] <= 0:
        raise ValueError("positive forecast horizon required")
    threshold = -math.log(p["alpha"]) / (p["forecast_horizon_seconds"] / 3600)
    if not p["initial_risk"] < threshold < p["changed_risk"]:
        raise ValueError("risk levels must bracket the implemented one-Spot survival threshold")
    if not pilot and p.get("approved_for_measurement") is not True:
        raise ValueError("pilot review and approved_for_measurement=true required")
    if not pilot and (not isinstance(p.get("pilot_evidence"), str) or
                      len(p["pilot_evidence"]) < 20 or "REPLACE" in p["pilot_evidence"]):
        raise ValueError("record actual pilot evidence before main measurements")


def render(a):
    plan = runner.read(a.plan)
    validate_plan(plan, a.pilot)
    if a.pilot:
        plan.update(window_seconds=1800, risk_after_seconds=600, fault_after_seconds=1200)
    c = runner.read(a.config)
    c.update(scenario="constant", initial_risk=plan["initial_risk"],
             changed_risk=plan["changed_risk"], restore_audit=True,
             max_seconds=plan["window_seconds"] + plan["provisioning_timeout_seconds"])
    c.pop("risk_schedule", None)
    value, member, karmada, policy = runner.render(c, runner.read(a.policy_source), a.run_id, "cost", "B")
    control = policy["spec"]["policy"]
    control.update(alpha=plan["alpha"], forecastHorizonSeconds=plan["forecast_horizon_seconds"])
    control["minOnDemand"] = 2 if a.arm == "A" else 1
    if a.arm != "C":
        control["fixedOnDemand"] = 2 if a.arm == "A" else 1
        policy["metadata"]["annotations"]["training.dcnlab.com/planned-partial"] = "disabled"
        interval = plan["fixed_interval_seconds"]
        policy["spec"]["checkpoint"].update(minIntervalSeconds=interval, maxIntervalSeconds=interval,
            riskBands=[{"maxLambdaPerHour": plan["changed_risk"], "intervalSeconds": interval}])
    else:
        control.pop("fixedOnDemand", None)
    value.update(evaluation="E3", e3=dict(plan, arm=a.arm, pilot=a.pilot),
                 risk_schedule=[{"after_seconds": plan["risk_after_seconds"],
                                 "lambda_per_hour": plan["changed_risk"]}])
    value["e3"]["plan_sha256"] = hashlib.sha256(a.plan.read_bytes()).hexdigest()
    a.out.mkdir(parents=True, exist_ok=False)
    for name, obj in [("run.json", value), ("policy.yaml", policy), ("e3-plan.json", plan),
                      ("member.yaml", {"apiVersion": "v1", "kind": "List", "items": member}),
                      ("karmada.yaml", {"apiVersion": "v1", "kind": "List", "items": karmada})]:
        runner.save(a.out / name, obj)
    print("Rendered only:", a.out, "arm=", a.arm)


def info(out, confirm):
    value = runner.read(out / "run.json")
    if value.get("evaluation") != "E3" or confirm != value["run_id"]:
        raise ValueError("E3 evidence and exact --confirm run ID required")
    validate_plan(value["e3"], value["e3"].get("pilot", False))
    return value


def authorize(out, role, alarm):
    value = runner.read(out / "run.json")
    if value["e3"]["arm"] == "A":
        raise ValueError("A has no fault target")
    target = fis.target(value)
    account = fis.aws(value["region"], "sts", "get-caller-identity")["Account"]
    arn = "arn:aws:ec2:" + value["region"] + ":" + account + ":instance/" + target["instance_id"]
    result = fis.aws(value["region"], "iam", "simulate-principal-policy",
                     "--policy-source-arn", role, "--action-names", "ec2:SendSpotInstanceInterruptions",
                     "--resource-arns", arn).get("EvaluationResults", [])
    if len(result) != 1 or result[0].get("EvalDecision") != "allowed" or result[0].get("MissingContextValues"):
        raise ValueError("IAM simulation must allow this exact instance ARN")
    if not alarm.startswith("arn:aws:cloudwatch:" + value["region"] + ":" + account + ":alarm:"):
        raise ValueError("alarm account/region mismatch")
    runner.save(out / "e3-authorization.json", dict(target=target, role=role, alarm=alarm))
    print("Exact Spot UID/instance bound; no interruption requested:", target)


def inject(out, value, elapsed):
    entered = time.monotonic()
    p = value["e3"]
    record = out / "e3-fault.json"
    if record.exists():
        return
    if p.get("pilot"):
        runner.save(record, {"outcome": "pilot_no_fault", "elapsed": elapsed})
        return
    if elapsed - p["fault_after_seconds"] > p["fault_tolerance_seconds"]:
        raise RuntimeError("missed preregistered fault time; refuse late injection")
    if p["arm"] == "A":
        runner.save(record, {"outcome": "od_control_no_fault", "elapsed": elapsed})
        return
    auth = runner.read(out / "e3-authorization.json")
    nodes = runner.get("aws", "nodeprovisions", selector=runner.POLICY_UID + "=" + value["policy_uid"])["items"]
    spots = [n for n in nodes if n.get("spec", {}).get("marketType") == "Spot"]
    if not spots and p["arm"] == "C":
        ready_od = [n for n in nodes if n.get("spec", {}).get("marketType") == "OnDemand"
                    and n.get("status", {}).get("phase") == "Ready"
                    and not n.get("metadata", {}).get("deletionTimestamp")]
        sts = runner.get("aws", "statefulset", value["app"])
        if len(ready_od) != 2 or sts.get("status", {}).get("readyReplicas") != 2:
            raise RuntimeError("OD transition lacks two ready workers and two ready replicas")
        decision = runner.read(out / "policy-latest.json").get("status", {}).get("policy", {})
        if decision.get("spotWorkers") != 0 or decision.get("onDemandWorkers") != 2 or runner.active_operations(value):
            raise RuntimeError("Spot absent without verified completed OD transition")
        data = fis.aws(value["region"], "ec2", "describe-instances", "--instance-ids", auth["target"]["instance_id"])
        states = [i["State"]["Name"] for r in data["Reservations"] for i in r["Instances"]]
        if len(states) != 1 or states[0] not in ("terminated", "stopped"):
            raise RuntimeError("old Spot exposure remains")
        runner.save(record, {"outcome": "no_spot_exposure", "elapsed": elapsed, "decision": decision})
        return
    if fis.target(value) != auth["target"]:
        raise RuntimeError("target changed: refuse to interrupt a replacement")
    entries = fis.aws(value["region"], "cloudwatch", "describe-alarms",
                      "--alarm-names", auth["alarm"].split(":alarm:", 1)[1]).get("MetricAlarms", [])
    if len(entries) != 1 or entries[0].get("AlarmArn") != auth["alarm"] or entries[0].get("StateValue") != "OK":
        raise RuntimeError("stop alarm must be OK; do not bypass missing telemetry")
    value["scenario"] = "interruption"
    runner.save(out / "run.json", value)
    runner.save(record, {"outcome": "request_intent", "elapsed": elapsed, "target": auth["target"]})
    fis.plan(out, auth["role"], auth["alarm"])
    if elapsed + time.monotonic() - entered - p["fault_after_seconds"] > p["fault_tolerance_seconds"]:
        raise RuntimeError("AWS validation exceeded fault tolerance; request not sent")
    deadline = entered + min(p["fault_after_seconds"] + p["fault_tolerance_seconds"],
                             p["window_seconds"] - 120) - elapsed
    fis.start(out, value["run_id"], False, expected_target=auth["target"],
              deadline_monotonic=deadline)
    runner.save(record, {"outcome": "requested_not_yet_verified", "elapsed": elapsed,
                        "request_returned_elapsed": elapsed + time.monotonic() - entered,
                        "target": auth["target"]})


def tick(out, value, elapsed):
    entered = time.monotonic()
    if not value.get("initial_composition_observed"):
        decision = runner.read(out / "policy-latest.json").get("status", {}).get("policy", {})
        expected = (2, 0) if value["e3"]["arm"] == "A" else (1, 1)
        if (decision.get("onDemandWorkers"), decision.get("spotWorkers")) != expected:
            raise RuntimeError("initial composition differs from preregistered arm")
        value["initial_composition_observed"] = True
    runner.advance_risk_schedule(out, value, elapsed)
    if elapsed >= value["e3"]["fault_after_seconds"]:
        inject(out, value, elapsed + time.monotonic() - entered)


def snapshot(out):
    value = runner.read(out / "run.json")
    result = {"observed_at": runner.now(), "resources": {}, "errors": []}
    for cluster, kinds in (("karmada", ("trainingpolicies", "restorerequests", "spotreplacements")),
                           ("aws", ("nodeprovisions", "pods", "fluidcrmigrations"))):
        for kind in kinds:
            try:
                objects = runner.get(cluster, kind)["items"]
                selected = [o for o in objects if
                            (value.get("policy_uid") and o.get("metadata", {}).get("labels", {}).get(runner.POLICY_UID) == value["policy_uid"])
                            or o.get("metadata", {}).get("labels", {}).get(runner.LABEL) == value["run_id"]]
                result["resources"][cluster + "/" + kind] = [safe_resource(o) for o in selected]
            except Exception as exc:
                result["errors"].append(cluster + "/" + kind + ":" + type(exc).__name__)
    if (out / "fis-started.json").exists():
        try:
            identifier = runner.read(out / "fis-started.json")["experiment"]["id"]
            response = fis.aws(value["region"], "fis", "get-experiment", "--id", identifier)["experiment"]
            result["fis"] = {k: response[k] for k in ("id", "state", "actions") if k in response}
            target = runner.read(out / "fis-target.json")["instance_id"]
            data = fis.aws(value["region"], "ec2", "describe-instances", "--instance-ids", target)
            result["target_ec2"] = [{"id": i["InstanceId"], "state": i["State"]["Name"]}
                                   for r in data["Reservations"] for i in r["Instances"]]
        except Exception as exc:
            result["errors"].append("fis:" + type(exc).__name__)
    runner.save(out / "e3-final.json", result)
    print("Snapshot saved; check errors and actual FIS/restore results.")


def preflight(out):
    value = runner.read(out / "run.json")
    runner.guard_idle()
    crd = json.loads(runner.kubectl("karmada", "get", "crd", "trainingpolicies.training.dcnlab.com", "-o", "json"))
    schema = next(v for v in crd["spec"]["versions"] if v["name"] == "v1alpha1")["schema"]["openAPIV3Schema"]
    if "fixedOnDemand" not in schema["properties"]["spec"]["properties"]["policy"]["properties"]:
        raise ValueError("fixedOnDemand CRD missing")
    runner.kubectl("aws", "get", "storageclass", value["storage_class"], "-o", "name")
    quotas = {}
    for code in ("L-DB2E81BA", "L-3819A6DF"):
        q = fis.aws(value["region"], "service-quotas", "get-service-quota", "--service-code", "ec2", "--quota-code", code)["Quota"]
        quotas[code] = {"name": q["QuotaName"], "value": q["Value"]}
    runner.save(out / "e3-preflight.json", {"at": runner.now(), "quotas": quotas,
        "note": "Limits are not free capacity. Verify running instances, NFS, GPU drivers and controller images."})
    print("Read-only checks passed. NFS access, images and spare quota still require pilot verification.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("render")
    for name in ("config", "policy-source", "plan", "out"):
        p.add_argument("--" + name, type=Path, required=True)
    p.add_argument("--run-id", required=True)
    p.add_argument("--arm", choices=("A", "B", "C"), required=True)
    p.add_argument("--pilot", action="store_true")
    for name in ("preflight", "prepare", "start", "authorize-fis", "snapshot", "stop", "cleanup"):
        p = sub.add_parser(name)
        p.add_argument("--evidence", type=Path, required=True)
        p.add_argument("--confirm", required=True)
        if name == "authorize-fis":
            p.add_argument("--role-arn", required=True)
            p.add_argument("--alarm-arn", required=True)
    a = parser.parse_args()
    if a.command == "render":
        render(a)
        return
    info(a.evidence, a.confirm)
    if a.command == "start":
        try:
            runner.start(a.evidence, tick_callback=tick)
        finally:
            snapshot(a.evidence)
    elif a.command == "authorize-fis":
        authorize(a.evidence, a.role_arn, a.alarm_arn)
    elif a.command == "cleanup":
        runner.cleanup(a.evidence, a.confirm)
    elif a.command in ("preflight", "snapshot"):
        globals()[a.command](a.evidence)
    elif a.command == "prepare":
        preflight(a.evidence)
        runner.prepare(a.evidence)
    else:
        getattr(runner, a.command)(a.evidence)


if __name__ == "__main__":
    main()
