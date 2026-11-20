"""Read-only, fresh-namespace evidence collector for IEEE Access v2 runs.

Never requests Secret/ConfigMap contents. Captured trainer stdout is retained
only in private lossless gzip frames; public metrics still use the allowlist.
This observer is independent of the legacy runner's fixed namespace/arm names.
"""
import argparse
from datetime import datetime, timezone
import fcntl
import gzip
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import time
import uuid

try:
    from .collect import event_from_message, timestamp, PREFIX
    from .e1_experiment import evidence_resource
    from .ieee_v2 import ARMS, canonical_hash
    from .run import save
    from .ieee_v2_ledger import AWSReader, ownership
except ImportError:
    from collect import event_from_message, timestamp, PREFIX
    from e1_experiment import evidence_resource
    from ieee_v2 import ARMS, canonical_hash
    from run import save
    from ieee_v2_ledger import AWSReader, ownership

KINDS = ('pods', 'statefulsets', 'noderequests', 'nodeprovisions', 'trainingruntimes',
         'spotriskprofiles', 'fluidcrmigrations', 'spotreplacements', 'spotrecoveries',
         'restorerequests', 'restoreplans', 'resourcebindings', 'propagationpolicies',
         'persistentvolumeclaims', 'events')


def validate_run(info):
    if info.get('protocol') == 'HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118':
        if (info.get('arm') not in {'A','B','C'} or info.get('world_size')!=4
            or info.get('namespace')!='eval-v3-'+info.get('run_id','') or not info.get('workload_lock_sha256')):
            raise ValueError('four-rank v3 run/namespace/workload lock required')
        return
    final = info.get('protocol') == 'ieee-access-final-20261117'
    arms = {'E1-A','E1-B','E2-A','E2-B'} if final else ARMS
    if info.get('protocol') not in {'ieee-access-v2','ieee-access-final-20261117'} or info.get('arm') not in arms:
        raise ValueError('not a supported IEEE evaluation run')
    prefix = 'eval-final-' if final else 'eval-v2-'
    if info.get('namespace') != prefix + info.get('run_id', ''):
        raise ValueError('run namespace identity mismatch')
    if info.get('world_size') != 2 or not info.get('workload_lock_sha256'):
        raise ValueError('missing two-rank workload lock')


def log_events(text, info, pod):
    world = 4 if info.get('protocol')=='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118' else 2
    created = timestamp(pod['metadata']['creationTimestamp'])
    after = max(created, timestamp(info['started_at'])) if info.get('started_at') else created
    expected = next((e.get('value') for c in pod['spec']['containers'] if c['name'] == 'trainer'
                     for e in c.get('env', []) if e['name'] == 'EVAL_RUN_ID'), None)
    if expected != info['run_id']:
        raise ValueError('Pod trainer run_id mismatch')
    for line in text.splitlines():
        if PREFIX not in line:
            continue
        try:
            raw = json.loads(line.split(PREFIX, 1)[1])
            e = event_from_message(PREFIX + json.dumps(raw))
        except (TypeError, ValueError):
            continue
        if e is None or e['run_id'] != expected or e['rank'] not in range(world) or timestamp(e['timestamp']) < after:
            continue
        if raw.get('world_size') != world or not e.get('attempt_id'):
            continue
        e.update(world_size=world, pod_uid=pod['metadata']['uid'], pod_name=pod['metadata']['name'],
                 node=pod['spec'].get('nodeName', ''))
        yield e


def launcher_io_events(text, info, pod):
    """CRI-timestamped local I/O observations, separate from optimizer progress."""
    app=info.get('app');name=pod['metadata']['name'];uid=pod['metadata'].get('uid')
    if not app or not uid or not name.startswith(app+'-'):return
    try:rank=int(name[len(app)+1:])
    except ValueError:return
    world=info['world_size']
    if name!=app+'-'+str(rank) or world not in (2,4) or rank not in range(world):return
    expected=next((e.get('value') for c in pod['spec']['containers'] if c['name']=='trainer' for e in c.get('env',[]) if e['name']=='EVAL_RUN_ID'),None)
    if expected!=info['run_id']:raise ValueError('Pod trainer run_id mismatch')
    after=timestamp(pod['metadata']['creationTimestamp'])
    if info.get('started_at'):after=max(after,timestamp(info['started_at']))
    prefix='[Launcher lifecycle] checkpoint_buffer_materialized '
    for line in text.splitlines():
        if prefix not in line:continue
        try:
            at=line.split(' ',1)[0]
            if timestamp(at)<after:continue
            raw=json.loads(line.split(prefix,1)[1])
            n,w,t=raw['buffer_bytes'],raw['written_bytes'],raw['elapsed_seconds']
            if type(n) is not int or n<0 or type(w) is not int or type(t) not in (int,float) or not math.isfinite(t) or t<0:continue
            mode=raw['mode']
            if mode not in ('reuse-identical','atomic-write') or w!=(0 if mode=='reuse-identical' else n):continue
        except (TypeError,ValueError,KeyError):continue
        yield dict(event='launcher_checkpoint_io',timestamp=at,run_id=expected,rank=rank,world_size=world,
                   pod_uid=uid,pod_name=name,node=pod['spec'].get('nodeName',''),mode=mode,
                   buffer_bytes=n,written_bytes=w,elapsed_seconds=t,
                   event_semantics='launcher-local compare/write/fsync; NOT training progress or full restore latency')


POLICY_FIELDS = set("checkpointSafetyMaximumSeconds checkpointSafetyMaximumReached decisionActor observationSemantics alpha forecastHorizonSeconds spotPricePerHour onDemandPricePerHour priceEvaluated lossCostPerEviction lossCostObservedAt onDemandDurationSeconds checkpointSeconds copySeconds checkpointCostsObservedAt checkpointMinSeconds checkpointMaxSeconds paperAsyncProfileEnabled iterationTimeSeconds iterationObservedAt globalStep worldSize readyRanks runtimeReadyForCheckpoint policyUID policyNamespace policyName policyGeneration workloadUID riskName riskGeneration riskObservedGeneration riskObservedAt riskValidUntil riskFresh lambdaPerHour riskSource method vmPolicySource vmPolicyStage decisionStage intervalSource intervalCostEvaluated onDemandWorkers spotWorkers costEvaluated economicsSource checkpointIntervalSeconds onDemandHoldUntil reason decisionObservedAt".split())


def policy_events(text, policy_uid, namespace):
    for line in text.splitlines():
        try:
            raw = json.loads(line.split(' ',1)[1])
        except (ValueError, IndexError):
            continue
        if raw.get('msg') != 'EvaluationPolicyDecision' or raw.get('policyUID') != policy_uid or raw.get('policyNamespace') != namespace:
            continue
        yield {k:v for k,v in raw.items() if k in POLICY_FIELDS}


def trainer_log_skip_reason(pod):
    """Retired Pods remain in resource evidence, without blocking kubelet RPCs.

    A Running Pod with no deletion timestamp always keeps strict log collection.
    Final full stdout must be captured by the cleanup operator BEFORE deletion.
    """
    if pod['metadata'].get('deletionTimestamp'):
        return 'deleting'
    phase = pod.get('status', {}).get('phase')
    if phase == 'Pending':
        return 'phase-' + phase
    return None  # Terminal/unknown Pods still get final logs/audits before deletion.


def save_trainer_log_frame(target, pod_uid, text):
    """Preserve the exact captured UTF-8 response, including duplicate lines.

    This is a private capture receipt, not a guarantee of unobserved kubelet
    history or logs generated after a node disappears. No new log RPC is made.
    """
    import re
    if not re.fullmatch(r'[A-Za-z0-9_.-]+',pod_uid):
        raise ValueError('invalid Pod UID in private log frame')
    folder=target/'private-trainer-log-frames';folder.mkdir(mode=0o700,exist_ok=True)
    path=folder/(pod_uid+'.log.gz');payload=text.encode('utf-8')
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'wb') as raw:
        with gzip.GzipFile(fileobj=raw,mode='wb',compresslevel=1,mtime=0) as zipped:
            zipped.write(payload)
        raw.flush();os.fsync(raw.fileno())
    return dict(pod_uid=pod_uid,path=str(path.relative_to(target)),captured_bytes=len(payload),
                captured_utf8_sha256=hashlib.sha256(payload).hexdigest(),compressed_bytes=path.stat().st_size,
                private=True,rpc_limit_bytes=10000000,near_rpc_limit=len(payload)>=9500000)


class Observer:
    def __init__(self, directory, kubeconfigs, aws_reader=None):
        self.directory = Path(directory)
        self.info = json.loads((self.directory / 'run.json').read_text())
        validate_run(self.info)
        self.ns = self.info['namespace']
        self.kubeconfigs = kubeconfigs
        self.aws_reader = aws_reader
        cache = self.directory / "instance-ownership.json"
        self.owned = json.loads(cache.read_text()) if cache.exists() else {}

    def command(self, cluster, *args):
        p = subprocess.run(['kubectl', '--kubeconfig=' + self.kubeconfigs[cluster], '--request-timeout=15s',
                            '-n', self.ns, *args], capture_output=True, text=True, timeout=25)
        if p.returncode:
            # Do not serialize arbitrary command stderr into public evidence.
            raise RuntimeError('read-only kubectl failed for ' + cluster + '/' + args[0])
        return p.stdout

    def capture(self):
        instant = datetime.now(timezone.utc).isoformat()
        target = self.directory / 'snapshots' / (instant.replace(':', '-') + '-' + uuid.uuid4().hex[:8])
        target.mkdir(parents=True, mode=0o700)
        errors = []
        pods = []
        nodes = []
        policy_uid = None
        for cluster in self.kubeconfigs:
            if cluster == 'mgmt':continue
            for kind in KINDS:
                try:
                    obj = json.loads(self.command(cluster, 'get', kind, '-o', 'json'))
                    if kind == 'nodeprovisions' and cluster == 'aws':
                        nodes = obj['items']
                    if kind == 'noderequests' and cluster == 'karmada':
                        policies = [p for p in obj['items'] if p['metadata'].get('labels', {}).get('training.dcnlab.com/evaluation-run') == self.info['run_id']]
                        if len(policies) == 1:
                            policy_uid = policies[0]['metadata']['uid']
                    if kind == 'pods' and cluster == 'aws':
                        pods = obj['items']
                    save(target / (cluster + '-' + kind + '.json'), evidence_resource(obj))
                except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                    errors.append({'cluster':cluster, 'kind':kind, 'error':'unavailable'})
        if self.kubeconfigs.get('mgmt') and policy_uid:
            try:
                since = self.info.get('started_at', instant)
                decision_rows=[]
                for component in ('spot-vm-management-controller','checkpoint-recovery-coordinator'):
                    cmd = ['kubectl', '--kubeconfig='+self.kubeconfigs['mgmt'], '--request-timeout=15s',
                           '-n', 'hybridspot-system', 'logs', 'deployment/'+component,
                           '--timestamps=true', '--since-time='+since, '--limit-bytes=10000000']
                    p = subprocess.run(cmd, text=True, capture_output=True, timeout=25)
                    if p.returncode:
                        errors.append({'operation':'policy_decision_logs','component':component,'error':'unavailable'});continue
                    rows=list(policy_events(p.stdout,policy_uid,self.ns));decision_rows.extend(rows)
                    save(target / (component+'-decisions.json'), rows)
                save(target / 'policy-decisions.json',decision_rows)
            except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                errors.append({'operation':'policy_decision_logs','error':'unavailable'})
        if self.aws_reader:
            try:
                known_uids = {v['policy_uid'] for v in self.owned.values()}
                if policy_uid and known_uids and known_uids != {policy_uid}:
                    raise ValueError('run policy recreated')
                if policy_uid:
                    self.owned = ownership(nodes, policy_uid, self.owned)
                    save(self.directory / 'instance-ownership.json', self.owned)
                save(target / 'aws-instance-ledger.json', self.aws_reader.observe(self.owned, instant))
            except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                errors.append({'operation':'exact_instance_ledger', 'error':'unavailable_or_identity_mismatch'})
        events = []
        io_events = []
        skipped_logs = []
        log_frames = []
        for pod in pods:
            if pod.get('metadata', {}).get('labels', {}).get('training.dcnlab.com/evaluation-run') != self.info['run_id']:
                continue
            skip = trainer_log_skip_reason(pod)
            if skip:
                skipped_logs.append({'pod_uid':pod['metadata']['uid'], 'reason':skip})
                continue
            try:
                text = self.command('aws', 'logs', pod['metadata']['name'], '-c', 'trainer', '--timestamps=true', '--limit-bytes=10000000')
                log_frames.append(save_trainer_log_frame(target,pod['metadata']['uid'],text))
                events.extend(log_events(text, self.info, pod))
                io_events.extend(launcher_io_events(text, self.info, pod))
            except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                errors.append({'pod_uid':pod['metadata']['uid'], 'error':'trainer_logs_unavailable'})
        # Per-snapshot original sanitized observations are immutable. Cumulative
        # JSONL may repeat observations; metrics/export deduplicate exact events.
        save(target / 'training-events.json', events)
        save(target / 'launcher-io-events.json', io_events)
        with (self.directory / 'launcher-io-events.jsonl').open('a') as f:
            for e in io_events:f.write(json.dumps(e,sort_keys=True)+'\n')
        with (self.directory / 'training-events.jsonl').open('a') as f:
            for e in events:
                f.write(json.dumps(e, sort_keys=True) + '\n')
        save(target / 'manifest.json', {'observed_at':instant, 'namespace':self.ns, 'errors':errors,
                                       'completed_at':datetime.now(timezone.utc).isoformat(),
                                       'trainer_logs_skipped':skipped_logs,
                                       'private_trainer_log_frames':log_frames,
                                       'events_sha256':canonical_hash(events)})
        return {'snapshot':str(target), 'errors':errors, 'events':len(events)}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--directory', type=Path, required=True)
    p.add_argument('--seconds', type=int, default=0, help='0 = once; bounded read-only watch otherwise')
    p.add_argument('--interval', type=int, default=20)
    p.add_argument('--aws-profile')
    p.add_argument('--aws-account-id')
    p.add_argument('--aws-region', default='ap-northeast-2')
    a = p.parse_args()
    if bool(a.aws_profile) != bool(a.aws_account_id):
        p.error('AWS profile and expected account must be supplied together')
    if not 0 <= a.seconds <= 18000 or not 5 <= a.interval <= 300:
        p.error('seconds must be 0..18000, interval 5..300')
    keys = {name:os.environ.get(env) for name,env in [('karmada','KARMADA_KUBECONFIG'),('aws','AWS_KUBECONFIG')]}
    if not all(keys.values()):
        p.error('KARMADA_KUBECONFIG and AWS_KUBECONFIG required')
    if os.environ.get('MGMT_KUBECONFIG'):
        keys['mgmt'] = os.environ['MGMT_KUBECONFIG']
    os.umask(0o077)
    reader = AWSReader(a.aws_profile, a.aws_account_id, a.aws_region) if a.aws_profile else None
    observer = Observer(a.directory, keys, reader)
    with (a.directory / '.observer.lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        end = time.monotonic() + a.seconds
        while True:
            print(json.dumps(observer.capture()), flush=True)
            if time.monotonic() >= end:
                break
            time.sleep(min(a.interval, max(0, end-time.monotonic())))


if __name__ == '__main__':
    main()
