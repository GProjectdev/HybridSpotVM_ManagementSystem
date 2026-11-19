"""Offline R2/R3 observations from UID-bound observer snapshots, without cloud calls.

Snapshot time is a polling observation, never an exact policy/VM event time.
Policy decision and checkpoint coordinator branches are retained independently.
Missing fields remain null. This report does not estimate causal policy benefit.
"""
import argparse
import csv
import hashlib
import json
from pathlib import Path

LABEL = 'training.dcnlab.com/'
FIELDS = ('observed_at', 'policy_uid', 'generation', 'policy_observed_at', 'lambda_per_hour',
          'vm_policy_stage', 'desired_on_demand', 'desired_spot', 'decision_stage',
          'policy_interval_source', 'policy_interval_seconds', 'checkpoint_interval_source',
          'checkpoint_interval_seconds', 'checkpoint_reason', 'cost_observed_at',
          'checkpoint_cost_seconds', 'copy_cost_seconds')


def policy_row(obj, run, observed_at):
    m, spec, status = obj.get('metadata', {}), obj.get('spec', {}), obj.get('status', {})
    if (not run.get('policy_uid') or not run.get('workload_uid')
        or m.get('uid') != run['policy_uid'] or m.get('namespace') != run['namespace']
        or m.get('labels', {}).get(LABEL+'evaluation-run') != run['run_id']
        or spec.get('workloadRef', {}).get('uid') != run['workload_uid']):
        return None
    p, cp = status.get('policy', {}), status.get('checkpoint', {})
    costs = cp.get('measuredCosts', {})
    values = (observed_at, m['uid'], m.get('generation'), p.get('observedAt'),
              p.get('lambdaPerHour'), p.get('vmPolicyStage'), p.get('onDemandWorkers'),
              p.get('spotWorkers'), p.get('decisionStage'), p.get('intervalSource'),
              p.get('checkpointIntervalSeconds'), cp.get('intervalSource'),
              cp.get('checkpointIntervalSeconds'), cp.get('reason'), costs.get('observedAt'),
              costs.get('checkpointSeconds'), costs.get('copySeconds'))
    return dict(zip(FIELDS, values))


def summarize(directory):
    directory = Path(directory)
    run = json.loads((directory/'run.json').read_text())
    rows, files, errors, vm_observations = [], [], [], []
    for snapshot in sorted((directory/'snapshots').iterdir()):
        if not snapshot.is_dir(): continue
        manifest = snapshot/'manifest.json'
        if not manifest.exists():
            errors.append({'snapshot': snapshot.name, 'missing': 'manifest.json'})
            continue
        observed = json.loads(manifest.read_text()).get('observed_at')
        if not observed:
            errors.append({'snapshot': snapshot.name, 'missing': 'observed_at'})
            continue
        for name in ('karmada-noderequests.json', 'aws-instance-ledger.json'):
            path = snapshot/name
            if not path.exists():
                errors.append({'snapshot': snapshot.name, 'missing': name})
                continue
            raw = path.read_bytes()
            files.append({'path': str(path.relative_to(directory)), 'sha256': hashlib.sha256(raw).hexdigest()})
            data = json.loads(raw)
            if name.startswith('karmada'):
                for obj in data.get('items', []):
                    row = policy_row(obj, run, observed)
                    if row: rows.append(row)
            else:
                for v in data:
                    if v.get('policy_uid') == run['policy_uid'] and v.get('provider_ownership_verified') is True:
                        vm_observations.append({k: v.get(k) for k in ('instance_id', 'nodeprovision_uid', 'state', 'market', 'observed_at')})
    sources = sorted({r['checkpoint_interval_source'] for r in rows if r['checkpoint_interval_source']})
    return rows, {'run_id': run['run_id'], 'policy_uid': run.get('policy_uid'),
                  'semantics': 'polling observations only; no inferred event time, causal benefit, or missing value imputation',
                  'policy_snapshot_rows': len(rows), 'checkpoint_interval_sources_observed': sources,
                  'missing_fields': {k: sum(r[k] is None for r in rows) for k in FIELDS},
                  'verified_vm_observations': vm_observations, 'source_files': files, 'errors': errors}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--directory', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    a = p.parse_args()
    rows, summary = summarize(a.directory)
    a.out.mkdir(parents=True, exist_ok=True)
    with (a.out/'r2-r3-observations.csv').open('w') as f:
        w = csv.DictWriter(f, fieldnames=FIELDS); w.writeheader(); w.writerows(rows)
    (a.out/'r2-r3-coverage.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2)+'\n')


if __name__ == '__main__': main()
