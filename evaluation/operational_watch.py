"""Experiment-only space/deadline/cost watch; no provisioning or FIS dispatch.

Default mode only observes. Optional normal cleanup requires a separate future
paid authorization receipt and reuses integrated_control's UID-precondition path.
Operational amendment 20261120; frozen parent tools and scientific settings stay immutable.
"""
import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import subprocess
import sys
import time

GIB = 1024**3


def stamp(value):
    t = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if t.tzinfo is None:
        raise ValueError('explicit timezone required')
    return t.astimezone(timezone.utc)


def authorization(run, receipt, now):
    if (run.get('paid_authorization') is not True or receipt.get('authorized') is not True
            or receipt.get('run_id') != run['run_id']
            or receipt.get('frozen_manifest_sha256') != run.get('frozen_manifest_sha256')
            or receipt.get('hard_deadline_utc') != run.get('hard_deadline_utc')
            or receipt.get('budget_usd') != run.get('approved_budget_usd')
            or not receipt.get('researcher_message_reference')
            or not run.get('policy_uid') or not run.get('started_at')):
        raise ValueError('separate exact-run paid authorization/UID binding required')
    if not (type(run.get('approved_budget_usd')) in (int,float) and math.isfinite(run['approved_budget_usd']) and run['approved_budget_usd']>0):
        raise ValueError('finite positive approved budget required')
    if now < stamp(run['started_at']):
        raise ValueError('run has not started')
    if not (0 < (stamp(run['hard_deadline_utc'])-stamp(run['started_at'])).total_seconds() <= 18000
            and (stamp(run['hard_deadline_utc'])-stamp(run['cleanup_start_utc'])).total_seconds() >= 1200):
        raise ValueError('invalid absolute execution/cleanup window')


def decision(run, sample, now, failures):
    reasons = []
    if now >= stamp(run['cleanup_start_utc']):
        reasons.append('absolute_cleanup_deadline')
    if sample.get('error'):
        failures += 1
        if failures >= 2:
            reasons.append('two_consecutive_safety_sensor_failures')
    else:
        failures = 0
        if sample['nfs_free_bytes'] <= run['nfs_stop_free_GiB']*GIB:
            reasons.append('NFS_free_headroom')
        if sample['run_allocated_bytes'] >= run['active_paths_stop_GiB']*GIB:
            reasons.append('owned_run_storage_envelope')
        if sample['local_free_bytes'] <= 80*GIB:
            reasons.append('management_evidence_disk_headroom')
        if sample['nonterminal_owned_instances'] > run['max_expected_worker_instances']:
            reasons.append('unexpected_concurrent_owned_capacity')
        if (sample['estimated_GPU_and_root_cost_usd'] + sample['cleanup_compute_reserve_usd']
                + run['other_cost_reserve_usd'] >= run['approved_budget_usd']):
            reasons.append('cost_estimate_plus_cleanup_reserve')
    return {'stop_requested': bool(reasons), 'reasons': reasons, 'consecutive_sensor_failures': failures}


def storage_sample(directory, run, key):
    paths = run.get('storage_paths', [])
    if not paths or any(not p.startswith('/mnt/nfs/') or run['namespace'] not in p or '..' in Path(p).parts for p in paths):
        raise ValueError('bind exact UID-verified run storage roots first')
    # Input is base64 JSON, not interpolated shell code. No remote file mutation.
    optional=run.get('optional_storage_paths',[])
    if any(not p.startswith('/mnt/nfs/') or run['namespace'] not in p or '..' in Path(p).parts for p in optional):
        raise ValueError('invalid optional run root')
    encoded = base64.b64encode(json.dumps({'required':paths,'optional':optional}).encode()).decode()
    remote = '''import sys,json,base64,os,subprocess
binding=json.loads(base64.b64decode(sys.argv[1]));paths=binding['required']+binding['optional'];st=os.statvfs('/mnt/nfs');total=0
for p in paths:
 if not os.path.exists(p) and p in binding['optional']:continue
 if not os.path.isdir(p) or os.path.islink(p):raise RuntimeError('missing/invalid bound storage root')
 total+=int(subprocess.check_output(['du','-x','-B1','-s','--',p],text=True).split()[0])
print(json.dumps({'nfs_free_bytes':st.f_bavail*st.f_frsize,'run_allocated_bytes':total}))'''
    proc = subprocess.run(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=10','-i',key,
                           'ubuntu@13.125.179.70','sudo','python3','-',encoded],
                          input=remote,text=True,capture_output=True,timeout=25)
    if proc.returncode:
        raise RuntimeError('NFS safety sensor unavailable')
    result = json.loads(proc.stdout)
    st = __import__('os').statvfs(directory)
    result['local_free_bytes'] = st.f_bavail*st.f_frsize
    return result


def observe(directory, run, key, now):
    result = storage_sample(directory, run, key)
    snapshots = [p for p in (directory/'snapshots').glob('*/manifest.json')]
    if not snapshots:
        raise ValueError('no complete observer heartbeat')
    newest = max(snapshots, key=lambda p: stamp(json.loads(p.read_text())['observed_at']))
    last = json.loads(newest.read_text())
    pods=json.loads((newest.parent/'aws-pods.json').read_text())['items']
    active={p['metadata']['uid'] for p in pods if not p['metadata'].get('deletionTimestamp') and p.get('status',{}).get('phase')=='Running'}
    essential=[];noncritical=[]
    for error in last.get('errors',[]):
        not_applicable=error.get('cluster')=='aws' and error.get('kind') in ['resourcebindings','propagationpolicies']
        retired_or_not_running=error.get('error')=='trainer_logs_unavailable' and error.get('pod_uid') not in active
        (noncritical if not_applicable or retired_or_not_running else essential).append(error)
    if not 0 <= (now-stamp(last['observed_at'])).total_seconds() <= 90:
        raise ValueError('observer heartbeat stale or essential collection incomplete')
    # A live Running rank remains strict unless it is the exact positively lost
    # Spot from this Run's already completed FIS action. Other collection errors
    # are never hidden by an experiment being in progress.
    fault_candidate = (run.get('arm') == 'C' and essential
                       and all(e.get('error') == 'trainer_logs_unavailable' for e in essential)
                       and (directory/'fis-started.json').exists())
    if essential and not fault_candidate:
        raise ValueError('observer heartbeat stale or essential collection incomplete')
    # Bind the ownership set before an independent provider query. The observer
    # publishes growing ownership before its complete snapshot, so joining that
    # new set to only older ledgers can falsely report a missing replacement.
    try:
        from .ieee_v2_ledger import AWSReader
    except ImportError:
        from ieee_v2_ledger import AWSReader
    bound_at=datetime.now(timezone.utc)
    owned=json.loads((directory/'instance-ownership.json').read_text())
    reader=AWSReader(run['aws_profile'],run['expected_aws_account'],run['region'])
    began=datetime.now(timezone.utc)
    rows=reader.observe(owned,began.isoformat())
    ended=datetime.now(timezone.utc)
    retired = validate_current_ledger(run,owned,rows,ended,directory=directory)
    if fault_candidate:
        try:
            from .fis_observation_guard import expected_lost_pod_uids
        except ImportError:
            from fis_observation_guard import expected_lost_pod_uids
        load=lambda name:json.loads((directory/name).read_text())
        started=load('fis-started.json')
        experiment=reader.command('fis','get-experiment','--id',started['experiment']['id'])['experiment']
        ended=datetime.now(timezone.utc)
        allowed=expected_lost_pod_uids(run,load('authorization.json'),load('fis-final-plan.json'),
            started,experiment,pods,json.loads((newest.parent/'aws-nodeprovisions.json').read_text())['items'],owned,rows,ended)
        if any(e.get('pod_uid') not in allowed for e in essential):
            raise ValueError('non-target Running rank log is still essential')
        result['expected_FIS_target_log_unavailable']=[dict(e,verified_experiment_id=experiment['id']) for e in essential]
    with (directory/'training-watch-ledger.jsonl').open('a') as f:
        f.write(json.dumps(dict(at=ended.isoformat(),ownership_bound_at=bound_at.isoformat(),
                               owned=owned,rows=rows,prior_termination_receipts=retired))+'\n')
    result['noncritical_observer_errors_preserved']=noncritical
    result['absent_retired_instances_with_positive_receipts']=sorted(retired)
    return ledger_sample(directory, run, ended, result, rows,
                         owned_override=owned, history_cutoff=bound_at)


def ledger_sample(directory, run, now, result, current_rows=None, *, owned_override=None, history_cutoff=None):
    ownership = (owned_override if owned_override is not None
                 else json.loads((directory/'instance-ownership.json').read_text()))
    history, terminal = {}, {}
    groups = [json.loads(path.read_text()) for path in sorted((directory/'snapshots').glob('*/aws-instance-ledger.json')) if (path.parent/'manifest.json').exists()]
    if history_cutoff is not None:
        groups=[[r for r in group if stamp(r['observed_at']) <= history_cutoff] for group in groups]
    if current_rows is not None:
        groups.append(current_rows)
    for group in groups:
        for row in group:
            iid = row['instance_id']
            if (iid not in ownership or row.get('policy_uid') != run['policy_uid']
                    or row.get('nodeprovision_uid') != ownership[iid].get('nodeprovision_uid')):
                raise ValueError('ledger UID identity mismatch')
            if row['state'] != 'NOT_OBSERVED' and not row.get('provider_ownership_verified'):
                raise ValueError('provider ownership unverified')
            if row['state'] == 'terminated':
                terminal[iid] = min(terminal.get(iid,stamp(row['observed_at'])),stamp(row['observed_at']))
            if iid not in history or stamp(row['observed_at']) > stamp(history[iid]['observed_at']):
                history[iid] = row
    if current_rows is not None:
        validate_current_ledger(run, ownership, current_rows, now, directory=directory)
    if set(history) != set(ownership):
        raise ValueError('incomplete owned instance ledger')
    if current_rows is not None and any(r['instance_id'] in terminal and r['state'] not in {'terminated','NOT_OBSERVED'} for r in current_rows):
        raise ValueError('terminal receipt conflicts with current provider state')
    prices = [json.loads(p.read_text()) for p in (directory/'price-history').glob('*.json')]
    prices = [p for p in prices if stamp(p['observedAt']) <= now]
    if not prices or not 0 <= (now-max(stamp(p['observedAt']) for p in prices)).total_seconds() <= 360:
        raise ValueError('fresh price receipts required for safety estimate')
    rate = max(max(p['spotPricePerHour'],p['onDemandPricePerHour']) for p in prices)
    if rate <= 0 or not __import__('math').isfinite(rate):
        raise ValueError('invalid safety price')
    gpu_hours, nonterminal = 0.0, 0
    for iid, row in history.items():
        if row['state'] == 'NOT_OBSERVED' and iid not in terminal:
            raise ValueError('unknown termination is not zero cost')
        start = row.get('launch_time')
        if not start:
            prior = [json.loads(p.read_text()) for p in (directory/'snapshots').glob('*/aws-instance-ledger.json')]
            start = next((r.get('launch_time') for group in prior for r in group if r['instance_id']==iid and r.get('launch_time')),None)
        if not start:
            raise ValueError('missing billed interval start')
        finish = terminal.get(iid,now)
        seconds = (finish-stamp(start)).total_seconds()
        if seconds < 0:
            raise ValueError('negative execution interval')
        gpu_hours += max(60,seconds)/3600
        nonterminal += iid not in terminal
    result.update(nonterminal_owned_instances=nonterminal,
                  estimated_GPU_and_root_cost_usd=gpu_hours*(rate+100*.0912/720),
                  cleanup_compute_reserve_usd=run['max_expected_worker_instances']*rate/3,
                  cost_semantics='gross conservative live price/termination-observation estimate; not invoice or scientific cost metric')
    return result


def file_sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def ownership_document_sha(path):
    """Bind every ownership field while allowing equivalent JSON encodings.

    The observer and cleanup operator both atomically rewrite this ledger, but
    use different whitespace. Formatting must not look like an ownership change.
    Completion evidence remains bound to its original, immutable file bytes.
    """
    document = json.loads(path.read_text())
    encoded = json.dumps(document, sort_keys=True, separators=(',', ':'),
                         ensure_ascii=True, allow_nan=False).encode('utf-8')
    return hashlib.sha256(encoded).hexdigest()


def completion_proof(run, proof, now):
    goal = run['goal_steps']; world = run['world_size']
    if not 4 == world or not stamp(run['started_at']) <= stamp(proof['at']) <= now:
        raise ValueError('invalid all-rank completion time')
    if proof.get('rank_steps') != {str(r):goal for r in range(world)} or sorted(proof.get('completed_ranks',[])) != list(range(world)):
        raise ValueError('all ranks must reach the frozen goal')
    audits=proof.get('audits',[])
    if len(audits)!=world or {a.get('rank') for a in audits}!=set(range(world)):
        raise ValueError('one completion audit per rank required')
    for a in audits:
        if (a.get('passed') is not True or a.get('event')!='completion_audit'
            or a.get('run_id')!=run['run_id'] or a.get('global_step')!=goal
            or a.get('world_size')!=world or not a.get('pod_uid') or not a.get('attempt_id')
            or not stamp(run['started_at'])<=stamp(a['timestamp'])<=stamp(proof['at'])):
            raise ValueError('invalid rank completion audit')
    for field in ('model_sha256','optimizer_sha256','data_contract_sha256'):
        values={a.get(field) for a in audits}
        if len(values)!=1 or not all(isinstance(v,str) and __import__('re').fullmatch('[0-9a-f]{64}',v) for v in values):
            raise ValueError('rank state audit mismatch')


def enter_cleanup(directory, run, now):
    """No cloud mutation. Call immediately before normal success quiesce.

    The CLI additionally verifies the still-live source UID and authorization.
    An unbound legacy cleanup-requested.json never disables training monitoring.
    """
    proof=json.loads((directory/'formal-completion-observed.json').read_text())
    completion_proof(run,proof,now)
    owned=json.loads((directory/'instance-ownership.json').read_text())
    if not owned or any(v.get('policy_uid')!=run['policy_uid'] or not v.get('nodeprovision_uid') or v.get('nodeprovision_namespace')!=run['namespace'] for v in owned.values()):
        raise ValueError('exact cleanup ownership binding required')
    receipt=dict(schema='success-cleanup-phase-v2',at=now.isoformat(),run_id=run['run_id'],
                 namespace=run['namespace'],policy_uid=run['policy_uid'],
                 frozen_manifest_sha256=run['frozen_manifest_sha256'],
                 ownership_sha256=ownership_document_sha(directory/'instance-ownership.json'),
                 completion_sha256=file_sha(directory/'formal-completion-observed.json'))
    # Atomic and exclusive: monitor never sees a partial receipt; no replacement.
    temporary=directory/('.cleanup-phase-'+__import__('uuid').uuid4().hex)
    try:
        with temporary.open('x') as f:
            json.dump(receipt,f,indent=2);f.flush();__import__('os').fsync(f.fileno())
        __import__('os').link(temporary,directory/'cleanup-phase.json')
    finally:
        temporary.unlink(missing_ok=True)
    return receipt


def cleanup_phase(directory, run, now):
    path=directory/'cleanup-phase.json'
    if not path.exists():return None
    r=json.loads(path.read_text())
    schema = r.get('schema')
    ownership_sha = (ownership_document_sha(directory/'instance-ownership.json')
                     if schema == 'success-cleanup-phase-v2'
                     else file_sha(directory/'instance-ownership.json'))
    if (schema not in {'success-cleanup-phase-v1', 'success-cleanup-phase-v2'} or any(r.get(k)!=run.get(k) for k in
        ('run_id','namespace','policy_uid','frozen_manifest_sha256'))
        or r.get('ownership_sha256')!=ownership_sha
        or r.get('completion_sha256')!=file_sha(directory/'formal-completion-observed.json')
        or not stamp(run['started_at'])<=stamp(r['at'])<=now):
        raise ValueError('invalid UID-bound cleanup phase receipt')
    proof=json.loads((directory/'formal-completion-observed.json').read_text())
    completion_proof(run,proof,stamp(r['at']))
    return r


def cleanup_policy(policy, run, receipt):
    if policy is None:
        if receipt is None:raise ValueError('source absent without verified cleanup phase')
        return None
    if policy['metadata']['uid']!=run['policy_uid']:
        raise ValueError('recreated source: refuse foreign cleanup')
    return policy


def validate_current_ledger(run, owned, rows, now, *, directory=None):
    # A fresh exact-ID query may omit an instance that already terminated.
    # Reuse only the original UID-bound positive receipt; never turn absence
    # into a new termination timestamp or relax live-provider checks.
    retired = {}
    missing = {r['instance_id']: owned[r['instance_id']] for r in rows
               if r.get('state') == 'NOT_OBSERVED' and r.get('instance_id') in owned}
    if directory is not None and missing:
        try:
            from .ieee_v2_ledger import historical_termination_receipts
        except ImportError:
            from ieee_v2_ledger import historical_termination_receipts
        retired = historical_termination_receipts(directory, run, missing)
    if len(rows)!=len(owned) or {r['instance_id'] for r in rows}!=set(owned):
        raise ValueError('incomplete current cleanup ledger')
    for row in rows:
        if (row.get('policy_uid')!=run['policy_uid'] or row.get('nodeprovision_namespace')!=run['namespace'] or row.get('nodeprovision_uid')!=owned[row['instance_id']]['nodeprovision_uid']
            or (row.get('state') != 'NOT_OBSERVED' and not row.get('provider_ownership_verified'))
            or row.get('state') not in
            {'pending','running','stopping','stopped','shutting-down','terminated','NOT_OBSERVED'}
            or not 0<=(now-stamp(row['observed_at'])).total_seconds()<=90):
            raise ValueError('stale/unknown/foreign current cleanup ledger')
        if row['state'] == 'NOT_OBSERVED':
            proof = retired.get(row['instance_id'])
            if (not proof or not stamp(run['started_at']) <=
                    stamp(proof['ledger_row']['launch_time']) <=
                    stamp(proof['ledger_row']['observed_at']) <= stamp(row['observed_at'])):
                raise ValueError('absent instance lacks prior UID-bound positive termination')
    return retired


def observe_cleanup(directory, run, key, now):
    # No training Pod RPC or live-policy requirement once success is proven.
    # AWS remains a fresh independent sensor, never inferred from disappearance.
    if cleanup_phase(directory,run,now) is None:raise ValueError('cleanup receipt required')
    try:
        from .ieee_v2_ledger import AWSReader
        from .integrated_control import Control
    except ImportError:
        from ieee_v2_ledger import AWSReader
        from integrated_control import Control
    control=Control(directory,run['kubeconfigs'],run['aws_profile'])
    cleanup_policy(control.get('karmada','noderequest',run['policy']),run,True)
    owned=json.loads((directory/'instance-ownership.json').read_text())
    began=datetime.now(timezone.utc)
    rows=control.cloud.observe(owned,began.isoformat())
    ended=datetime.now(timezone.utc)
    retired = validate_current_ledger(run,owned,rows,ended,directory=directory)
    with (directory/'cleanup-watch-ledger.jsonl').open('a') as f:
        f.write(json.dumps(dict(at=ended.isoformat(),rows=rows,prior_termination_receipts=retired))+'\n')
    result=ledger_sample(directory,run,ended,storage_sample(directory,run,key),rows)
    result['phase']='cleanup'
    result['absent_retired_instances_with_positive_receipts']=sorted(retired)
    result['positive_EC2_termination_observed']=all(r['state']=='terminated' or r['instance_id'] in retired for r in rows)
    # EBS/ENI/keypairs/temporary resources still require the normal cleanup audit.
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--directory',type=Path,required=True)
    p.add_argument('--authorization',type=Path,required=True)
    p.add_argument('--repository',type=Path,required=True)
    p.add_argument('--ssh-key',default='/home/ubuntu/jsj-key.pem')
    p.add_argument('--normal-cleanup-on-stop',action='store_true')
    p.add_argument('--enter-cleanup',action='store_true',help='bind successful all-rank completion before normal deletion')
    args = p.parse_args()
    if args.enter_cleanup:
        run=json.loads((args.directory/'run.json').read_text());now=datetime.now(timezone.utc)
        authorization(run,json.loads(args.authorization.read_text()),now)
        sys.path.insert(0,str(args.repository))
        from evaluation.integrated_control import Control
        control=Control(args.directory,run['kubeconfigs'],run['aws_profile'])
        control.policy()  # must still be the exact live source before deletion
        enter_cleanup(args.directory,run,now)
        return
    failures = 0
    while True:
        run = json.loads((args.directory/'run.json').read_text())
        now = datetime.now(timezone.utc)
        authorization(run,json.loads(args.authorization.read_text()),now)
        try:
            phase = cleanup_phase(args.directory, run, now)
            sample = (observe_cleanup(args.directory,run,args.ssh_key,now) if phase else observe(args.directory,run,args.ssh_key,now))
        except Exception as error:
            sample = {'error':type(error).__name__, 'reason':str(error)[:300]}
        state = decision(run,sample,now,failures);failures=state['consecutive_sensor_failures']
        record = dict(at=now.isoformat(),run_id=run['run_id'],sample=sample,**state)
        record['phase'] = 'cleanup' if (args.directory/'cleanup-phase.json').exists() else 'training'
        with (args.directory/'safety-watch.jsonl').open('a') as f:
            f.write(json.dumps(record,allow_nan=False)+'\n')
        if state['stop_requested']:
            with (args.directory/'safety-stop-request.json').open('x') as f:
                json.dump(record,f,indent=2)
            if args.normal_cleanup_on_stop:
                # Independent paid receipt validated above; no provisioning path.
                sys.path.insert(0,str(args.repository))
                from evaluation.integrated_control import Control
                control=Control(args.directory,run['kubeconfigs'],run['aws_profile'])
                policy=cleanup_policy(control.get('karmada','noderequest',run['policy']), run, cleanup_phase(args.directory,run,now))
                fis=args.directory/'fis-started.json'
                if fis.exists():
                    exp=json.loads(fis.read_text())['experiment']['id']
                    actual=control.cloud.command('fis','get-experiment','--id',exp)['experiment']
                    if (actual.get('tags',{}).get('evaluation-run')!=run['run_id'] or actual.get('tags',{}).get('policy-uid')!=run['policy_uid']):
                        raise ValueError('FIS ownership mismatch; manual investigation required')
                    if actual['state']['status'] in ['initiating','running']:
                        control.cloud.command('fis','stop-experiment','--id',exp)
                if policy is None:
                    # Normal cleanup is already in progress. Reuse the UID-safe
                    # quiesce path; it accepts absence and rejects replacements.
                    control.quiesce()
                    return
                control.kub('karmada','-n',run['namespace'],'patch','noderequest',run['policy'],
                            '--type=json','--patch-file=/dev/stdin',obj=[
                                {'op':'test','path':'/metadata/uid','value':run['policy_uid']},
                                {'op':'add','path':'/metadata/annotations/training.dcnlab.com~1suspend','value':'true'}])
                # A separate approved cleanup operator handles in-flight CP/FIS
                # receipts. Allow bounded drain, then request normal UID cleanup.
                time.sleep(min(120,max(0,(stamp(run['hard_deadline_utc'])-now).total_seconds()-1200)))
                control.quiesce()
            return
        time.sleep(min(15,max(0,(stamp(run['cleanup_start_utc'])-now).total_seconds())))


if __name__=='__main__':
    main()
