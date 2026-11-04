"""Arm one E2 interruption 20 minutes after both ranks report a step."""
import argparse
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import time
import e2_experiment as e2
import fis
import run as runner


def anchor(path, run_id):
    ranks = {}
    if path.exists():
        for line in path.read_text(encoding="utf-8").splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            rank = event.get("rank")
            if event.get("run_id") == run_id and event.get("event") == "step" and rank in (0, 1):
                stamp = datetime.fromisoformat(event["timestamp"].replace("Z", "+00:00"))
                if stamp.tzinfo is None:
                    raise ValueError("timestamp needs timezone")
                ranks.setdefault(rank, stamp)
    return max(ranks.values()) if len(ranks) == 2 else None


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--evidence", type=Path, required=True)
    for name in ("confirm", "instance-id", "role-arn", "alarm-arn"):
        p.add_argument("--" + name, required=True)
    a = p.parse_args()
    out = a.evidence
    value = e2.info(out, a.confirm)
    if value.get("phase") != "running" or (out / "fis-started.json").exists():
        raise ValueError("requires running E2 without previous FIS")
    target = fis.target(value)
    if target["instance_id"] != a.instance_id:
        raise ValueError("explicit instance is not the owned Spot")
    with (out / "fis-schedule.json").open("x", encoding="utf-8") as f:
        json.dump({"state": "armed", "target": target}, f)
    while True:
        if e2.info(out).get("phase") != "running":
            raise ValueError("runner stopped; no injection")
        start = anchor(out / "monitor-events.jsonl", value["run_id"])
        if start is not None:
            break
        time.sleep(5)
    due = start + timedelta(minutes=20)
    if datetime.now(timezone.utc) >= due:
        raise ValueError("deadline passed; refusing late injection")
    runner.save(out / "fis-schedule.json", {"state": "waiting", "target": target,
                "training_start": start.isoformat(), "scheduled_at": due.isoformat()})
    print("Scheduled FIS request:", due.isoformat(), target, flush=True)
    while datetime.now(timezone.utc) < due:
        if e2.info(out).get("phase") != "running":
            raise ValueError("runner stopped; no injection")
        time.sleep(1)
    if (datetime.now(timezone.utc) - due).total_seconds() > 30:
        raise ValueError("deadline missed; no injection")
    if fis.target(e2.info(out)) != target:
        raise ValueError("target changed; no injection")
    alarms = fis.aws(value["region"], "cloudwatch", "describe-alarms",
                     "--alarm-names", a.alarm_arn.split(":alarm:")[-1])
    if not any(x.get("AlarmArn") == a.alarm_arn and x.get("StateValue") == "OK"
               for x in alarms.get("MetricAlarms", [])):
        raise ValueError("matching stop alarm must be OK")
    e2.checkpoint_gate(out)
    fis.plan(out, a.role_arn, a.alarm_arn)
    e2.snapshot(out)
    if (datetime.now(timezone.utc) - due).total_seconds() > 30:
        raise ValueError("validation exceeded deadline tolerance; no injection")
    fis.start(out, a.confirm, False)


if __name__ == "__main__":
    main()
