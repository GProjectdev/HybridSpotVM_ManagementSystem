"""Local-only B shadow/C admission observer over immutable collector snapshots.

Neither this command nor a ready receipt grants paid execution. The ordinary
operational watchdog remains mandatory. A first candidate is latched once;
C's separate dispatcher rechecks current cloud ownership and OD health.
"""
import argparse
from datetime import datetime, timezone, timedelta
import json
import time
from pathlib import Path
try:
    from . import post_risk_fault_gate as gate, post_risk_evidence as evidence
    from .final_fis import save
except ImportError:
    import post_risk_fault_gate as gate, post_risk_evidence as evidence
    from final_fis import save


def read(path):
    return json.loads(Path(path).read_text())


def safety_record(run, record):
    if record.get('run_id')!=run['run_id'] or record.get('phase')!='training':
        raise ValueError('current training safety record required')
    s=record['sample']
    if s.get('error'):raise ValueError('safety sensor unavailable')
    reserve=run.get('other_cost_reserve_usd')
    if not gate.number(reserve):raise ValueError('explicit other-cost reserve required')
    return dict(observed_at=record['at'],stop_requested=record['stop_requested'],
        nfs_free_bytes=s['nfs_free_bytes'],local_free_bytes=s['local_free_bytes'],
        estimated_cost_usd=s['estimated_GPU_and_root_cost_usd'],
        cleanup_cost_reserve_usd=s['cleanup_compute_reserve_usd']+reserve)


def enforce_first(run, first, current, now):
    """Refresh safety/progress, never select a later checkpoint or a new target.

    Dispatch grace equals the predeclared observation freshness window. It is
    a bounded tool latency allowance, not permission to defer to a later round.
    """
    if first.get('contract_sha256')!=gate.digest(gate.validate_contract(run)) or first.get('run_id')!=run['run_id']:
        raise ValueError('first receipt belongs to another contract')
    if not first.get('ready'):return dict(first,ready=False,terminal=True)
    for key in ('checkpoint_uid','checkpoint_id','saved_step','archives_sha256',
                'target_instance_id','target_rank','target_nodeprovision_uid','risk_applied_at'):
        if current.get('ready') and current.get(key)!=first.get(key):
            raise ValueError('first eligible checkpoint/target changed; never retarget')
    expiry=gate.stamp(first['evaluated_at'])+timedelta(seconds=run['post_risk_gate']['max_observation_age_seconds'])
    if gate.stamp(now)>=expiry:
        return dict(current,ready=False,terminal=True,reason='first_candidate_dispatch_window_expired')
    if current.get('ready'):
        current=dict(current,admission_valid_until_utc=min(expiry,gate.stamp(current['admission_valid_until_utc'])).isoformat())
    return current


def evaluate(directory, run, snapshot, watch_record, now):
    directory=Path(directory);snapshot=Path(snapshot)
    gate.validate_contract(run)
    application_path=directory/'post-risk-application.json'
    prior=read(application_path) if application_path.exists() else None
    snapshots=evidence.load_snapshot(snapshot)
    if snapshots['manifest'].get('namespace')!=run['namespace']:
        raise ValueError('snapshot namespace mismatch')
    events=read(snapshot/'training-events.json');decisions=read(snapshot/'policy-decisions.json')
    bundle=evidence.build(run,snapshots,events,decisions,safety_record(run,watch_record),application_receipt=prior)
    decision=gate.check(run,bundle,now)
    if bundle['risk_application'] and not prior:
        save(application_path,bundle['risk_application'],exclusive=True)
    first_path=directory/'post-risk-first-gate.json'
    if first_path.exists():
        first=read(first_path)
        # B's historical shadow event is final; subsequent progress does not
        # erase it or create a cloud authorization. C must still refresh gates.
        if run['arm']=='B':
            return dict(first,shadow_final=True),bundle
        decision=enforce_first(run,first,decision,now)
    else:
        gate.record_first(directory,run,decision)
    receipt=dict(decision,source_snapshot=str(snapshot.resolve()),bundle_sha256=gate.digest(bundle))
    # No writes to original snapshot/run/manifest or scientific evidence.
    with (directory/'post-risk-observations.jsonl').open('a') as f:
        f.write(json.dumps(receipt,allow_nan=False)+'\n')
    return receipt,bundle


def latest_inputs(directory):
    directory=Path(directory)
    candidates=list((directory/'snapshots').glob('*/manifest.json'))
    if not candidates:raise ValueError('no complete observer snapshot')
    newest=max(candidates,key=lambda p:gate.stamp(read(p)['observed_at'])).parent
    # JSONL writer may currently be appending: an incomplete last line is not
    # accepted. Caller retries normally; the separate watchdog remains active.
    lines=(directory/'safety-watch.jsonl').read_text().splitlines()
    if not lines:raise ValueError('no safety observation')
    return newest,json.loads(lines[-1])


def request_stop(directory, run, reason, now):
    path=Path(directory)/'post-risk-stop-request.json'
    value=dict(run_id=run['run_id'],policy_uid=run['policy_uid'],workload_uid=run['workload_uid'],
               at=now,reason=reason,normal_uid_cleanup_required=True)
    if not path.exists():save(path,value,exclusive=True)
    return value


def watch(directory, seconds, interval=10):
    if not 0<seconds<=18000 or not 1<=interval<=30:raise ValueError('bounded watch required')
    until=time.monotonic()+seconds; failures=0
    while time.monotonic()<until:
        run=read(Path(directory)/'run.json');now=datetime.now(timezone.utc).isoformat()
        if (Path(directory)/'cleanup-phase.json').exists() or run.get('phase')!='running':return
        try:
            snapshot,safety=latest_inputs(directory)
            result,_=evaluate(directory,run,snapshot,safety,now)
            failures=0
        except (ValueError,KeyError,TypeError,OSError) as error:
            failures+=1
            with (Path(directory)/'post-risk-observer-errors.jsonl').open('a') as f:
                f.write(json.dumps(dict(at=now,error=type(error).__name__,reason=str(error)[:300]))+'\n')
            if failures>=2:
                request_stop(directory,run,'two_post_risk_evidence_failures',now);return
        else:
            if result.get('eligible') or result.get('terminal'):
                benign_ineligibility={'goal_completed_without_gate','no_spot_at_first_eligible',
                    'active_operation_conflict','goal_may_finish_during_notice',
                    'insufficient_recovery_and_remaining_work_budget'}
                if (run['arm']=='C' and not result.get('ready')) or (
                        result.get('terminal') and result['reason'] not in benign_ineligibility):
                    request_stop(directory,run,'post_risk_gate_stop:'+result['reason'],now)
                return result
        if gate.stamp(now)>=gate.stamp(run['cleanup_start_utc']):return
        time.sleep(min(interval,max(0,until-time.monotonic())))
    # An accidentally short watch cannot silently remove required observation.
    run=read(Path(directory)/'run.json')
    if run.get('phase')=='running' and not (Path(directory)/'post-risk-first-gate.json').exists():
        request_stop(directory,run,'post_risk_watch_ended_without_receipt',datetime.now(timezone.utc).isoformat())


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--directory',type=Path,required=True)
    p.add_argument('--seconds',type=int,default=0,help='0=one local check; otherwise bounded watch after four-rank startup')
    a=p.parse_args()
    if a.seconds:
        print(json.dumps(watch(a.directory,a.seconds),allow_nan=False));return
    run=read(a.directory/'run.json')
    snapshot,safety=latest_inputs(a.directory)
    result,_=evaluate(a.directory,run,snapshot,safety,datetime.now(timezone.utc).isoformat())
    print(json.dumps(result,allow_nan=False))


if __name__=='__main__':main()
