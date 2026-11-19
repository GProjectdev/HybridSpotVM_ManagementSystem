"""Offline E3 evidence report; no cloud calls or third-party dependencies."""

import argparse
from datetime import datetime, timezone
from decimal import Decimal
import hashlib
import io
import json
import math
from pathlib import Path
import re
import tarfile

from collect import event_from_message, safe_resource


PLAN_NUMBERS = ("window_seconds", "provisioning_timeout_seconds", "fixed_interval_seconds",
                "risk_after_seconds", "fault_after_seconds", "fault_tolerance_seconds",
                "initial_risk", "changed_risk", "alpha", "forecast_horizon_seconds")
DECISION_NUMBERS = ("intervalSeconds", "checkpointIntervalSeconds", "onDemandWorkers", "spotWorkers",
                    "desiredWorkers", "targetWorkers", "fixedOnDemand", "minOnDemand", "lambdaPerHour",
                    "minIntervalSeconds", "maxIntervalSeconds", "forecastHorizonSeconds", "alpha")


def numeric_fields(value, keys):
    if not isinstance(value, dict):
        return {}
    return {key: value[key] for key in keys if type(value.get(key)) in (int, float)
            and math.isfinite(value[key]) and value[key] >= 0}


def plan_projection(plan):
    result = numeric_fields(plan, PLAN_NUMBERS)
    if type(plan.get("pilot")) is bool:
        result["pilot"] = plan["pilot"]
    digest = plan.get("plan_sha256")
    if isinstance(digest, str) and re.fullmatch(r"[0-9a-fA-F]{64}", digest):
        result["plan_sha256"] = digest.lower()
    return result


def timestamp(value):
    if not isinstance(value, str):
        raise ValueError("timestamp must be an ISO string")
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ValueError("timestamp requires timezone")
    return result.astimezone(timezone.utc)


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8"))


def identifier(value):
    return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.-]{1,128}", value) else None


def observations(run, warnings):
    analytic = run.get("analytic_interval_observed")
    if type(analytic) is not bool:
        analytic = None
        warnings.append("analytic_interval_observed missing or invalid")
    result = []
    entries = run.get("risk_schedule_observations")
    if not isinstance(entries, list):
        warnings.append("risk_schedule_observations missing or invalid")
        entries = []
    for entry in entries:
        if not isinstance(entry, dict):
            warnings.append("invalid risk schedule observation")
            continue
        safe = {}
        for key in ("after_seconds", "actual_after_seconds", "lambda_per_hour"):
            value = entry.get(key)
            if type(value) in (int, float) and math.isfinite(value) and value >= 0:
                safe[key] = value
        for key in ("requested_at", "consumed_at"):
            if key in entry:
                try:
                    safe[key] = timestamp(entry[key]).isoformat()
                except (ValueError, TypeError):
                    warnings.append("invalid risk observation timestamp")
        if "decision" in entry:
            safe["decision"] = numeric_fields(entry["decision"], DECISION_NUMBERS)
        result.append(safe)
    return analytic, result


def progress(path, run_id, start, end, warnings):
    events = {0: [], 1: []}
    if not path.exists():
        warnings.append("missing monitor-events.jsonl")
    else:
        for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if not line.strip():
                continue
            try:
                raw = json.loads(line)
                if not isinstance(raw, dict):
                    raise ValueError()
                if "run_id" in raw and raw["run_id"] != run_id:
                    warnings.append("foreign run event ignored at line %d" % number)
                    continue
                if raw.get("event") not in ("step", "completed"):
                    # Validate audit receipts, but never use them as progress samples.
                    event_from_message("EVAL_EVENT " + json.dumps(raw))
                    continue
                rank = raw.get("rank")
                if type(rank) is not int or rank not in events:
                    raise ValueError()
                when = timestamp(raw.get("timestamp"))
                if identifier(raw.get("attempt_id")) is None:
                    raise ValueError()
                for key in ("global_step", "tokens_seen"):
                    if type(raw.get(key)) is not int or raw[key] < 0:
                        raise ValueError()
                safe = {key: raw[key] for key in ("rank", "attempt_id", "global_step", "tokens_seen")}
                safe["timestamp"] = when.isoformat()
                events[rank].append((when, number, safe))
            except (ValueError, TypeError, OverflowError):
                warnings.append("invalid monitor event at line %d; possible log gap" % number)
    ranks = {}
    for rank, entries in events.items():
        entries.sort(key=lambda item: (item[0], item[1]))
        before = [entry for entry in entries if entry[0] <= start]
        inside = [entry for entry in entries if start <= entry[0] <= end]
        baseline = before[-1][2] if before else None
        latest = inside[-1][2] if inside else None
        ranks[str(rank)] = {"baseline": baseline, "latest": latest}
        if baseline is None or latest is None:
            warnings.append("rank %d missing baseline or in-window progress" % rank)
        if baseline and timestamp(baseline["timestamp"]) < start:
            warnings.append("rank %d baseline precedes start; boundary log gap" % rank)
        if latest and timestamp(latest["timestamp"]) < end:
            warnings.append("rank %d latest precedes end; boundary log gap" % rank)
        if any(b[2]["attempt_id"] == a[2]["attempt_id"] and
               b[2]["global_step"] > a[2]["global_step"] + 1
               for a, b in zip(inside, inside[1:])):
            warnings.append("rank %d sparse step logs; intermediate progress unobserved" % rank)
        if any(a[0] == b[0] and a[2] != b[2] for a, b in zip(entries, entries[1:]) if a[0] <= end):
            warnings.append("rank %d conflicting same-time events; progress unavailable" % rank)
            ranks[str(rank)]["latest"] = None
    totals = {"global_step": None, "tokens_seen": None}
    if all(r["baseline"] is not None and r["latest"] is not None for r in ranks.values()):
        for key in totals:
            # Counters represent shared progress, not a sum across ranks.
            totals[key] = min(r["latest"][key] for r in ranks.values()) - min(
                r["baseline"][key] for r in ranks.values())
            if totals[key] < 0:
                warnings.append("negative net %s after rollback; retained without clamping" % key)
        if any(ranks["0"]["latest"][key] != ranks["1"]["latest"][key] for key in totals):
            warnings.append("latest rank counters differ; minimum shared counter used")
    warnings.append("log completeness unverified; absence of detected gaps is not proof of complete logging")
    return ranks, totals


def estimate_cost(path, start, end, useful_tokens=None):
    if path is None:
        return None
    rows = read_json(Path(path))
    if not isinstance(rows, list):
        raise ValueError("cost ledger must be a JSON list")
    seen, items = set(), []
    total = Decimal(0)
    lifecycle_total = Decimal(0)
    for row in rows:
        if not isinstance(row, dict):
            raise ValueError("invalid cost ledger entry")
        instance = identifier(row.get("instance_id"))
        market = row.get("market")
        if not instance or instance in seen:
            raise ValueError("invalid or duplicate instance_id")
        if market not in ("spot", "on-demand", "ondemand", "on_demand", "Spot", "On-Demand"):
            raise ValueError("invalid market")
        seen.add(instance)
        first, last = timestamp(row.get("start")), timestamp(row.get("end"))
        rate = row.get("usd_per_hour")
        if last <= first or type(rate) not in (int, float) or not math.isfinite(rate) or rate < 0:
            raise ValueError("invalid lifecycle or usd_per_hour")
        seconds = max(0.0, (min(last, end) - max(first, start)).total_seconds())
        cost = Decimal(str(seconds)) * Decimal(str(rate)) / Decimal(3600)
        lifecycle_seconds = (last - first).total_seconds()
        lifecycle_cost = Decimal(str(lifecycle_seconds)) * Decimal(str(rate)) / Decimal(3600)
        total += cost
        lifecycle_total += lifecycle_cost
        items.append({"instance_id": instance, "market": market,
                      "start": first.isoformat(), "end": last.isoformat(), "usd_per_hour": rate,
                      "window_seconds": seconds, "estimated_usd": float(cost),
                      "window_estimated_usd": float(cost), "lifecycle_seconds": lifecycle_seconds,
                      "lifecycle_estimated_usd": float(lifecycle_cost)})
    if not all(math.isfinite(float(value)) for value in (total, lifecycle_total)):
        raise ValueError("cost estimate overflow")
    def per_million(amount):
        if type(useful_tokens) is not int or useful_tokens <= 0:
            return None
        value = float(amount * Decimal(1000000) / Decimal(useful_tokens))
        if not math.isfinite(value):
            raise ValueError("cost per useful token overflow")
        return value
    return {"estimated_usd": float(total), "window_estimated_usd": float(total),
            "lifecycle_estimated_usd": float(lifecycle_total), "instances": items,
            "usd_per_useful_million_tokens": per_million(lifecycle_total),
            "window_usd_per_useful_million_tokens": per_million(total),
            "useful_tokens_denominator": useful_tokens,
            "ratio_basis": "Lifecycle estimate divided by observed useful window tokens; "
                           "window ratio uses only window cost. estimated_usd is the window estimate.",
            "label": "Estimate from user-supplied lifecycle ledger; completeness unverified",
            "ledger_completeness_verified": False}


def monitor_audit(path, run_id, warnings):
    """Use the collector's event contract without retaining unknown log fields."""
    result = []
    if not path.exists():
        return result
    for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
        if not line.strip():
            continue
        try:
            raw = json.loads(line)
            if not isinstance(raw, dict) or raw.get("run_id") != run_id or identifier(run_id) is None:
                raise ValueError()
            event = event_from_message("EVAL_EVENT " + json.dumps(raw))
            if event is None:
                raise ValueError()
            result.append(event)
        except (ValueError, TypeError, OverflowError):
            warnings.append("monitor audit skipped unknown, malformed or unmatched event at line %d" % number)
    return result


def snapshot_audit(evidence, run_id, warnings):
    """Collector writes snapshots/*.json; also accept JSONL with that same row schema."""
    sources = sorted((evidence / "snapshots").glob("*.json"))
    sources += sorted((evidence / "snapshots").glob("*.jsonl"))
    if (evidence / "snapshots.jsonl").exists():
        sources.append(evidence / "snapshots.jsonl")
    if not sources:
        warnings.append("collector snapshots missing; no snapshot audit included")
    result = []
    kinds = {"nodeprovisions", "fluidcrmigrations", "trainingpolicies", "trainingruntimes",
             "spotriskprofiles", "restorerequests", "spotrecoveries", "spotreplacements", "pods", "deployments"}
    for path in sources:
        try:
            text = path.read_text(encoding="utf-8")
        except (OSError, UnicodeError):
            warnings.append("unreadable collector snapshot skipped")
            continue
        for line in text.splitlines() if path.suffix == ".jsonl" else [text]:
            if not line.strip():
                continue
            try:
                raw = json.loads(line)
                if (not isinstance(raw, dict) or raw.get("run_id") != run_id or identifier(run_id) is None
                        or not isinstance(raw.get("resources"), list)):
                    raise ValueError()
                safe = {"timestamp": timestamp(raw.get("timestamp")).isoformat(), "run_id": run_id, "resources": []}
                for resource in raw["resources"]:
                    if (not isinstance(resource, dict) or resource.get("cluster") not in ("aws", "karmada", "mgmt")
                            or resource.get("resource") not in kinds or not isinstance(resource.get("items"), list)):
                        raise ValueError()
                    items = []
                    for item in resource["items"]:
                        if not isinstance(item, dict) or not isinstance(item.get("metadata", {}), dict):
                            raise ValueError()
                        projected = safe_resource(item)
                        json.dumps(projected, allow_nan=False)
                        items.append(projected)
                    safe["resources"].append({"cluster": resource["cluster"], "resource": resource["resource"], "items": items})
                result.append(safe)
            except (ValueError, TypeError, AttributeError, OverflowError):
                warnings.append("collector snapshot skipped: unknown/malformed schema or unmatched run")
    return result


def final_projection(value, field=None):
    """Keep structured receipts, not arbitrary metadata, messages or resource specs."""
    keys = {"observed_at", "timestamp", "fis", "fis_result", "experiment", "id", "state",
            "status", "outcome", "startTime", "endTime", "resources", "resource", "cluster",
            "kind", "apiVersion", "items", "metadata", "name", "namespace", "uid", "generation",
            "phase", "observedGeneration", "conditions", "type", "reason", "lastTransitionTime",
            "pods", "pod_identities", "rank", "pod_uid", "pod_name", "node_name", "node_uid",
            "nodeName", "podUID", "instance_id", "instanceId", "market", "ready", "verified",
            "errors", "target_ec2", "actions", "actionId", "creationTimestamp", "spec"}
    if isinstance(value, dict):
        if field == "resources":
            keys = {"karmada/trainingpolicies", "karmada/restorerequests", "karmada/spotreplacements",
                    "aws/nodeprovisions", "aws/pods", "aws/fluidcrmigrations"}
        elif field == "actions":
            return [{"name": identifier(name), "state": final_projection(action.get("state")),
                     "actionId": identifier(action.get("actionId"))}
                    for name, action in value.items() if identifier(name) and isinstance(action, dict)]
        elif field == "spec":
            keys = {"nodeName"}
        return {key: final_projection(item, key) for key, item in value.items() if key in keys}
    if isinstance(value, list):
        return [final_projection(item) for item in value]
    if value is None or type(value) in (bool, int):
        return value
    if type(value) is float:
        return value if math.isfinite(value) else None
    if isinstance(value, str):
        # Receipt identifiers and timestamps only; never free-form diagnostic text.
        return value if re.fullmatch(r"[A-Za-z0-9_:.+/@-]{1,256}", value) else None
    return None


def report(evidence, cost_ledger=None):
    evidence = Path(evidence)
    run = read_json(evidence / "run.json")
    if not isinstance(run, dict):
        raise ValueError("run.json must be an object")
    e3 = run.get("e3")
    if not isinstance(e3, dict) or identifier(e3.get("arm")) is None:
        raise ValueError("run.json requires e3.arm")
    warnings = []
    start = timestamp(e3.get("measurement_started_at", run.get("measurement_started_at")))
    end = timestamp(e3.get("measurement_ended_at", run.get("measurement_ended_at")))
    if end <= start:
        raise ValueError("measurement end must be after start")
    ranks, useful = progress(evidence / "monitor-events.jsonl", run.get("run_id"), start, end, warnings)
    analytic, risk = observations(run, warnings)
    fault = None
    try:
        value = read_json(evidence / "e3-fault.json")
        outcome = value.get("outcome") if isinstance(value, dict) else None
        if identifier(outcome) is None:
            raise ValueError("invalid outcome")
        fault = {"outcome": outcome}
    except (OSError, ValueError, TypeError):
        warnings.append("e3-fault.json missing or invalid outcome")
    cost = estimate_cost(cost_ledger, start, end, useful["tokens_seen"])
    if cost is None:
        warnings.append("cost unavailable: no user-supplied cost ledger")
    final = None
    try:
        raw_final = read_json(evidence / "e3-final.json")
        if not isinstance(raw_final, dict):
            raise ValueError("invalid final snapshot")
        final = final_projection(raw_final)
        if raw_final.get("errors"):
            warnings.append("final snapshot reports collection errors; receipts may be incomplete")
    except (OSError, ValueError, TypeError):
        warnings.append("e3-final.json missing or invalid")
    audit_events = monitor_audit(evidence / "monitor-events.jsonl", run.get("run_id"), warnings)
    snapshots = snapshot_audit(evidence, run.get("run_id"), warnings)
    plan = plan_projection(e3)
    summary = {"evaluation": "E3", "run_id": identifier(run.get("run_id")), "arm": e3["arm"],
               "measurement_started_at": start.isoformat(), "measurement_ended_at": end.isoformat(),
               "measurement_seconds": (end - start).total_seconds(), "rank_progress": ranks,
               "useful_progress": useful, "e3_fault": fault, "analytic_interval_observed": analytic,
               "risk_schedule_observations": risk, "cost": cost, "warnings": warnings,
               "runner_phase": identifier(run.get("phase")), "comparison_validity": "not_evaluated",
               "final_snapshot_available": final is not None,
               "plan": plan, "monitor_audit_event_count": len(audit_events), "snapshot_audit_count": len(snapshots),
               "verdict": "not_evaluated",
               "interpretation": "Recorded observations only; no pass or reliability claim. "
               "Useful progress = minimum latest rank counter minus minimum baseline rank counter. "
               "Latest samples are selected by timestamp, including rollback across attempts."}
    body = json.dumps(summary, indent=2, allow_nan=False) + "\n"
    markdown = "# E3 offline evidence report\n\nObservations only; no pass claim.\n\n```json\n" + body + "```\n"
    payload = {"e3-summary.json": body.encode(), "e3-summary.md": markdown.encode()}
    payload["e3-plan.json"] = (json.dumps(plan, indent=2, allow_nan=False) + "\n").encode()
    payload["e3-monitor-events.jsonl"] = "".join(json.dumps(event, allow_nan=False) + "\n" for event in audit_events).encode()
    if snapshots:
        payload["e3-snapshots.jsonl"] = "".join(json.dumps(row, allow_nan=False) + "\n" for row in snapshots).encode()
    if final is not None:
        payload["e3-final.json"] = (json.dumps(final, indent=2, allow_nan=False) + "\n").encode()
    hashes = {name: hashlib.sha256(data).hexdigest() for name, data in payload.items()}
    payload["e3-manifest.json"] = (json.dumps({"algorithm": "sha256", "files": hashes}, indent=2) + "\n").encode()
    # Package generated bytes only; never recursively archive the evidence directory.
    archive = io.BytesIO()
    with tarfile.open(fileobj=archive, mode="w:gz") as tar:
        for name, data in payload.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o644
            tar.addfile(info, io.BytesIO(data))
    payload["e3-evidence.tar.gz"] = archive.getvalue()
    payload["e3-evidence.tar.gz.sha256"] = (hashlib.sha256(archive.getvalue()).hexdigest() +
                                            "  e3-evidence.tar.gz\n").encode()
    for name in payload:
        if name in ("e3-final.json", "e3-plan.json"):
            continue
        path = evidence / name
        if path.is_symlink() or path.exists() and not path.is_file():
            raise ValueError("unsafe output path: " + name)
    for name, data in payload.items():
        if name not in ("e3-final.json", "e3-plan.json"):
            (evidence / name).write_bytes(data)
    return summary


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", type=Path, required=True)
    parser.add_argument("--cost-ledger", type=Path)
    args = parser.parse_args(argv)
    try:
        report(args.evidence, args.cost_ledger)
    except (OSError, ValueError, TypeError, OverflowError) as exc:
        parser.exit(2, "E3 report failed (%s); check evidence schema and paths.\n" % type(exc).__name__)
    print(args.evidence / "e3-summary.json")


if __name__ == "__main__":
    main()
