"""Summarize collected run evidence as CSV without estimating missing values.

python analyze.py EVIDENCE [EVIDENCE ...] [--output summary.csv]
Optional per-directory billing.csv (or --billing-csv FILE shared by runs) has:
run_id,cost_type,instance_id,start,end,price_per_hour,currency,amount,description
VM rows use cost_type=vm, an explicit instance ID, timezone-aware [start,end),
and price_per_hour. Extra rows use cost_type=extra and explicit amount, including
zero to attest no extra cost. All selected rows need one consistent currency.
Costs cover the explicit ledger intervals, including provisioning if supplied;
they are not inferred from spot quotes, snapshot count, or training duration.
--baseline-run-id selects one included completed cost-A USD ledger; savings are
100 * (A.total_cost - B.total_cost) / A.total_cost for comparable cost-B runs.
Step times are observed rank-zero trainer durations (including replay), not CPU
time. Checkpoint phase time is startTime to completionTime, not measured pause
time; export time is completionTime to the latest per-file exportedAt. Totals
cover only observed completed rounds, not failed attempts or missing evidence.
"""

import argparse
import csv
from decimal import Decimal, DecimalException, InvalidOperation
import json
import math
from pathlib import Path
import sys

try:
    from .collect import INSTANCE, NAMESPACE, PARENT_LABEL, PARENT_UID, POLICY_LABEL, PREFIX, RUN_LABEL, event_from_message, load_run, timestamp
except ImportError:
    from collect import INSTANCE, NAMESPACE, PARENT_LABEL, PARENT_UID, POLICY_LABEL, PREFIX, RUN_LABEL, event_from_message, load_run, timestamp


def read_jsonl(path, issues):
    with path.open(encoding="utf-8") as handle:
        for number, line in enumerate(handle, 1):
            try:
                record = json.loads(line)
                if not isinstance(record, dict):
                    raise ValueError("record must be an object")
                yield record
            except ValueError:
                issues.append("invalid_jsonl:" + path.name + ":" + str(number))


def training_metrics(evidence, run, issues):
    events, seen = [], set()
    paths = sorted((evidence / "logs").glob("*/*.jsonl"))
    monitor = evidence / "monitor-events.jsonl"
    if monitor.exists():
        paths.insert(0, monitor)
    for path in paths:
        for record in read_jsonl(path, issues):
            try:
                message = PREFIX + json.dumps(record) if path == monitor else record.get("message")
                event = event_from_message(message)
                if event is None or event["run_id"] != run["run_id"]:
                    continue
                at = timestamp(event["timestamp"])
                if run.get("started_at") and at < timestamp(run["started_at"]):
                    continue
                # Event identity is source-independent: monitor and UID logs overlap.
                session = event.get("attempt_id", event.get("session_id", event.get("session", "")))
                key = (event["timestamp"], session, event["rank"], event["global_step"], event["event"])
                if key in seen:
                    continue
                seen.add(key)
                events.append((at, event))
            except (ValueError, TypeError):
                issues.append("invalid_training_event:" + path.name)
    events.sort(key=lambda item: item[0])
    rank_zero = [(at, event) for at, event in events if event["rank"] == 0]
    steps = [event["global_step"] for _, event in rank_zero if event["event"] == "step"]
    durations = [event["step_seconds"] for _, event in rank_zero
                 if event["event"] == "step" and "step_seconds" in event]
    duration_total = math.fsum(durations) if durations else ""
    completed = [(at, event) for at, event in events if event["event"] == "completed" and event["global_step"] >= run["goal_steps"]]
    progress = [event["global_step"] for _, event in rank_zero if event["event"] in {"step", "completed"}]
    rank_completion = {}
    for at, event in completed:
        rank_completion.setdefault(event["rank"], at)
    world_size = run.get("world_size", 2)
    all_ranks = all(rank in rank_completion for rank in range(world_size))
    goal_completed = all_ranks if events else ""
    end = max(rank_completion[rank] for rank in range(world_size)) if all_ranks else None
    # Training completion is event evidence; runner phase governs eligibility.
    elapsed = ""
    if end is not None and run.get("started_at"):
        elapsed = (end - timestamp(run["started_at"])).total_seconds()
    # These are observed counts, not an extrapolation across log gaps.
    return {
        "observed_attempted_steps": len(steps) if steps else "",
        "observed_unique_steps": len(set(steps)) if steps else "",
        "observed_replayed_steps": len(steps) - len(set(steps)) if steps else "",
        "observed_step_time_samples": len(durations) if steps else "",
        "observed_step_seconds_total": duration_total,
        "observed_step_seconds_mean": duration_total / len(durations) if durations else "",
        "goal_steps_per_second": run["goal_steps"] / elapsed if elapsed != "" and elapsed > 0 else "",
        "max_global_step": max(progress) if progress else "",
        "goal_completed": goal_completed,
        "all_rank_completion_observed": all_ranks if events else "",
        "completion_time": end.isoformat().replace("+00:00", "Z") if end else "",
        "time_to_goal_seconds": elapsed,
        "observed_restores": sum(event["event"] == "restored" for _, event in rank_zero) if rank_zero else "",
    }


def completed_rounds(obj, run, parent_uids=frozenset(), timings=None):
    meta = obj.get("metadata", {})
    spec = obj.get("spec", {})
    labels = meta.get("labels", {})
    annotations = meta.get("annotations", {})
    parent_uid = annotations.get(PARENT_UID)
    parent_bound = parent_uid in parent_uids and labels.get(PARENT_LABEL) == parent_uid
    ref = spec.get("workloadRef", {})
    if (meta.get("namespace") != NAMESPACE or not run.get("workload_uid") or
            ref.get("uid") != run["workload_uid"] or ref.get("name") != run["app"] or
            (labels.get(POLICY_LABEL) != run["policy"] and not parent_bound) or
            labels.get(RUN_LABEL, run["run_id"]) != run["run_id"] or not run.get("started_at")):
        return
    status = obj.get("status", {})
    reports = status.get("clusters", [status])
    for report in reports:
        if report.get("clusterName", "aws") != "aws":
            continue
        retained = report.get("lastSuccessfulFullCheckpoint")
        if isinstance(retained, dict):
            result = retained.get("result", {})
            child = {"metadata": {**meta, "uid": retained.get("uid"),
                      "annotations": {"training.dcnlab.com/checkpoint-id": retained.get("checkpointID")}},
                     "spec": result.get("spec", {}), "status": result.get("status", {})}
            child["metadata"].pop("generation", None)  # Parent schedule generation is not the child's.
            yield from completed_rounds(child, run, parent_uids, timings)
        # A Completed persistent schedule is not itself a completed checkpoint.
        if "schedule" in spec or report.get("phase") != "Completed":
            continue
        annotations = meta.get("annotations", {})
        uid = annotations.get("training.dcnlab.com/source-checkpoint-uid") or meta.get("uid")
        identity = annotations.get("training.dcnlab.com/checkpoint-id") or report.get("checkpointID")
        if not uid or not identity:
            continue
        generation = meta.get("generation")
        if generation is not None and report.get("observedGeneration") != generation:
            continue
        try:
            start = timestamp(report["startTime"])
            end = timestamp(report["completionTime"])
            if start < timestamp(run["started_at"]) or end < start:
                continue
            run_end = run.get("completed_at") if run.get("phase") == "completed" else run.get("ended_at")
            if run_end and end > timestamp(run_end):
                continue
        except (KeyError, ValueError, TypeError):
            continue
        key = (uid, identity)
        if timings is not None:
            phase_seconds = (end - start).total_seconds()
            copy_seconds = None
            try:
                pods = report["pods"]
                if not pods or any(not pod.get("checkpointFiles") for pod in pods):
                    raise ValueError("missing archive timing")
                exports = []
                for pod in pods:
                    for file in pod["checkpointFiles"]:
                        checkpoint_at = timestamp(file["checkpointTime"])
                        exported_at = timestamp(file["exportedAt"])
                        if checkpoint_at < start or exported_at < checkpoint_at:
                            raise ValueError("invalid archive timing")
                        exports.append(exported_at)
                last_export = max(exports)
                if last_export >= end and (not run_end or last_export <= timestamp(run_end)):
                    copy_seconds = (last_export - end).total_seconds()
            except (KeyError, ValueError, TypeError, AttributeError):
                pass
            previous = timings.get(key)
            if previous is None or (previous[1] is None and copy_seconds is not None):
                timings[key] = (phase_seconds, copy_seconds)
        yield key


def snapshot_metrics(evidence, run, issues):
    rounds, instances = set(), set()
    timings = {}
    migrations, policies = [], []
    checkpoint_observed = False
    snapshots = 0
    for path in sorted((evidence / "snapshots").glob("*.json")):
        try:
            snap = json.loads(path.read_text(encoding="utf-8"))
            if snap.get("run_id") != run["run_id"]:
                continue
            timestamp(snap["timestamp"])
            snapshots += 1
            for resource in snap.get("resources", []):
                kind = resource.get("resource")
                if kind == "fluidcrmigrations" and run.get("workload_uid") and run.get("started_at"):
                    checkpoint_observed = True
                    migrations.extend(resource.get("items", []))
                if kind == "trainingpolicies":
                    policies.extend(resource.get("items", []))
                if kind == "nodeprovisions":
                    for obj in resource.get("items", []):
                        meta = obj.get("metadata", {})
                        identity = obj.get("status", {}).get("instanceId", "")
                        if meta.get("namespace") == NAMESPACE and meta.get("labels", {}).get(POLICY_LABEL) == run["policy"] and INSTANCE.fullmatch(identity):
                            instances.add(identity)
        except (ValueError, KeyError, TypeError, AttributeError):
            issues.append("invalid_snapshot:" + path.name)
    parent_uids = {obj.get("metadata", {}).get("uid") for obj in migrations
                   if "schedule" in obj.get("spec", {})
                   and obj.get("metadata", {}).get("namespace") == NAMESPACE
                   and obj.get("metadata", {}).get("labels", {}).get(POLICY_LABEL) == run["policy"]
                   and obj.get("spec", {}).get("workloadRef", {}).get("uid") == run.get("workload_uid")
                   and obj.get("spec", {}).get("workloadRef", {}).get("name") == run["app"]}
    parent_uids.discard(None)
    for obj in migrations:
        rounds.update(completed_rounds(obj, run, parent_uids, timings))
    phase_times = [value[0] for value in timings.values()]
    copy_times = [value[1] for value in timings.values() if value[1] is not None]
    policy_evidence = policy_metrics(policies, run)
    return {"snapshots": snapshots if snapshots else "",
            "observed_completed_checkpoints": len(rounds) if checkpoint_observed else "",
            "observed_checkpoint_phase_seconds_total": sum(phase_times) if phase_times else "",
            "observed_checkpoint_phase_seconds_mean": sum(phase_times) / len(phase_times) if phase_times else "",
            "observed_checkpoint_export_samples": len(copy_times) if rounds else "",
            "observed_checkpoint_export_seconds_total": sum(copy_times) if copy_times else "",
            "observed_checkpoint_export_seconds_mean": sum(copy_times) / len(copy_times) if copy_times else "",
            "observed_instance_ids": ";".join(sorted(instances)), **policy_evidence}, instances


def policy_metrics(policies, run):
    observed, analytic, composition, violations = False, set(), set(), set()
    expected = (1, 1) if run["experiment"] == "checkpoint" else ((2, 0) if run["arm"] == "A" else None)
    for obj in policies:
        meta, spec, status = obj.get("metadata", {}), obj.get("spec", {}), obj.get("status", {})
        if (meta.get("name") != run["policy"] or meta.get("namespace") != NAMESPACE or
                not run.get("workload_uid") or spec.get("workloadRef", {}).get("uid") != run["workload_uid"] or
                (run.get("policy_uid") and meta.get("uid") != run["policy_uid"])):
            continue
        observed = True
        checkpoint = status.get("checkpoint", {})
        if (checkpoint.get("intervalCostEvaluated") is True and
                checkpoint.get("intervalSource") in {"checkpoint-cost-analytic", "paper-equations-1-5-analytic"}):
            analytic.add((checkpoint.get("observedAt"), checkpoint.get("checkpointIntervalSeconds")))
        if run["experiment"] == "checkpoint" and (spec.get("policy", {}).get("fixedOnDemand") != 1 or spec.get("targetWorkers") != 2):
            violations.add("spec")
        decision = status.get("policy", {})
        counts = decision.get("onDemandWorkers"), decision.get("spotWorkers")
        if expected and all(type(value) is int for value in counts) and decision.get("provisioningBlocked") is not True:
            identity = (decision.get("observedAt"), *counts)
            composition.add(identity)
            if counts != expected:
                violations.add(identity)
    return {"analytic_interval_observed": bool(analytic) if observed else "",
            "analytic_interval_samples": len(analytic) if observed else "",
            "fixed_composition_verified": bool(composition) and not violations if expected and observed else "",
            "fixed_composition_violations": len(violations) if expected and observed else ""}


def nonnegative_decimal(value):
    try:
        result = Decimal(value)
    except (InvalidOperation, TypeError):
        raise ValueError("missing_or_invalid_amount") from None
    if not result.is_finite() or result < 0:
        raise ValueError("negative_or_nonfinite_amount")
    return result


def billing_metrics(path, run, instances, issues):
    empty = {"currency": "", "vm_cost": "", "extra_cost": "", "total_cost": "",
             "billing_status": "missing", "billing_basis": ""}
    if not path.exists():
        return empty
    totals = {"vm": Decimal(0), "extra": Decimal(0)}
    counts = {"vm": 0, "extra": 0}
    currencies, billed_ids = set(), set()
    intervals = {}
    problems = []
    try:
        with path.open(encoding="utf-8-sig", newline="") as handle:
            reader = csv.DictReader(handle)
            required = {"run_id", "cost_type", "instance_id", "start", "end", "price_per_hour", "currency", "amount"}
            if not required.issubset(reader.fieldnames or []):
                raise ValueError("missing_ledger_columns")
            for number, row in enumerate(reader, 2):
                if row["run_id"] != run["run_id"]:
                    continue
                try:
                    kind = row["cost_type"]
                    if kind not in totals:
                        raise ValueError("invalid_cost_type")
                    currency = row["currency"]
                    if len(currency) != 3 or not currency.isalpha() or not currency.isupper():
                        raise ValueError("invalid_currency")
                    currencies.add(currency)
                    if kind == "vm":
                        identity = row["instance_id"]
                        if not INSTANCE.fullmatch(identity):
                            raise ValueError("invalid_instance_id")
                        if instances and identity not in instances:
                            raise ValueError("instance_not_in_run_evidence")
                        start, end = timestamp(row["start"]), timestamp(row["end"])
                        if end <= start:
                            raise ValueError("nonpositive_billing_interval")
                        intervals.setdefault(identity, []).append((start, end))
                        billed_ids.add(identity)
                        delta = end - start
                        seconds = Decimal(delta.days * 86400 + delta.seconds) + Decimal(delta.microseconds) / 1000000
                        value = nonnegative_decimal(row["price_per_hour"]) * seconds / 3600
                    else:
                        value = nonnegative_decimal(row["amount"])
                    totals[kind] += value
                    counts[kind] += 1
                except (ValueError, TypeError, DecimalException) as exc:
                    # Do not echo arbitrary CSV values into summary output.
                    problems.append("invalid_billing_row:" + str(number) + ":" + type(exc).__name__)
        for values in intervals.values():
            values.sort()
            if any(right[0] < left[1] for left, right in zip(values, values[1:])):
                problems.append("overlapping_billing_intervals")
        if len(currencies) > 1:
            problems.append("mixed_currencies")
        if counts["vm"] and instances - billed_ids:
            problems.append("missing_instance_billing")
    except (OSError, ValueError, csv.Error):
        problems.append("invalid_billing_ledger")
    if problems:
        issues.extend(problems)
        return {**empty, "billing_status": "invalid"}
    if not any(counts.values()):
        return empty
    result = {**empty, "billing_status": "validated", "billing_basis": "explicit_ledger_intervals",
              "currency": next(iter(currencies), "")}
    for kind in totals:
        if counts[kind]:
            result[kind + "_cost"] = str(totals[kind])
    if counts["vm"] and counts["extra"]:
        result["total_cost"] = str(totals["vm"] + totals["extra"])
    return result


def summarize(evidence, billing_csv=None):
    evidence = Path(evidence)
    run = load_run(evidence)
    issues = []
    result = {key: run[key] for key in ("run_id", "experiment", "arm", "model", "goal_steps")}
    result.update({key: run.get(key, "") for key in ("instance_type", "batch_size", "seed", "training_image", "dataset")})
    result.update(world_size=run.get("world_size", 2), region=run.get("region", "ap-northeast-2"))
    result.update(phase=run.get("phase", ""), failure_reason=run.get("failure_reason", ""))
    result.update(training_metrics(evidence, run, issues))
    metrics, instances = snapshot_metrics(evidence, run, issues)
    result.update(metrics)
    result.update(billing_metrics(Path(billing_csv) if billing_csv else evidence / "billing.csv", run, instances, issues))
    result["cost_per_goal_step"] = (str(Decimal(result["total_cost"]) / run["goal_steps"])
                                    if run.get("phase") == "completed" and
                                    result["all_rank_completion_observed"] is True and result["total_cost"] != "" else "")
    error_path = evidence / "errors.jsonl"
    result["collection_errors"] = sum(1 for _ in read_jsonl(error_path, issues)) if error_path.exists() else ""
    exclusions = []
    if run.get("phase") != "completed":
        exclusions.append("run_" + str(run.get("phase", "outcome_missing")))
    if result["all_rank_completion_observed"] is not True:
        exclusions.append("all_rank_completion_evidence_missing")
    if run["arm"] == "D" and result["analytic_interval_observed"] is not True:
        exclusions.append("D_missing_analytic_evidence")
    if run["experiment"] == "checkpoint" or run["arm"] == "A":
        if result["fixed_composition_violations"]:
            exclusions.append("fixed_composition_violation")
        elif result["fixed_composition_verified"] is not True:
            exclusions.append("fixed_composition_evidence_missing")
    result["excluded"] = bool(exclusions)
    result["exclusion_reasons"] = ";".join(exclusions)
    result["issues"] = ";".join(sorted(set(issues)))
    return result


def baseline_savings(rows, baseline_run_id=None):
    for row in rows:
        row.update(baseline_run_id=baseline_run_id or "", cost_saving_percent="",
                   cost_comparison_status="not_requested")
    if baseline_run_id is None:
        return
    matches = [row for row in rows if row["run_id"] == baseline_run_id]

    def eligible(row):
        return (row["experiment"] == "cost" and row["phase"] == "completed" and
                row.get("all_rank_completion_observed") is True and
                not row["excluded"] and row["billing_status"] == "validated" and
                row["currency"] == "USD" and row["total_cost"] != "")

    required = ("model", "goal_steps", "instance_type", "batch_size", "seed", "dataset")
    if (len(matches) != 1 or matches[0]["arm"] != "A" or not eligible(matches[0]) or
            Decimal(matches[0]["total_cost"]) <= 0 or
            any(matches[0].get(key, "") in ("", None) for key in required)):
        raise ValueError("baseline requires one included completed cost-A run with positive USD total and comparison fields")
    baseline = matches[0]
    compare = (*required, "world_size", "region", "training_image")
    for row in rows:
        if row is baseline:
            row["cost_comparison_status"] = "baseline"
        elif row["experiment"] != "cost" or row["arm"] != "B":
            row["cost_comparison_status"] = "not_cost_B"
        elif not eligible(row):
            row["cost_comparison_status"] = "ineligible_run_or_billing"
        elif any(row.get(key, "") != baseline.get(key, "") for key in compare):
            row["cost_comparison_status"] = "configuration_mismatch"
        else:
            a, b = Decimal(baseline["total_cost"]), Decimal(row["total_cost"])
            row["cost_saving_percent"] = str(100 * (a - b) / a)
            row["cost_comparison_status"] = "comparable"


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("evidence", nargs="+", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--billing-csv", type=Path)
    parser.add_argument("--baseline-run-id")
    args = parser.parse_args(argv)
    try:
        rows = [summarize(path, args.billing_csv) for path in args.evidence]
        baseline_savings(rows, args.baseline_run_id)
    except (OSError, ValueError) as exc:
        parser.exit(2, "analysis failed: " + type(exc).__name__ + "\n")
    handle = args.output.open("w", newline="", encoding="utf-8") if args.output else sys.stdout
    try:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    finally:
        if args.output:
            handle.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
