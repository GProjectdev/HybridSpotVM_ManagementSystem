"""Run-scoped IEEE Access v2 preparation and analysis; no cloud mutations.

prepare writes create-only, suspended manifests for N0/N1/S0/S1/P0/P1.
It intentionally does not call the legacy namespace-bound live runner.
metrics requires real both-rank completion and a reviewed billing ledger;
missing measurements remain null rather than invented from step gaps.
"""
import argparse
import copy
from decimal import Decimal, InvalidOperation
import hashlib
import json
from pathlib import Path
import re

try:
    from . import run as legacy
    from .collect import event_from_message, timestamp, PREFIX
except ImportError:
    import run as legacy
    from collect import event_from_message, timestamp, PREFIX

ARMS = {"N0", "N1", "S0", "S1", "P0", "P1"}
HERE = Path(__file__).resolve().parent
PROTOCOL = HERE.parent / "experiments" / "ieee-access-v2" / "config"
SHA = re.compile(r"[0-9a-f]{64}\Z")
IMAGE = re.compile(r"[^\s@]+@sha256:[0-9a-f]{64}\Z")


def canonical_hash(obj):
    return hashlib.sha256(json.dumps(obj, sort_keys=True, separators=(",", ":"), allow_nan=False).encode()).hexdigest()


def render(config, source, run_id, arm, *, risk_source=None):
    if arm not in ARMS:
        raise ValueError("arm must be N0/N1/S0/S1/P0/P1")
    # Derived identity prevents reusing an app/PV name in another namespace.
    ns = "eval-v2-" + run_id
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,15}[a-z0-9]|[a-z]", run_id):
        raise ValueError("run_id must be a unique 1-17 character DNS label")
    c = copy.deepcopy(config)
    if c.get("namespace", ns) != ns:
        raise ValueError("namespace must be derived from this run_id: " + ns)
    c.update(namespace=ns, model="gpt2", scenario="constant", restore_audit=True)
    c.pop("risk_schedule", None)
    if c.get("cluster") != "aws1" or c.get("instance_type") != "g5.xlarge":
        raise ValueError("protocol requires aws1 / g5.xlarge")
    if not IMAGE.fullmatch(c.get("training_image", "")):
        raise ValueError("training_image must be pinned to a SHA256 digest")
    if c.get("goal_steps") != 4747 or c.get("batch_size") != 4 or c.get("sequence_length") != 512 or c.get("seed") != 42:
        raise ValueError("workload differs from locked pilot target/batch/sequence/seed")
    if c.get("learning_rate") != 0.00005:
        raise ValueError("learning rate differs from locked workload")
    if not 0 < c.get("max_seconds", 0) <= 5 * 3600:
        raise ValueError("positive max_seconds within daily five-hour limit required")
    for name in ("asset_manifest_sha256",):
        if not SHA.fullmatch(c.get(name, "")):
            raise ValueError(name + " must identify the verified shared asset manifest")
    # Reuse workload rendering, not legacy prepare/start/cleanup globals.
    info, member, resources, policy = legacy.render(c, source, run_id, "checkpoint", "D", namespace=ns)
    policy["kind"] = "NodeRequest"
    policy["spec"]["policy"] = {"method": "paper-eq6-7", "alpha": 0.8, "minOnDemand": 1, "forecastHorizonSeconds": 3600, "economics": {"enabled": True, "onDemandDurationSeconds": 3600, "maxAgeSeconds": 3600}}
    if arm.startswith("P"):
        policy["spec"]["policy"].pop("fixedOnDemand", None)
    else:
        policy["spec"]["policy"]["fixedOnDemand"] = 2 if arm == "N0" else 1
    policy["spec"]["replacement"] = {"enabled": True}
    checkpoint = policy["spec"]["checkpoint"]
    for key in ("measuredCosts", "paperProfile", "riskBands", "candidateIntervalSeconds"):
        checkpoint.pop(key, None)
    checkpoint.update(minIntervalSeconds=60, maxIntervalSeconds=600, resume=True)
    annotations = policy["metadata"]["annotations"]
    annotations["training.dcnlab.com/planned-partial"] = "disabled"
    if arm == "N0":
        annotations["training.dcnlab.com/periodic-checkpoint"] = "disabled"
    else:
        annotations.pop("training.dcnlab.com/periodic-checkpoint", None)
    credentials = policy["spec"]["capacity"]["aws"]["credentialsRef"]
    credentials["namespace"] = ns
    sts = next(o for o in resources if o["kind"] == "StatefulSet")
    sts["spec"]["template"]["spec"]["nodeSelector"] = {
        "ml.dcn.ssu.ac.kr/provider": "AWS", "ml.dcn.ssu.ac.kr/node-provision-namespace": ns}
    env = sts["spec"]["template"]["spec"]["containers"][0]["env"]
    env.extend({"name": k, "value": str(v)} for k, v in {
        "EVAL_COMPLETION_AUDIT": 1, "EVAL_LIFECYCLE_EVENTS": 1, "NCCL_IB_DISABLE": 1, "FLUIDCR_CHECKPOINT_WATCHDOG_TIMEOUT": 240,
        "CTRL_CHECKPOINT_TIMEOUT": 300, "FLUIDCR_DISABLE_ENUMERATE_PATCH": 1,
        "FLUIDCR_DISABLE_ITER_PATCH": 1, "FLUIDCR_DISABLE_EPOCH_RANGE_PATCH": 1}.items())
    risk = next(o for o in resources if o["kind"] == "SpotRiskProfile")
    if arm.startswith(("S", "P")):
        if not isinstance(risk_source, dict) or risk_source.get("status") != "LOCKED":
            raise ValueError("E2 needs a pre-registered LOCKED risk source; controlled smoke inputs are not formal E2 inputs")
        spec = risk_source.get("profile_spec", {})
        if not spec.get("paperEstimator", {}).get("feedEndpoint") or "staticLambdaPerHour" in spec:
            raise ValueError("E2 needs the same causal paperEstimator feed in all arms")
        if risk_source.get("wall_seconds_per_trace_hour") != 3600:
            raise ValueError("historical risk hours must not be compressed")
        if not SHA.fullmatch(risk_source.get("trace_sha256", "")) or not SHA.fullmatch(risk_source.get("forecast_sha256", "")):
            raise ValueError("trace and pre-registered forecasts require hashes")
        if any(spec.get(k) != c[k2] for k, k2 in (("region", "region"), ("availabilityZone", "availability_zone"), ("instanceType", "instance_type"))):
            raise ValueError("risk profile must match the execution region/AZ/type while declaring external trace provenance")
        risk["spec"] = copy.deepcopy(spec)
    info.update(protocol="ieee-access-v2", experiment="E1" if arm.startswith("N") else "E2", arm=arm,
                ready_for_paid_execution=False, phase="rendered-suspended", namespace=ns,
                event_after_first_committed_step_seconds=1800 if arm in {"S1", "P1"} else None,
                tokens_per_committed_step=4096, checkpoint_mode="disabled" if arm == "N0" else "adaptive",
                risk_source_sha256=canonical_hash(risk_source) if risk_source is not None else None,
                required_gates=["fresh namespaces/PVCs", "verified assets", "current AWS account and actual capacity",
                                "bounded normal cleanup installed", "rank0 on actual OD", "UID-bound live workload",
                                "both-rank completion and state checks", "exact instance cost/termination ledger"])
    # Exclude identity-only fields when proving S/P share workload/checkpoint data.
    info["workload_lock_sha256"] = canonical_hash({k: c[k] for k in (
        "training_image", "goal_steps", "batch_size", "sequence_length", "seed", "learning_rate",
        "nfs_server", "nfs_dataset_path", "nfs_model_path", "asset_manifest_sha256")})
    info['bootstrap_risk'] = 'static 0.05 shared by all arms; activate locked replay for all E2 arms after first committed step'
    info['bootstrap_on_demand'] = 2 if arm == 'N0' else 1
    info['policy_activation'] = 'after both ranks first committed step; UID-bound patch removes bootstrap fixedOnDemand only for Proposed'
    info["completion_audit_required"] = True
    info["checkpoint_lock_sha256"] = canonical_hash(checkpoint)
    info["render_sha256"] = canonical_hash({"member": member, "resources": resources, "policy": policy})
    return info, member, resources, policy


def interval_union(intervals):
    ordered = sorted((timestamp(a), timestamp(b)) for a, b in intervals)
    result = []
    for a, b in ordered:
        if b < a:
            raise ValueError("interval ends before it starts")
        if result and a <= result[-1][1]:
            result[-1] = (result[-1][0], max(b, result[-1][1]))
        else:
            result.append((a, b))
    return sum((b - a).total_seconds() for a, b in result)


def compute_cost(segments, owned_instances):
    """Reviewed GPU-only price segments; no currency mixing or overlap inflation."""
    if not segments or not owned_instances:
        raise ValueError("complete instance ledger and explicit billing segments required")
    total = Decimal(0)
    per_instance = {}
    for segment in segments:
        instance = segment["instance_id"]
        if instance not in owned_instances or segment.get("currency") != "USD":
            raise ValueError("billing must name a UID-reviewed owned instance and USD")
        a, b = timestamp(segment["start"]), timestamp(segment["end"])
        if a >= b:
            raise ValueError("billing interval must be positive")
        try:
            price = Decimal(str(segment["price_per_hour"]))
        except InvalidOperation as e:
            raise ValueError("invalid price") from e
        if not price.is_finite() or price < 0:
            raise ValueError("price must be finite and nonnegative")
        intervals = per_instance.setdefault(instance, [])
        if any(a < y and x < b for x, y in intervals):
            raise ValueError("overlapping billing segments for one instance")
        intervals.append((a, b))
        total += Decimal(str((b - a).total_seconds())) / Decimal(3600) * price
    if set(per_instance) != set(owned_instances):
        raise ValueError("missing billing for an owned instance")
    for instance, evidence in owned_instances.items():
        if evidence.get("state") != "terminated" or not evidence.get("nodeprovision_uid") or not evidence.get("policy_uid"):
            raise ValueError("cost needs positive provider termination and UID ownership evidence")
        start, end = timestamp(evidence["billing_start"]), timestamp(evidence["billing_end"])
        intervals = sorted(per_instance[instance])
        if intervals[0][0] != start or intervals[-1][1] != end or any(x[1] != y[0] for x, y in zip(intervals, intervals[1:])):
            raise ValueError("billing must continuously cover each instance's reviewed lifetime")
    return float(total)


def metrics(info, events, *, request_at=None, billing=None, owned_instances=None, checkpoint_stalls=None):
    if info.get("protocol") != "ieee-access-v2" or info.get("arm") not in ARMS:
        raise ValueError("not an IEEE v2 run")
    goal = info["goal_steps"]
    records, seen = [], set()
    for raw in events:
        e = event_from_message(PREFIX + json.dumps(raw))
        if e is None or e["run_id"] != info["run_id"] or e["rank"] not in (0, 1):
            raise ValueError("foreign or invalid event in run")
        if not raw.get("pod_uid") or not e.get("attempt_id") or raw.get("world_size") != 2:
            raise ValueError("Pod UID, attempt ID and world_size=2 are required")
        key = (raw.get("pod_uid"), e.get("attempt_id"), e["rank"], e["event"], e["global_step"], e["timestamp"])
        if key in seen:
            continue
        seen.add(key)
        e["pod_uid"] = raw["pod_uid"]
        records.append((timestamp(e["timestamp"]), e))
    records.sort(key=lambda x: x[0])
    if request_at:
        request_time = timestamp(request_at)
        records = [(at, e) for at, e in records if at >= request_time]
    # Each rank must actually attest completion at the exact locked goal.
    completed = {r: [(at, e) for at, e in records if e["rank"] == r and e["event"] == "completed" and e["global_step"] == goal] for r in (0, 1)}
    common_steps = set.intersection(*({e["global_step"] for _, e in records if e["event"] == "step" and e["rank"] == r} for r in (0, 1)))
    first = None
    if common_steps:
        first_step = min(common_steps)
        first = max(min(at for at, e in records if e["event"] == "step" and e["rank"] == r and e["global_step"] == first_step) for r in (0, 1))
    goal_events = {r:[at for at,e in records if e['rank']==r and e['event']=='step' and e['global_step']==goal] for r in (0,1)}
    done = all(completed.values())
    if info.get('completion_audit_required'):
        done = done and all(goal_events.values())
    finish = max(min(at for at, _ in completed[r]) for r in (0, 1)) if done else None
    if done and info.get('completion_audit_required'):
        # Final hashing/verification time is billed, but not useful training time.
        finish = max(min(goal_events[r]) for r in (0,1))
    audits = {r:[e for _,e in records if e['rank']==r and e['event']=='completion_audit' and e['global_step']==goal] for r in (0,1)}
    audit_ok = all(audits.values()) and all(e['passed'] for rows in audits.values() for e in rows)
    if audit_ok:
        audit_ok = all(len({e[field] for rows in audits.values() for e in rows}) == 1
                       for field in ('model_sha256','optimizer_sha256','data_contract_sha256'))
    result = {"run_id": info["run_id"], "arm": info["arm"], "all_rank_completion_observed": done,
              "quality": "REQUIRES_STATE_AND_PROTOCOL_REVIEW" if done else "INCOMPLETE",
              "target_steps": goal, "useful_tokens": goal * info["tokens_per_committed_step"] if done else None,
              "first_committed_step_utc": first.isoformat() if first else None,
              "completion_utc": finish.isoformat() if finish else None,
              "T_startup_seconds": None, "T_E2E_seconds": None, "T_training_seconds": None,
              "UsefulThroughput_tokens_per_second": None, "estimated_gpu_compute_cost_usd": None,
              "cost_per_useful_step_usd": None, "checkpoint_stall_seconds": None}
    try:
        from .ieee_v2_lifecycle import analyze
    except ImportError:
        from ieee_v2_lifecycle import analyze
    result['ddp_lifecycle'] = analyze([e for _,e in records])
    result['final_state_audit_passed'] = bool(audit_ok)
    if done and info.get('completion_audit_required') and not audit_ok:
        result['quality'] = 'FINAL_STATE_AUDIT_MISSING_OR_MISMATCHED'
    request = timestamp(request_at) if request_at else None
    if request and first:
        if request > first:
            raise ValueError("request occurs after first committed step")
        result["T_startup_seconds"] = (first - request).total_seconds()
    if done and first:
        duration = (finish - first).total_seconds()
        if duration <= 0:
            raise ValueError("completion must follow first committed step")
        result["T_training_seconds"] = duration
        result["UsefulThroughput_tokens_per_second"] = result["useful_tokens"] / duration
        if request:
            result["T_E2E_seconds"] = (finish - request).total_seconds()
    if billing is not None:
        cost = compute_cost(billing, owned_instances)
        result["estimated_gpu_compute_cost_usd"] = cost
        if done:
            result["cost_per_useful_step_usd"] = cost / goal
    if checkpoint_stalls is not None:
        result["checkpoint_stall_seconds"] = interval_union(checkpoint_stalls)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("prepare", help="offline only; refuses existing output directory")
    p.add_argument("--config", type=Path, required=True)
    p.add_argument("--policy-source", type=Path, required=True)
    p.add_argument("--risk-source", type=Path)
    p.add_argument("--run-id", required=True)
    p.add_argument("--arm", choices=sorted(ARMS), required=True)
    p.add_argument("--out", type=Path, required=True)
    p = sub.add_parser("metrics")
    p.add_argument("--run", type=Path, required=True)
    p.add_argument("--events", type=Path, required=True)
    p.add_argument("--request-at")
    p.add_argument("--billing", type=Path)
    p.add_argument("--out", type=Path, required=True)
    a = parser.parse_args()
    if a.command == "prepare":
        if a.out.exists():
            parser.error("output already exists; preserve the prior attempt")
        read = lambda path: json.loads(path.read_text())
        info, member, resources, policy = render(read(a.config), read(a.policy_source), a.run_id, a.arm,
                                                risk_source=read(a.risk_source) if a.risk_source else None)
        a.out.mkdir(parents=True)
        bootstrap = copy.deepcopy(policy)
        bootstrap['spec']['policy']['fixedOnDemand'] = info['bootstrap_on_demand']
        risk = next(o for o in resources if o['kind']=='SpotRiskProfile')
        bootstrap_risk = copy.deepcopy(risk)
        bootstrap_risk['spec'].pop('paperEstimator',None)
        bootstrap_risk['spec']['staticLambdaPerHour'] = 0.05
        workload = next(o for o in resources if o['kind']=='StatefulSet')
        prerequisites = [o for o in resources if o['kind'] not in {'StatefulSet','SpotRiskProfile'}]
        for name, obj in [("risk.bootstrap.json", bootstrap_risk), ("risk.armed.json", risk), ("workload.stopped.json", workload), ("policy.bootstrap.json", bootstrap), ("run.json", info), ("member-storage.json", {"apiVersion": "v1", "kind": "List", "items": member}),
                          ("karmada.json", {"apiVersion": "v1", "kind": "List", "items": prerequisites}), ("policy.unbound.json", policy)]:
            legacy.save(a.out / name, obj)
        print("Rendered suspended/unbound manifests only. No API calls, VM creation or FIS.")
    else:
        billing = json.loads(a.billing.read_text()) if a.billing else {}
        result = metrics(json.loads(a.run.read_text()), [json.loads(x) for x in a.events.read_text().splitlines() if x.strip()],
                         request_at=a.request_at, billing=billing.get("segments"), owned_instances=billing.get("instances"))
        if a.out.exists():
            parser.error("refuse to overwrite metrics; choose a new output")
        legacy.save(a.out, result)
        print(json.dumps(result))


if __name__ == "__main__":
    main()
