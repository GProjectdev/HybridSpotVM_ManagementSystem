"""Read-only, fresh-namespace evidence collector for IEEE Access v2 runs.

Never requests Secret/ConfigMap contents or writes raw application log lines.
This observer is independent of the legacy runner's fixed namespace/arm names.
"""
import argparse
from datetime import datetime, timezone
import fcntl
import json
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
        if e is None or e['run_id'] != expected or e['rank'] not in (0, 1) or timestamp(e['timestamp']) < after:
            continue
        if raw.get('world_size') != 2 or not e.get('attempt_id'):
            continue
        e.update(world_size=2, pod_uid=pod['metadata']['uid'], pod_name=pod['metadata']['name'],
                 node=pod['spec'].get('nodeName', ''))
        yield e


POLICY_FIELDS = set("policyUID policyNamespace policyName policyGeneration workloadUID riskName riskGeneration riskObservedGeneration riskObservedAt riskValidUntil riskFresh lambdaPerHour riskSource method vmPolicySource vmPolicyStage decisionStage intervalSource intervalCostEvaluated onDemandWorkers spotWorkers costEvaluated economicsSource checkpointIntervalSeconds onDemandHoldUntil reason decisionObservedAt".split())


def policy_events(text, policy_uid, namespace):
    for line in text.splitlines():
        try:
            raw = json.loads(line.split(' ',1)[1])
        except (ValueError, IndexError):
            continue
        if raw.get('msg') != 'EvaluationPolicyDecision' or raw.get('policyUID') != policy_uid or raw.get('policyNamespace') != namespace:
            continue
        yield {k:v for k,v in raw.items() if k in POLICY_FIELDS}


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
                cmd = ['kubectl', '--kubeconfig='+self.kubeconfigs['mgmt'], '--request-timeout=15s',
                       '-n', 'hybridspot-system', 'logs', 'deployment/spot-vm-management-controller',
                       '--timestamps=true', '--since-time='+since, '--limit-bytes=10000000']
                p = subprocess.run(cmd, text=True, capture_output=True, timeout=25)
                if p.returncode:raise RuntimeError('policy logs unavailable')
                save(target / 'policy-decisions.json', list(policy_events(p.stdout, policy_uid, self.ns)))
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
        for pod in pods:
            if pod.get('metadata', {}).get('labels', {}).get('training.dcnlab.com/evaluation-run') != self.info['run_id']:
                continue
            try:
                text = self.command('aws', 'logs', pod['metadata']['name'], '-c', 'trainer', '--limit-bytes=10000000')
                events.extend(log_events(text, self.info, pod))
            except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                errors.append({'pod_uid':pod['metadata']['uid'], 'error':'trainer_logs_unavailable'})
        # Per-snapshot original sanitized observations are immutable. Cumulative
        # JSONL may repeat observations; metrics/export deduplicate exact events.
        save(target / 'training-events.json', events)
        with (self.directory / 'training-events.jsonl').open('a') as f:
            for e in events:
                f.write(json.dumps(e, sort_keys=True) + '\n')
        save(target / 'manifest.json', {'observed_at':instant, 'namespace':self.ns, 'errors':errors,
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
