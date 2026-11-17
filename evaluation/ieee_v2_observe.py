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
except ImportError:
    from collect import event_from_message, timestamp, PREFIX
    from e1_experiment import evidence_resource
    from ieee_v2 import ARMS, canonical_hash
    from run import save

KINDS = ('pods', 'statefulsets', 'noderequests', 'nodeprovisions', 'trainingruntimes',
         'spotriskprofiles', 'fluidcrmigrations', 'spotreplacements', 'spotrecoveries',
         'restorerequests', 'restoreplans', 'resourcebindings', 'propagationpolicies',
         'persistentvolumeclaims', 'events')


def validate_run(info):
    if info.get('protocol') != 'ieee-access-v2' or info.get('arm') not in ARMS:
        raise ValueError('not an IEEE v2 run')
    if info.get('namespace') != 'eval-v2-' + info.get('run_id', ''):
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


class Observer:
    def __init__(self, directory, kubeconfigs):
        self.directory = Path(directory)
        self.info = json.loads((self.directory / 'run.json').read_text())
        validate_run(self.info)
        self.ns = self.info['namespace']
        self.kubeconfigs = kubeconfigs

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
        for cluster in self.kubeconfigs:
            for kind in KINDS:
                try:
                    obj = json.loads(self.command(cluster, 'get', kind, '-o', 'json'))
                    if kind == 'pods' and cluster == 'aws':
                        pods = obj['items']
                    save(target / (cluster + '-' + kind + '.json'), evidence_resource(obj))
                except (RuntimeError, ValueError, subprocess.TimeoutExpired):
                    errors.append({'cluster':cluster, 'kind':kind, 'error':'unavailable'})
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
    a = p.parse_args()
    if not 0 <= a.seconds <= 18000 or not 5 <= a.interval <= 300:
        p.error('seconds must be 0..18000, interval 5..300')
    keys = {name:os.environ.get(env) for name,env in [('karmada','KARMADA_KUBECONFIG'),('aws','AWS_KUBECONFIG')]}
    if not all(keys.values()):
        p.error('KARMADA_KUBECONFIG and AWS_KUBECONFIG required')
    os.umask(0o077)
    observer = Observer(a.directory, keys)
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
