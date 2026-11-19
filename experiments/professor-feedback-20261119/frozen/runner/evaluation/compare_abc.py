"""Offline descriptive A/B/C comparison; never selects the fastest successful retry.

Register comparison_slot and block_id BEFORE execution. Retain non-slot retries,
failures and costs. Eligibility is an explicit human protocol review, not a
conclusion supplied by this script. This module never calls Kubernetes or AWS.
"""
import argparse
import hashlib
import json
import math
from pathlib import Path
import re
import statistics

STATES = {"planned", "running", "completed", "failed", "ineligible", "aborted", "capacity_denied"}
SHA = re.compile(r"[0-9a-f]{64}\Z")
COMMON_FIELDS = ("system_lock", "training_code_sha256", "region", "availability_zone",
                 "instance_type", "storage_class", "nfs_server", "risk_lock_sha256",
                 "fault_window", "checkpoint_buffer_reuse", "group_replacement_batch_size")


def number(value, *, positive=False):
    return (isinstance(value, (int, float)) and not isinstance(value, bool)
            and math.isfinite(value) and (value > 0 if positive else value >= 0))


def summarize(register, base):
    if register.get("schema_version") != 1:
        raise ValueError("explicit schema_version=1 required")
    base = Path(base)
    rows, slots, hashes, seen, aggregates = [], {}, {}, set(), {}

    def read_file(name):
        path = base / name
        raw = path.read_bytes()
        hashes[str(name)] = hashlib.sha256(raw).hexdigest()
        return json.loads(raw)

    for attempt in register["attempts"]:
        ident = attempt["attempt_id"]
        model, arm, block = (attempt[k] for k in ("model", "arm", "block_id"))
        state = attempt["status"]
        if (not isinstance(ident, str) or not ident or ident in seen
                or model not in {"gpt2", "resnet50"} or arm not in {"A", "B", "C"}
                or state not in STATES or not isinstance(block, str) or not block
                or type(attempt.get("comparison_slot")) is not bool):
            raise ValueError("invalid or duplicate attempt/arm/model/block/status/slot")
        seen.add(ident)
        key = (model, block, arm)
        if attempt["comparison_slot"] and key in slots:
            raise ValueError("duplicate preassigned comparison slot; do not select a retry")
        run = read_file(attempt["run_json"]) if attempt.get("run_json") else {}
        metrics = read_file(attempt["metrics_json"]) if attempt.get("metrics_json") else {}
        cost = metrics.get("estimated_gpu_compute_cost_usd")
        if cost is not None and not number(cost):
            raise ValueError("invalid estimated GPU cost")
        if run and (run.get("run_id") != ident or run.get("model") != model
                    or run.get("arm") != arm or run.get("world_size") != 4):
            raise ValueError("run identity/model/arm/four-rank mismatch")
        if metrics and (metrics.get("run_id") != ident or metrics.get("arm") != arm):
            raise ValueError("metrics identity/arm mismatch")
        review = attempt.get("review", {})
        reasons = []
        if state != "completed":
            reasons.append("attempt_not_completed")
        if not (review.get("protocol_eligible") is True and review.get("reviewer")
                and review.get("evidence")):
            reasons.append("external_protocol_review_missing_or_ineligible")
        if not (metrics.get("all_rank_completion_observed") is True
                and metrics.get("final_state_audit_passed") is True):
            reasons.append("all_rank_completion_or_final_state_audit_missing")
        if not number(metrics.get("T_E2E_seconds"), positive=True) or cost is None:
            reasons.append("complete_time_or_cost_missing")
        if not (type(run.get("goal_steps")) is int and run["goal_steps"] > 0
                and run.get("goal_steps") == metrics.get("target_steps")):
            reasons.append("goal_contract_missing_or_mismatched")
        if not all(isinstance(run.get(k), str) and SHA.fullmatch(run[k])
                   for k in ("workload_lock_sha256", "comparison_lock_sha256")):
            reasons.append("comparison_contract_missing")
        if any(k not in run or run[k] is None for k in COMMON_FIELDS):
            reasons.append("common_system_risk_storage_contract_missing")
        row = dict(attempt_id=ident, model=model, block_id=block, arm=arm,
                   status=state, comparison_slot=attempt["comparison_slot"],
                   eligible_for_descriptive_comparison=not reasons,
                   ineligibility_reasons=reasons, failure_reason=attempt.get("failure_reason"),
                   estimated_gpu_compute_cost_usd=cost, T_E2E_seconds=metrics.get("T_E2E_seconds"))
        rows.append(row)
        if attempt["comparison_slot"]:
            slots[key] = (row, run)
        group = aggregates.setdefault(model, dict(observed_attempt_cost_usd=0.0,
                                                  executed_attempts=0, executed_attempts_missing_cost=0))
        if state != "planned":
            group["executed_attempts"] += 1
            if cost is None:
                group["executed_attempts_missing_cost"] += 1
            else:
                group["observed_attempt_cost_usd"] += cost

    comparisons = []
    for model, block in sorted({(m, b) for m, b, _ in slots}):
        for left, right in (("A", "B"), ("B", "C"), ("A", "C")):
            pair = [slots.get((model, block, arm)) for arm in (left, right)]
            item = dict(model=model, block_id=block, contrast=f"{left}_vs_{right}", available=False)
            if any(v is None or not v[0]["eligible_for_descriptive_comparison"] for v in pair):
                item["reason"] = "preassigned_slot_missing_or_ineligible"
            else:
                (a, ar), (b, br) = pair
                if ar["workload_lock_sha256"] != br["workload_lock_sha256"]:
                    item["reason"] = "workload_contract_mismatch"
                elif any(ar[k] != br[k] for k in COMMON_FIELDS):
                    item["reason"] = "common_system_risk_storage_contract_mismatch"
                elif left == "B" and ar["comparison_lock_sha256"] != br["comparison_lock_sha256"]:
                    item["reason"] = "B_C_policy_risk_checkpoint_contract_mismatch"
                else:
                    ca, cb = a["estimated_gpu_compute_cost_usd"], b["estimated_gpu_compute_cost_usd"]
                    ta, tb = a["T_E2E_seconds"], b["T_E2E_seconds"]
                    item.update(available=True, delta_cost_right_minus_left_usd=cb-ca,
                                delta_time_right_minus_left_seconds=tb-ta,
                                cost_saving_fraction_vs_left=(ca-cb)/ca if ca > 0 else None,
                                time_ratio_right_to_left=tb/ta)
            comparisons.append(item)
    descriptives = []
    for model, arm in sorted({(r["model"], r["arm"]) for r in rows}):
        selected = [r for r in rows if r["model"] == model and r["arm"] == arm
                    and r["comparison_slot"] and r["eligible_for_descriptive_comparison"]]
        for field in ("T_E2E_seconds", "estimated_gpu_compute_cost_usd"):
            values = [r[field] for r in selected]
            descriptives.append(dict(model=model, arm=arm, metric=field, n=len(values),
                                     values=values, mean=statistics.mean(values) if values else None,
                                     sample_sd=statistics.stdev(values) if len(values) > 1 else None))
    return dict(kind="DESCRIPTIVE_OFFLINE_REQUIRES_EXTERNAL_PROTOCOL_REVIEW",
                formal_result=False, confidence_intervals_computed=False,
                attempts=rows, comparisons=comparisons, descriptives=descriptives,
                all_attempt_cost_accounting_by_model=aggregates, input_sha256=hashes,
                cost_scope="gross GPU price integral estimate; not AWS invoice or total cloud cost",
                warning="Unknown costs remain unknown. No failed/ineligible slot is replaced by a successful retry.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--register", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    register = json.loads(args.register.read_text())
    result = summarize(register, args.register.parent)
    result["register_sha256"] = hashlib.sha256(args.register.read_bytes()).hexdigest()
    with args.out.open("x") as handle:
        json.dump(result, handle, ensure_ascii=False, indent=2, allow_nan=False)
        handle.write("\n")


if __name__ == "__main__":
    main()
