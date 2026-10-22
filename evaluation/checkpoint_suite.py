#!/usr/bin/env python3
"""Local config generation and explicitly authorized, bounded FIS scheduling."""
import argparse
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import time
import fis
import run as runner


def configure(base, output, steps, timeout, scenario):
    if steps <= 0 or timeout <= 1800:
        raise ValueError("positive steps and timeout > 1800 required")
    config = runner.read(base)
    # Preserve the policy source when the generated config is in another directory.
    config["policy_source"] = str((base.resolve().parent / config["policy_source"]).resolve())
    config.update(model="resnet50", restore_audit=False, batch_size=32,
                  goal_steps=steps, max_seconds=timeout, scenario=scenario,
                  initial_risk=0.05)
    config["risk_schedule"] = [
        {"after_seconds": 600, "lambda_per_hour": 0.8},
        {"after_seconds": 1800, "lambda_per_hour": 0.05},
    ]
    runner.validate(config, "r50check", "checkpoint", "D")
    with output.open("x", encoding="utf-8") as handle:
        json.dump(config, handle, indent=2)
        handle.write("\n")


def elapsed_since_progress(evidence, now):
    path = evidence / "runner-events.jsonl"
    if not path.exists():
        return None
    for line in path.read_text(encoding="utf-8").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue  # A concurrent writer may not have finished its final line.
        if event.get("event") == "all_ranks_progress_observed":
            start = datetime.fromisoformat(event["timestamp"].replace("Z", "+00:00"))
            return (now - start).total_seconds()
    return None


def gate(run, confirm, offset, tolerance, elapsed):
    if run.get("run_id") != confirm or run.get("experiment") != "checkpoint":
        raise ValueError("exact checkpoint run-id confirmation required")
    if run.get("scenario") != "interruption" or run.get("phase") != "running":
        raise ValueError("requires running interruption scenario")
    if not all(math.isfinite(x) for x in (offset, tolerance)) or offset <= 0 or not 0 <= tolerance <= 60:
        raise ValueError("positive offset and tolerance 0..60 seconds required")
    if offset >= run["max_seconds"]:
        raise ValueError("offset must precede run timeout")
    if elapsed is None or elapsed < offset:
        return False
    if elapsed > offset + tolerance:
        raise ValueError("missed injection window; do not shift the experiment")
    return True


def interrupt(evidence, confirm, offset, tolerance, wait_seconds, allow_no_alarm):
    if not math.isfinite(wait_seconds) or not 0 < wait_seconds <= 14400:
        raise ValueError("wait-seconds must be in (0, 14400]")
    # Require the reviewed plan before waiting. Never plan or select a new target here.
    runner.read(evidence / "fis-template.json")
    runner.read(evidence / "fis-target.json")
    deadline = time.monotonic() + wait_seconds
    while time.monotonic() < deadline:
        run = runner.read(evidence / "run.json")
        if (evidence / "fis-started.json").exists():
            raise ValueError("FIS already requested; inspect AWS, never automatically retry")
        elapsed = elapsed_since_progress(evidence, datetime.now(timezone.utc))
        if gate(run, confirm, offset, tolerance, elapsed):
            runner.event(evidence, "scheduled_interruption_due",
                         after_seconds=offset, actual_after_seconds=elapsed)
            fis.start(evidence, confirm, allow_no_alarm)
            return
        time.sleep(1)
    raise TimeoutError("bounded FIS wait expired; no interruption requested")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    c = commands.add_parser("configure")
    c.add_argument("--base", type=Path, required=True)
    c.add_argument("--out", type=Path, required=True)
    c.add_argument("--steps", type=int, default=22000)
    c.add_argument("--timeout", type=int, default=14400)
    c.add_argument("--scenario", choices=["risk-schedule", "interruption"], default="interruption")
    c = commands.add_parser("interrupt")
    c.add_argument("--evidence", type=Path, required=True)
    c.add_argument("--confirm", required=True)
    c.add_argument("--after-seconds", type=float, default=1500)
    c.add_argument("--tolerance-seconds", type=float, default=30)
    c.add_argument("--wait-seconds", type=float, default=7200)
    c.add_argument("--allow-no-alarm", action="store_true")
    args = parser.parse_args()
    if args.command == "configure":
        configure(args.base, args.out, args.steps, args.timeout, args.scenario)
    else:
        interrupt(args.evidence, args.confirm, args.after_seconds, args.tolerance_seconds,
                  args.wait_seconds, args.allow_no_alarm)


if __name__ == "__main__":
    main()
