"""Experiment-only space/deadline/cost watch; no provisioning or FIS dispatch.

Default mode only observes. Optional normal cleanup requires a separate future
paid authorization receipt and reuses integrated_control's UID-precondition path.
The frozen experimental settings are never modified by this helper.
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


def observe(directory, run, key, now):
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
    if not 0 <= (now-stamp(last['observed_at'])).total_seconds() <= 90 or essential:
        raise ValueError('observer heartbeat stale or essential collection incomplete')
    result['noncritical_observer_errors_preserved']=noncritical
    ownership = json.loads((directory/'instance-ownership.json').read_text())
    history, terminal = {}, {}
    for path in sorted((directory/'snapshots').glob('*/aws-instance-ledger.json')):
        if not (path.parent/'manifest.json').exists():
            continue
        for row in json.loads(path.read_text()):
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
    if set(history) != set(ownership):
        raise ValueError('incomplete owned instance ledger')
    prices = [json.loads(p.read_text()) for p in (directory/'price-history').glob('*.json')]
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


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--directory',type=Path,required=True)
    p.add_argument('--authorization',type=Path,required=True)
    p.add_argument('--repository',type=Path,required=True)
    p.add_argument('--ssh-key',default='/home/ubuntu/jsj-key.pem')
    p.add_argument('--normal-cleanup-on-stop',action='store_true')
    args = p.parse_args()
    failures = 0
    while True:
        run = json.loads((args.directory/'run.json').read_text())
        now = datetime.now(timezone.utc)
        authorization(run,json.loads(args.authorization.read_text()),now)
        try:
            sample = observe(args.directory,run,args.ssh_key,now)
        except Exception as error:
            sample = {'error':type(error).__name__}
        state = decision(run,sample,now,failures);failures=state['consecutive_sensor_failures']
        record = dict(at=now.isoformat(),run_id=run['run_id'],sample=sample,**state)
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
                policy=control.policy()
                fis=args.directory/'fis-started.json'
                if fis.exists():
                    exp=json.loads(fis.read_text())['experiment']['id']
                    actual=control.cloud.command('fis','get-experiment','--id',exp)['experiment']
                    if (actual.get('tags',{}).get('evaluation-run')!=run['run_id'] or actual.get('tags',{}).get('policy-uid')!=run['policy_uid']):
                        raise ValueError('FIS ownership mismatch; manual investigation required')
                    if actual['state']['status'] in ['initiating','running']:
                        control.cloud.command('fis','stop-experiment','--id',exp)
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
