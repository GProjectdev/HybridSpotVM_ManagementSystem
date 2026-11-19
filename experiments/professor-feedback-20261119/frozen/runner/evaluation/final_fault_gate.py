"""Offline final E2 committed-step gate. Never sends faults or calls cloud APIs."""
try:
    from .collect import event_from_message,PREFIX,timestamp
except ImportError:
    from collect import event_from_message,PREFIX,timestamp
import json
from datetime import timedelta


def check(run,events,*,already_started=False,now,current_pods,max_age_seconds=30):
    v3=run.get('protocol')=='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118'
    world=4 if v3 else 2
    if (v3 and (run.get('arm')!='C' or run.get('world_size')!=4)) or (not v3 and (run.get('protocol')!='ieee-access-final-20261117' or run.get('arm')!='E2-B')):
        raise ValueError('final E2-B only')
    if already_started:return {'ready':False,'reason':'single_fault_already_started'}
    trigger=run.get('fault_trigger',{})
    if trigger.get('type')!='committed_global_step' or type(trigger.get('value')) is not int or not 0<trigger['value']<run['goal_steps']:
        raise ValueError('invalid predeclared committed-step trigger')
    if v3 and trigger['value']!=(run['goal_steps']+1)//2:raise ValueError('v3 fault must remain at50percent')
    if now and run.get('hard_deadline_utc') and timestamp(now)>=timestamp(run['hard_deadline_utc']):
        return {'ready':False,'reason':'deadline_reached'}
    if not run.get('policy_uid') or not run.get('workload_uid') or not run.get('started_at'):
        raise ValueError('live policy/workload UID and start time required')
    if set(current_pods)!=set(range(world)) or len(set(current_pods.values()))!=world or not all(current_pods.values()):
        raise ValueError('two distinct current Pod UIDs required')
    if not 0 < max_age_seconds <= 60:raise ValueError('bounded fresh progress required')
    latest={}
    for raw in events:
        if raw.get('run_id')!=run['run_id'] or raw.get('world_size')!=world or raw.get('rank') not in range(world):raise ValueError('foreign rank/run')
        if not raw.get('pod_uid') or not raw.get('attempt_id'):raise ValueError('rank attempt/Pod UID required')
        if raw['pod_uid']!=current_pods[raw['rank']]:continue
        e=event_from_message(PREFIX+json.dumps(raw));at=timestamp(e['timestamp'])
        if at<timestamp(run['started_at']):continue
        if now and at>timestamp(now):raise ValueError('future observation')
        if e['event'] not in ('step','completed'):continue
        rank=e['rank']
        # Time ordering matters: a restored rank may have rolled back. Never
        # retain its earlier high step as evidence of current committed progress.
        if rank not in latest or at>latest[rank][0]:latest[rank]=(at,e['global_step'],e['event'])
        elif at==latest[rank][0] and (e['global_step'],e['event'])!=latest[rank][1:]:raise ValueError('ambiguous rank observation')
    if len(latest)!=world:return {'ready':False,'reason':'both_rank_progress_required'}
    if any((timestamp(now)-v[0]).total_seconds()>max_age_seconds for v in latest.values()):return {'ready':False,'reason':'rank_progress_stale'}
    if any(v[2]=='completed' or v[1]>=run['goal_steps'] for v in latest.values()):return {'ready':False,'reason':'training_already_completed'}
    progress=min(v[1] for v in latest.values())
    return {'ready':progress>=trigger['value'],'reason':'step_reached' if progress>=trigger['value'] else 'step_pending','committed_step_lower_bound':progress,'trigger_step':trigger['value'],'progress_valid_until_utc':(min(v[0] for v in latest.values())+timedelta(seconds=max_age_seconds)).isoformat()}
