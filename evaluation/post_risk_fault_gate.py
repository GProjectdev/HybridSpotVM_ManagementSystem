"""Read-only post-risk/durable gate shared by B shadow and C admission.

This is a separate experimental contract, not an override of the historical
50-percent gate. No cloud clients, checkpoint requests or policy mutations.
Provider ownership/health must additionally pass the live dispatcher's checks.
"""
import hashlib
import json
import math
import os
import re
from datetime import datetime, timedelta
from pathlib import Path

REVISION = 'post-risk-durable-v1'
LABEL = 'training.dcnlab.com/'


def stamp(value):
    t = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if t.tzinfo is None:
        raise ValueError('explicit timezone required')
    return t


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':'),
                                     allow_nan=False).encode()).hexdigest()


def sha(value):
    return isinstance(value, str) and len(value) == 64 and all(c in '0123456789abcdef' for c in value)


def number(value, minimum=0):
    return type(value) in (int, float) and math.isfinite(value) and value >= minimum


def validate_settings(run):
    g = run.get('post_risk_gate', {})
    if (run.get('protocol') != 'HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118'
            or run.get('arm') not in ('B', 'C') or run.get('world_size') != 4
            or g.get('revision') != REVISION):
        raise ValueError('explicit four-rank B/C post-risk contract required')
    if run.get('fault_window') is not None or run.get('fault_trigger') is not None:
        raise ValueError('historical time/step trigger cannot be combined with post-risk gate')
    if (g.get('start_hour'), g.get('update_hour'), g.get('refresh_seconds')) != (9, 10, 3600):
        raise ValueError('reviewed native hour9->10 contract required')
    if not sha(g.get('risk_lock_sha256')) or g['risk_lock_sha256'] != run.get('risk_lock_sha256'):
        raise ValueError('same frozen risk input hash required')
    expected = {'gpt2': 4800, 'resnet50': 26700}
    if run.get('model') not in expected or run.get('goal_steps') != expected[run['model']]:
        raise ValueError('unchanged model-specific work required')
    if not all(sha(g.get(k)) for k in ('trace_sha256','forecast_sha256')):
        raise ValueError('explicit frozen trace and forecast SHA256 required')
    for k in ('notice_seconds', 'notice_margin_seconds', 'recovery_reserve_seconds',
              'additional_operations_reserve_seconds', 'cleanup_reserve_seconds',
              'fast_seconds_per_step', 'slow_seconds_per_step', 'max_progress_age_seconds',
              'max_observation_age_seconds', 'nfs_reserve_bytes', 'next_checkpoint_reserve_bytes',
              'local_reserve_bytes', 'budget_usd'):
        if not number(g.get(k), 0 if k == 'notice_margin_seconds' else .000001):
            raise ValueError('explicit finite positive budget required: '+k)
    if (g['notice_seconds'] != 120 or g['cleanup_reserve_seconds'] < 1200
            or g['recovery_reserve_seconds'] < 1800
            or g['additional_operations_reserve_seconds'] < 600
            or not 0 < g['fast_seconds_per_step'] <= g['slow_seconds_per_step']
            or g['max_progress_age_seconds'] > 60 or g['max_observation_age_seconds'] > 90
            or g['nfs_reserve_bytes'] < 64*1024**3):
        raise ValueError('bounded observation/recovery/storage contract required')
    if (not number(g.get('updated_lambda_per_hour'))
            or g.get('target_selection') != 'lowest-rank-owned-spot'
            or g.get('first_eligible_only') is not True):
        raise ValueError('predeclared input and first eligible target rule required')
    return g


def validate_contract(run):
    g=validate_settings(run)
    if not all(run.get(k) for k in ('run_id', 'namespace', 'policy_uid', 'workload_uid',
                                     'source_created_at', 'hard_deadline_utc', 'cleanup_start_utc')):
        raise ValueError('live run identities and absolute deadlines required')
    hard, cleanup, origin = map(stamp, (run['hard_deadline_utc'], run['cleanup_start_utc'], run['source_created_at']))
    if not 0 < (hard-origin).total_seconds() <= 18000 or (hard-cleanup).total_seconds() < g['cleanup_reserve_seconds']:
        raise ValueError('bounded absolute runtime and cleanup required')
    return g


def check(run, observation, now):
    """Validate a read-only evidence bundle. Missing evidence never grants FIS.

    risk_application is a persisted observer receipt binding the actual fresh
    Coordinator decision to a matching acknowledged Member schedule. It is not
    merely the time of the forecast update or a computed expected interval.
    """
    g = validate_contract(run)
    at = stamp(now)

    def result(reason, *, ready=False, eligible=False, terminal=False, **details):
        return dict(revision=REVISION, run_id=run['run_id'], ready=ready,
                    eligible=eligible, terminal=terminal, reason=reason,
                    evaluated_at=now, contract_sha256=digest(g), **details)

    def fresh(value, limit):
        age = (at-stamp(value)).total_seconds()
        return 0 <= age <= limit

    if at >= stamp(run['cleanup_start_utc']):
        return result('cleanup_deadline', terminal=True)
    if not fresh(observation['captured_at'], g['max_observation_age_seconds']):
        return result('observation_stale_or_future', terminal=True)
    if (observation.get('run_id') != run['run_id']
            or observation.get('policy_uid') != run['policy_uid']
            or observation.get('workload_uid') != run['workload_uid']):
        raise ValueError('foreign observation bundle')
    safety = observation['safety']
    if not fresh(safety['observed_at'], g['max_observation_age_seconds']):
        return result('safety_observation_stale', terminal=True)
    required = ('nfs_free_bytes', 'local_free_bytes', 'estimated_cost_usd', 'cleanup_cost_reserve_usd')
    if not all(number(safety.get(k)) for k in required) or safety.get('stop_requested') is not False:
        return result('safety_unavailable_or_stop_requested', terminal=True)
    if safety['nfs_free_bytes'] < g['nfs_reserve_bytes']+g['next_checkpoint_reserve_bytes']:
        return result('nfs_next_checkpoint_headroom', terminal=True)
    if safety['local_free_bytes'] < g['local_reserve_bytes']:
        return result('local_log_headroom', terminal=True)
    if safety['estimated_cost_usd']+safety['cleanup_cost_reserve_usd'] >= g['budget_usd']:
        return result('cost_headroom', terminal=True)

    pods = {int(k): v for k,v in observation['current_pods'].items()}
    if set(pods) != set(range(4)) or len(set(pods.values())) != 4 or not all(pods.values()):
        raise ValueError('four distinct current source Pod UIDs required')
    latest = {}
    for e in observation['training_events']:
        if e.get('run_id') != run['run_id'] or e.get('world_size') != 4:
            raise ValueError('foreign training event')
        rank = e.get('rank')
        if type(rank) is not int or rank not in pods or not e.get('attempt_id'):
            raise ValueError('rank and attempt identity required')
        if type(e.get('global_step')) is not int or e['global_step'] < 0:
            raise ValueError('integer committed step required')
        if e.get('pod_uid') != pods[rank] or e.get('event') not in ('step', 'completed'):
            continue
        t = stamp(e['timestamp'])
        if t > at:
            raise ValueError('future progress')
        if rank not in latest or t > stamp(latest[rank]['timestamp']):
            latest[rank] = e
        elif t == stamp(latest[rank]['timestamp']) and e != latest[rank]:
            raise ValueError('ambiguous same-time progress')
    if len(latest) != 4:
        return result('waiting_four_rank_progress')
    if any(e['event'] == 'completed' or e['global_step'] >= run['goal_steps'] for e in latest.values()):
        return result('goal_completed_without_gate', terminal=True)
    if any(not fresh(e['timestamp'], g['max_progress_age_seconds']) for e in latest.values()):
        return result('rank_progress_stale')

    app = observation.get('risk_application')
    if not app:
        return result('waiting_risk_application')
    boundary = stamp(run['source_created_at'])+timedelta(seconds=3600)
    applied = stamp(app['applied_at'])
    if (app.get('policy_uid') != run['policy_uid'] or app.get('workload_uid') != run['workload_uid']
            or app.get('risk_lock_sha256') != g['risk_lock_sha256']
            or app.get('source_hour') != 10 or not sha(app.get('evidence_sha256'))
            or not boundary <= applied <= at
            or app.get('lambda_per_hour') != g['updated_lambda_per_hour']):
        return result('risk_application_binding_invalid', terminal=True)
    parent = observation['schedule']; pm = parent['metadata']; ps = parent['spec']; status = parent['status']
    if (pm.get('deletionTimestamp') or pm.get('namespace') != run['namespace']
            or pm.get('labels', {}).get(LABEL+'policy-uid') != run['policy_uid']
            or ps.get('workloadRef', {}).get('uid') != run['workload_uid']
            or pm.get('uid') != app.get('schedule_uid')
            or pm.get('generation') != app.get('schedule_generation')
            or status.get('observedGeneration') != pm.get('generation')
            or ps.get('schedule', {}).get('enabled') is not True
            or ps['schedule'].get('intervalSeconds') != app.get('interval_seconds')):
        return result('schedule_not_matching_applied_risk')
    cp = observation.get('checkpoint')
    if not cp:
        return result('waiting_new_periodic_checkpoint')
    cm, cs, cst = cp['metadata'], cp['spec'], cp.get('status', {})
    ann = cm.get('annotations', {}); cid = ann.get(LABEL+'checkpoint-id')
    if (cm.get('namespace') != run['namespace'] or not cm.get('uid') or cm.get('deletionTimestamp')
            or cm.get('labels', {}).get(LABEL+'scheduled-parent') != pm['uid']
            or ann.get(LABEL+'scheduled-parent-uid') != pm['uid']
            or ann.get(LABEL+'scheduled-parent-name') != pm['name']
            or cs.get('workloadRef', {}).get('uid') != run['workload_uid']
            or cs.get('partialCheckpoint') or cs.get('schedule') or cs.get('resume') is False or not cid):
        return result('checkpoint_not_owned_periodic_full_world', terminal=True)
    if stamp(cm['creationTimestamp']) < applied:
        return result('waiting_checkpoint_created_after_risk')
    # The actual Member scheduler embeds parent UID hash, generation and
    # reservation UnixNano in the ID. A pre-update reservation materialized
    # later must never masquerade as a post-update natural checkpoint.
    prefix=pm['name']+'-'+hashlib.sha256(pm['uid'].encode()).hexdigest()[:8]
    match=re.fullmatch(re.escape(prefix)+r'-g([0-9]+)-t([0-9]+)',cid)
    if not match or not app.get('initial_schedule_generation',app['schedule_generation']) <= int(match[1]) <= pm['generation']:
        return result('checkpoint_reservation_generation_invalid',terminal=True)
    if int(match[2])/1e9 < applied.timestamp() or int(match[2])/1e9 > stamp(cm['creationTimestamp']).timestamp()+1:
        return result('checkpoint_reservation_not_after_risk',terminal=True)
    if cst.get('phase') in ('Failed','Cancelled'):
        return result('first_periodic_checkpoint_failed',terminal=True)
    if (cst.get('phase') != 'Completed' or cst.get('observedGeneration') != cm.get('generation')
            or not cst.get('startTime') or not cst.get('completionTime')):
        return result('waiting_checkpoint_completion')
    began, completed = stamp(cst['startTime']), stamp(cst['completionTime'])
    if not applied <= stamp(cm['creationTimestamp']) <= began <= completed <= at:
        return result('checkpoint_time_order_invalid', terminal=True)
    reports = cst.get('pods', [])
    if len(reports) != 4 or {p.get('rank') for p in reports} != set(range(4)):
        return result('waiting_full_rank_archives')
    exports = []; hashes = []
    for p in reports:
        if p.get('podUID') != pods[p['rank']] or p.get('checkpointID') != cid or p.get('phase') != 'Resumed':
            return result('checkpoint_source_world_or_resume_mismatch')
        files = p.get('checkpointFiles', [])
        if not files:
            return result('waiting_durable_files')
        for f in files:
            h = f.get('sha256')
            if (not sha(h) or f.get('durableRef') != 'file-store:'+run['namespace']+'/sha256/'+h
                    or f.get('containerName') != 'trainer' or f.get('checkpointID') != cid
                    or not f.get('exportedAt')):
                return result('waiting_valid_durable_receipts')
            export = stamp(f['exportedAt'])
            if not completed <= export <= at:
                return result('archive_export_time_invalid', terminal=True)
            exports.append(export); hashes.append(h)
    durable = max(exports)
    saved = {}; resumed = {}
    for e in observation['training_events']:
        rank = e['rank']
        if e.get('pod_uid') != pods[rank] or e.get('checkpoint_id') != cid:
            continue
        t = stamp(e['timestamp'])
        if not began <= t <= at:
            continue
        if e.get('event') == 'checkpoint_saved':
            if rank in saved and saved[rank]['global_step'] != e['global_step']:
                raise ValueError('conflicting checkpoint saved step')
            saved[rank] = e
        if e.get('event') == 'training_resumed':
            resumed[rank] = e
    if len(saved) != 4 or len({e['global_step'] for e in saved.values()}) != 1:
        return result('waiting_common_saved_step')
    cpstep = next(iter(saved.values()))['global_step']
    if type(cpstep) is not int or not 0 <= cpstep < run['goal_steps']:
        raise ValueError('valid saved common step required')
    if len(resumed) != 4 or any(
            stamp(resumed[r]['timestamp']) < stamp(saved[r]['timestamp'])
            or latest[r]['attempt_id'] != saved[r]['attempt_id']
            or resumed[r]['attempt_id'] != saved[r]['attempt_id']
            or stamp(latest[r]['timestamp']) <= max(durable, stamp(resumed[r]['timestamp']))
            or latest[r]['global_step'] <= cpstep for r in pods):
        return result('waiting_all_rank_post_durable_progress')
    common = min(e['global_step'] for e in latest.values())
    furthest = max(e['global_step'] for e in latest.values())
    details = dict(checkpoint_uid=cm['uid'], checkpoint_id=cid, saved_step=cpstep,
                   common_step=common, durable_at=durable.isoformat(), archives_sha256=sorted(hashes),
                   risk_applied_at=app['applied_at'], progress_valid_until_utc=(
                       min(stamp(e['timestamp']) for e in latest.values())+
                       timedelta(seconds=g['max_progress_age_seconds'])).isoformat())
    if observation.get('active_operation_uids') != []:
        return result('active_operation_conflict', eligible=True, terminal=True, **details)
    workers = observation['workers']
    if (len(workers) != 4 or {w.get('rank') for w in workers} != set(range(4))
            or len({w.get('instance_id') for w in workers}) != 4
            or any(w.get('pod_uid') != pods[w['rank']] or w.get('policy_uid') != run['policy_uid']
                   or w.get('market') not in ('Spot','OnDemand') or not w.get('nodeprovision_uid')
                   or w.get('state') != 'running' or w.get('ownership_verified') is not True for w in workers)):
        return result('owned_running_world_required', eligible=True, terminal=True, **details)
    spots = sorted((w for w in workers if w['market'] == 'Spot'), key=lambda w:w['rank'])
    if not spots:
        return result('no_spot_at_first_eligible', eligible=True, terminal=True, **details)
    target = spots[0]
    details.update(target_instance_id=target['instance_id'], target_rank=target['rank'],
                   target_nodeprovision_uid=target['nodeprovision_uid'])
    if (run['goal_steps']-furthest)*g['fast_seconds_per_step'] <= g['notice_seconds']+g['notice_margin_seconds']:
        return result('goal_may_finish_during_notice', eligible=True, terminal=True, **details)
    remaining = (run['goal_steps']-cpstep)*g['slow_seconds_per_step']
    needed = g['notice_seconds']+g['recovery_reserve_seconds']+g['additional_operations_reserve_seconds']+remaining
    if (stamp(run['cleanup_start_utc'])-at).total_seconds() <= needed:
        return result('insufficient_recovery_and_remaining_work_budget', eligible=True, terminal=True, **details)
    details.update(remaining_work_budget_seconds=remaining,
                   admission_valid_until_utc=min(stamp(run['cleanup_start_utc'])-timedelta(seconds=needed),
                                                stamp(details['progress_valid_until_utc'])).isoformat())
    return result('first_post_risk_durable_candidate', ready=True, eligible=True, **details)


def record_first(directory, run, result):
    """Persist first eligibility/terminal decision; never silently choose a later CP.

    B and C use exactly the same gate, but neither this receipt nor ready=True
    authorizes a cloud call. The C dispatcher still needs live checks and consent.
    """
    if result.get('run_id') != run['run_id'] or result.get('contract_sha256') != digest(validate_contract(run)):
        raise ValueError('decision/run contract mismatch')
    if not result.get('eligible') and not result.get('terminal'):
        return None
    path = Path(directory)/'post-risk-first-gate.json'
    value = dict(result, mode='shadow' if run['arm']=='B' else 'admission',
                 paid_execution_authorized_by_this_receipt=False)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as f:
        json.dump(value, f, indent=2); f.flush(); os.fsync(f.fileno())
    return value
