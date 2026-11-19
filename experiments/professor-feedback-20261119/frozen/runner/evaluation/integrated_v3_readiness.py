"""Offline B/C configuration and timing screening, without cloud mutations.

Timing is a preflight envelope, not a prediction of live Spot availability.
Unknown sizing stays unknown. Live four-rank/UID/provider checks remain required.
"""
import hashlib
import json
import math


def contract_hash(contract):
    return hashlib.sha256(json.dumps(contract, sort_keys=True, separators=(',', ':'),
                                     allow_nan=False).encode()).hexdigest()


def compare_bc_contracts(b, c):
    """Inputs contain common settings only; run identity/fault outcome are separate."""
    missing = sorted(k for k in set(b) | set(c) if b.get(k) is None or c.get(k) is None)
    different = sorted(k for k in set(b) | set(c) if b.get(k) != c.get(k))
    return {'identical_draft_settings': not different,
            'ready_to_freeze': not different and not missing,
            'different_fields': different, 'unfrozen_fields': missing,
            'b_sha256': contract_hash(b), 'c_sha256': contract_hash(c)}


def timing_envelope(*, forecast_transition_seconds, fault_elapsed_seconds=None,
                    normal_goal_elapsed_seconds=None, durable_checkpoint_elapsed_seconds=None,
                    notice_seconds=120, margin_seconds=None):
    """Conservative rejection if planned all-OD transition can overlap the fault.

    No synthetic goal or checkpoint time is substituted for unknown inputs.
    All times share the predeclared risk replay anchor, not server boot time.
    """
    values = (forecast_transition_seconds, fault_elapsed_seconds, normal_goal_elapsed_seconds,
              durable_checkpoint_elapsed_seconds, notice_seconds, margin_seconds)
    if any(v is not None and (type(v) not in (int, float) or not math.isfinite(v) or v < 0)
           for v in values):
        raise ValueError('finite nonnegative elapsed seconds required')
    if forecast_transition_seconds <= 0 or notice_seconds <= 0:
        raise ValueError('positive transition and notice required')
    missing = [k for k, v in [('fault_elapsed_seconds', fault_elapsed_seconds),
                              ('normal_goal_elapsed_seconds', normal_goal_elapsed_seconds),
                              ('durable_checkpoint_elapsed_seconds', durable_checkpoint_elapsed_seconds),
                              ('margin_seconds', margin_seconds)] if v is None]
    reasons = []
    if normal_goal_elapsed_seconds is not None and normal_goal_elapsed_seconds <= forecast_transition_seconds:
        reasons.append('normal_run_ends_before_risk_transition')
    if fault_elapsed_seconds is not None:
        if fault_elapsed_seconds >= forecast_transition_seconds:
            reasons.append('spot_target_not_guaranteed_at_fault')
        elif margin_seconds is not None and fault_elapsed_seconds + notice_seconds + margin_seconds >= forecast_transition_seconds:
            reasons.append('fis_notice_or_cleanup_conflicts_with_planned_transition')
        if durable_checkpoint_elapsed_seconds is not None and durable_checkpoint_elapsed_seconds > fault_elapsed_seconds:
            reasons.append('durable_checkpoint_not_ready_at_declared_fault')
    return {'status': 'CONFLICT' if reasons else ('UNRESOLVED' if missing else 'OFFLINE_WINDOW_COMPATIBLE'),
            'reasons': reasons, 'missing_inputs': missing,
            'live_execution_authorized': False,
            'scope': 'timing envelope only; no policy economics/residency or live ownership proof'}
