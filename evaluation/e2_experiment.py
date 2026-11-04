#!/usr/bin/env python3
"""E2 GPT-2 recovery experiment; checkpoint-gated, explicit FIS injection."""
import argparse
import hashlib
import json
from pathlib import Path
import tarfile

import run as runner
import fis
from collect import event_from_message
from e1_experiment import durable_world, restored_progress, evidence_resource


def info(out, confirm=None):
    value = runner.read(out / "run.json")
    if value.get("evaluation") != "E2" or value.get("cluster") != "aws":
        raise ValueError("requires E2 AWS evidence")
    if confirm is not None and confirm != value["run_id"]:
        raise ValueError("--confirm must equal run_id")
    return value


def render(args):
    config = runner.read(args.config)
    config.update(model="gpt2", cluster="aws", scenario="interruption", restore_audit=True,
                  goal_steps=args.goal_steps, max_seconds=args.max_seconds)
    config.pop("risk_schedule", None)
    value, member, karmada, policy = runner.render(config, runner.read(args.policy_source), args.run_id, "checkpoint", "F300")
    policy["spec"]["policy"]["alpha"] = 0.05
    value.update(evaluation="E2", resume_steps=args.resume_steps)
    args.out.mkdir(parents=True, exist_ok=False)
    for name, obj in (("run.json", value), ("member.yaml", {"apiVersion": "v1", "kind": "List", "items": member}),
                      ("karmada.yaml", {"apiVersion": "v1", "kind": "List", "items": karmada}), ("policy.yaml", policy)):
        runner.save(args.out / name, obj)
    print("Rendered only. Review AWS template, NFS paths, image and FIS prerequisites before prepare.")


def checkpoint_gate(out):
    value = info(out)
    runner.owned(value, "karmada", "trainingpolicy", value["policy"], value["policy_uid"])
    pods = runner.get("aws", "pods", selector="app=" + value["app"])["items"]
    if len(pods) != 2 or any(p.get("status", {}).get("phase") != "Running" for p in pods):
        raise ValueError("both source ranks must be Running")
    uids = [p["metadata"]["uid"] for p in pods]
    candidates = []
    for obj in runner.get("aws", "fluidcrmigrations")["items"]:
        if obj.get("metadata", {}).get("labels", {}).get(runner.POLICY_UID) != value["policy_uid"]:
            continue
        if obj.get("spec", {}).get("workloadRef", {}).get("uid") != value["workload_uid"]:
            continue
        checkpoint_id = obj.get("metadata", {}).get("annotations", {}).get("training.dcnlab.com/checkpoint-id")
        if checkpoint_id and durable_world(obj, uids, checkpoint_id):
            candidates.append(obj)
    if not candidates:
        raise ValueError("no durable Completed two-rank checkpoint for current Pod UIDs; wait and retry plan")
    selected = max(candidates, key=lambda o: o["status"]["completionTime"])
    runner.save(out / "e2-checkpoint.json", evidence_resource(selected))
    return selected


def snapshot(out):
    value = info(out)
    if not value.get("policy_uid"):
        return
    resources = []
    for cluster in ("karmada", "aws"):
        for kind in ("nodeprovisions", "restorerequests", "spotrecoveries", "fluidcrmigrations"):
            for obj in runner.get(cluster, kind)["items"]:
                labels = obj.get("metadata", {}).get("labels", {})
                ref = obj.get("spec", {}).get("workloadRef", {})
                if labels.get(runner.POLICY_UID) == value["policy_uid"] or ref.get("uid") == value["workload_uid"]:
                    resources.append(dict(cluster=cluster, resource=evidence_resource(obj)))
    runner.save(out / "e2-resources.json", {"observed_at": runner.now(), "resources": resources})


def recovered_ranks(out, value):
    events = []
    path = out / "monitor-events.jsonl"
    if path.exists():
        for line in path.read_text(encoding="utf-8").splitlines():
            obj = json.loads(line)
            if obj.get("run_id") == value["run_id"]:
                safe = event_from_message("EVAL_EVENT " + json.dumps(obj))
                if safe:
                    events.append(safe)
    ranks = {str(rank): restored_progress([e for e in events if e.get("rank") == rank], value["resume_steps"])
             for rank in (0, 1)}
    return events, ranks


def completion_check(out, value):
    if not (out / "fis-started.json").exists():
        return False
    _, ranks = recovered_ranks(out, value)
    if any(v is None for v in ranks.values()) or len(set(ranks.values())) != 1:
        return False
    try:
        runner.verify_scenario(out, value)
    except RuntimeError:
        # Restore verification and old-node cleanup can lag training progress.
        return False
    return True


def report(out):
    """Offline allowlisted export; no kubeconfigs, manifests or raw stderr."""
    value = info(out)
    export = out / "presentation"
    export.mkdir(exist_ok=True)
    events, ranks = recovered_ranks(out, value)
    same_step = all(v is not None for v in ranks.values()) and len(set(ranks.values())) == 1
    required = ("e2-checkpoint.json", "e2-resources.json", "fis-final.json", "fis-instance-final.json")
    missing = [name for name in required if not (out / name).exists()]
    summary = {"run_id": value["run_id"], "evaluation": "E2", "model": "gpt2", "world_size": 2,
               "composition": "1 On-Demand + 1 Spot", "checkpoint_interval_seconds": 300,
               "runner_phase": value["phase"], "goal_steps": value["goal_steps"],
               "required_resume_steps": value["resume_steps"], "rank_restored_steps": ranks,
               "missing_evidence": missing,
               "candidate_pass": value["phase"] == "completed" and same_step and not missing,
               "interpretation": "Candidate only: review checkpoint identity, Verified receipts, FIS/EC2 and all-rank audits."}
    summary["configuration"] = {key: value.get(key) for key in (
        "region", "availability_zone", "instance_type", "training_image", "training_code_sha256",
        "batch_size", "sequence_length", "learning_rate", "seed", "max_seconds", "started_at", "completed_at")}
    runner.save(export / "summary.json", summary)
    runner.save(export / "training-events.json", events)
    names = ["summary.json", "training-events.json"]
    snapshots = []
    for path in sorted((out / "snapshots").glob("*.json")):
        obj = runner.read(path)
        snapshots.append({"timestamp": obj.get("timestamp"), "resources": [
            {"cluster": r.get("cluster"), "resource": r.get("resource"),
             "items": [evidence_resource(item) for item in r.get("items", [])]}
            for r in obj.get("resources", [])]})
    runner.save(export / "snapshots.json", snapshots)
    names.append("snapshots.json")
    for name in ("e2-checkpoint.json", "e2-resources.json"):
        if (out / name).exists():
            runner.save(export / name, runner.read(out / name))
            names.append(name)
    timeline = []
    if (out / "runner-events.jsonl").exists():
        for line in (out / "runner-events.jsonl").read_text(encoding="utf-8").splitlines():
            obj = json.loads(line)
            timeline.append({k: obj[k] for k in ("timestamp", "event", "instance_id", "experiment_id") if k in obj})
    runner.save(export / "timeline.json", timeline)
    names.append("timeline.json")
    for name in ("fis-final.json", "fis-instance-final.json"):
        if (out / name).exists():
            obj = runner.read(out / name)
            if name == "fis-final.json":
                exp = obj.get("experiment", {})
                result = {"id": exp.get("id"), "state": exp.get("state", {}).get("status"),
                          "startTime": exp.get("startTime"), "endTime": exp.get("endTime")}
            else:
                result = [{"id": i["InstanceId"], "state": i["State"]["Name"]}
                          for r in obj.get("Reservations", []) for i in r.get("Instances", [])]
            runner.save(export / name, result)
            names.append(name)
    text = "# E2 recorded evidence (not a live server capture)\n\n"
    text += "```json\n" + json.dumps(summary, indent=2) + "\n```\n\n"
    text += "Capture summary.json, timeline.json, e2-checkpoint.json, e2-resources.json and training-events.json.\n"
    text += "Missing files mean missing evidence, not success. Keep failed runs. No reliability claim from one run.\n"
    (export / "capture-guide.md").write_text(text, encoding="utf-8")
    names.append("capture-guide.md")
    runner.save(export / "sha256.json", {n: hashlib.sha256((export / n).read_bytes()).hexdigest() for n in names})
    archive = out.parent / (value["run_id"] + "-e2-presentation.tar.gz")
    with tarfile.open(archive, "w:gz") as tar:
        for name in names + ["sha256.json"]:
            tar.add(export / name, arcname=value["run_id"] + "/" + name, recursive=False)
    print("DOWNLOAD:", archive)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("render")
    p.add_argument("--config", type=Path, default=Path("evaluation/config.gpt2.example.json"))
    p.add_argument("--policy-source", type=Path, required=True)
    p.add_argument("--run-id", required=True)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--goal-steps", type=int, default=10000)
    p.add_argument("--resume-steps", type=int, default=10)
    p.add_argument("--max-seconds", type=int, default=7200)
    for command in ("prepare", "start", "plan-interruption", "interrupt", "snapshot", "report", "stop", "abort", "cleanup"):
        p = sub.add_parser(command)
        p.add_argument("--evidence", type=Path, required=True)
        if command in ("prepare", "start", "interrupt", "stop", "abort", "cleanup"):
            p.add_argument("--confirm", required=True)
        if command == "plan-interruption":
            p.add_argument("--role-arn", required=True)
            p.add_argument("--alarm-arn", required=True)
    args = parser.parse_args()
    if args.command == "render":
        if args.resume_steps < 3 or args.goal_steps <= args.resume_steps:
            parser.error("resume-steps >= 3 and goal-steps > resume-steps required")
        render(args)
        return
    out = args.evidence
    info(out, getattr(args, "confirm", None))
    if args.command == "plan-interruption":
        checkpoint_gate(out)
        fis.plan(out, args.role_arn, args.alarm_arn)
    elif args.command == "interrupt":
        checkpoint_gate(out)
        snapshot(out)
        fis.start(out, args.confirm, False)
    elif args.command == "start":
        try:
            runner.start(out, completion_check=completion_check)
        finally:
            try:
                snapshot(out)
            except Exception as exc:
                runner.event(out, "e2_final_snapshot_failed", error_type=type(exc).__name__)
            report(out)
    elif args.command == "cleanup":
        runner.cleanup(out, args.confirm)
    elif args.command == "abort":
        runner.abort_failed_run(out, args.confirm)
    elif args.command in ("snapshot", "report"):
        globals()[args.command](out)
    else:
        getattr(runner, args.command)(out)


if __name__ == "__main__":
    main()
