"""Export sanitized collected rank events using the v2 evaluation CSV contract.

Does not infer completion, cost, downtime, or useful-token throughput. Replay
attempts remain separate so downstream analysis can count useful work correctly.
"""
import argparse
import csv
import json
from pathlib import Path

try:
    from .collect import NAME, PREFIX, event_from_message, timestamp
except ImportError:
    from collect import NAME, PREFIX, event_from_message, timestamp

FIELDS = ("run_id", "timestamp_utc", "event", "rank", "global_step",
          "instance_id", "policy_id", "pod_uid", "attempt_id", "details")


def export(source, output, run_id, policy_id="", node_instances=None):
    node_instances = node_instances or {}
    if policy_id and not NAME.fullmatch(policy_id):
        raise ValueError("invalid policy identifier")
    if any(not NAME.fullmatch(k) or not NAME.fullmatch(v) for k, v in node_instances.items()):
        raise ValueError("invalid node/instance mapping")
    rows = {0: [], 1: []}
    seen = set()
    # Validate the entire input before creating any output.
    for line in Path(source).read_text(encoding="utf-8").splitlines():
        if not line.strip():
            continue
        raw = json.loads(line)
        event = event_from_message(PREFIX + json.dumps(raw))
        if event is None or event["run_id"] != run_id or event["rank"] not in rows:
            raise ValueError("input must contain only the requested two-rank run")
        timestamp(event["timestamp"])
        uid = raw.get("pod_uid", "")
        attempt = event.get("attempt_id", "")
        if uid and not NAME.fullmatch(uid):
            raise ValueError("invalid Pod UID")
        key = (uid, attempt, event["rank"], event["event"],
               event["global_step"], event["timestamp"])
        if key in seen:
            continue
        seen.add(key)
        # Only validated evaluation fields enter details. Never copy arbitrary
        # environment/Secret/Pod fields from the input JSON.
        rows[event["rank"]].append(dict(
            run_id=run_id, timestamp_utc=event["timestamp"], event=event["event"],
            rank=event["rank"], global_step=event["global_step"],
            instance_id=node_instances.get(raw.get("node", ""), ""),
            policy_id=policy_id, pod_uid=uid, attempt_id=attempt,
            details=json.dumps(event, sort_keys=True, ensure_ascii=False)))
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    for rank, events in rows.items():
        with (output / f"rank{rank}_training.csv").open("w", newline="", encoding="utf-8") as handle:
            writer = csv.DictWriter(handle, fieldnames=FIELDS)
            writer.writeheader()
            writer.writerows(events)
    return {rank: len(events) for rank, events in rows.items()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--events", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--policy-id", default="")
    parser.add_argument("--node-instances", type=Path, help="Reviewed JSON object mapping node name to instance ID")
    args = parser.parse_args()
    mapping = json.loads(args.node_instances.read_text()) if args.node_instances else {}
    print(json.dumps(export(args.events, args.output, args.run_id, args.policy_id, mapping)))


if __name__ == "__main__":
    main()
