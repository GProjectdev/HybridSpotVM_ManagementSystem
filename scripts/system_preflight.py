"""Read-only system/FIS prerequisites; no VM, IAM, experiment or cluster mutation.

Simulation is a prerequisite check, not proof of live GPU recovery or of every
AWS authorization layer. This tool never reads Secrets, logs, or Pod env values.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess

REQUIRED = {
    'mgmt': [('hybridspot-system', n) for n in (
        'public-cloud-vm-collector', 'spot-vm-management-controller',
        'checkpoint-recovery-coordinator', 'training-lifecycle-controller', 'placement-webhook')],
    'aws': [('remote-cluster-provisioner-system', 'remote-cluster-provisioner'),
            ('stateful-migration-system', 'stateful-checkpoint'),
            ('stateful-migration-system', 'stateful-member'),
            ('nfs-provisioner', 'nfs-client-provisioner')],
}
DAEMONSETS = {'aws': [('hybridspot-system', 'spot-watcher')]}


class ReadOnly:
    def __init__(self, runner=subprocess.run):
        self.runner = runner

    def json(self, args):
        p = self.runner(args, capture_output=True, text=True, timeout=30)
        if p.returncode:
            match = re.search(r'An error occurred \(([A-Za-z0-9_.-]+)\)', p.stderr or '')
            raise RuntimeError(match.group(1) if match else 'QUERY_UNAVAILABLE')
        return json.loads(p.stdout)

    def kub(self, kubeconfig, *args):
        return self.json(['kubectl', '--kubeconfig='+kubeconfig, '--request-timeout=15s', *args])


def inspect_clusters(reader, configs):
    checks = []
    for cluster, config in configs.items():
        try:
            reader.kub(config, 'get', '--raw=/version')
            checks.append({'check': cluster+'/api', 'passed': True})
            kind = 'clusters' if cluster == 'karmada' else 'nodes'
            items = reader.kub(config, 'get', kind, '-o', 'json')['items']
            checks.append({'check': cluster+'/'+kind, 'passed': bool(items) and all(
                any(c.get('type') == 'Ready' and c.get('status') == 'True'
                    for c in x.get('status', {}).get('conditions', [])) for x in items)})
            for namespace, name in REQUIRED.get(cluster, []):
                x = reader.kub(config, '-n', namespace, 'get', 'deployment', name, '-o', 'json')
                desired = x.get('spec', {}).get('replicas', 1)
                status = x.get('status', {})
                checks.append({'check': cluster+'/'+namespace+'/'+name, 'passed': desired > 0
                    and status.get('observedGeneration', 0) >= x['metadata'].get('generation', 1)
                    and status.get('updatedReplicas', 0) == desired
                    and status.get('availableReplicas', 0) == desired})
            for namespace, name in DAEMONSETS.get(cluster, []):
                x = reader.kub(config, '-n', namespace, 'get', 'daemonset', name, '-o', 'json')
                status = x.get('status', {}); desired = status.get('desiredNumberScheduled')
                checks.append({'check': cluster+'/'+namespace+'/'+name,
                               'passed': desired is not None
                               and status.get('observedGeneration', 0) >= x['metadata'].get('generation', 1)
                               and status.get('updatedNumberScheduled', 0) == desired
                               and status.get('numberReady', 0) == desired,
                               'desired_node_pods': desired})
        except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired) as e:
            checks.append({'check': cluster+'/query', 'passed': False,
                           'error_code': str(e) if isinstance(e, RuntimeError) else 'INVALID_OR_UNAVAILABLE_RESPONSE'})
    return checks


def inspect_fis(reader, profile, account, region, authorization_tag):
    if (not profile or not re.fullmatch(r'\d{12}', account)
        or not re.fullmatch(r'[a-z]{2}(?:-[a-z0-9]+)+-\d', region)
        or not re.fullmatch(r'[A-Za-z0-9._:/=+@-]{1,128}', authorization_tag)):
        raise ValueError('explicit profile/account/region/authorization tag required')
    base = ['aws', '--profile', profile, '--region', region, '--no-cli-pager']
    def aws(*args): return reader.json([*base, *args, '--output', 'json'])
    identity = aws('sts', 'get-caller-identity')
    if identity.get('Account') != account:
        raise ValueError('AWS account mismatch; no further AWS calls made')
    source = identity.get('Arn', '')
    if not source.startswith('arn:aws:iam::'+account+':user/'):
        raise ValueError('IAM user principal required for this simulation; role permission verification remains manual')
    checks = []
    for resource, actions, tag_kind in [('*', ['fis:CreateExperimentTemplate', 'fis:TagResource'], 'RequestTag'),
          ('experiment-template/*', ['fis:GetExperimentTemplate', 'fis:StartExperiment', 'fis:DeleteExperimentTemplate'], 'ResourceTag'),
          ('experiment/*', ['fis:StartExperiment'], 'RequestTag'),
          ('experiment/*', ['fis:GetExperiment', 'fis:StopExperiment'], 'ResourceTag')]:
        # A newly created experiment has request tags, not pre-existing resource
        # tags. Get/Stop has no tagging request. Do not simulate both contexts.
        contexts=json.dumps([{'ContextKeyName':'aws:'+tag_kind+'/HybridSpotValidationRun',
                              'ContextKeyValues':[authorization_tag], 'ContextKeyType':'string'}])
        arn = '*' if resource == '*' else 'arn:aws:fis:'+region+':'+account+':'+resource
        rows = aws('iam', 'simulate-principal-policy', '--policy-source-arn', source,
                   '--action-names', *actions, '--resource-arns', arn,
                   '--context-entries', contexts).get('EvaluationResults', [])
        by_action = {row['EvalActionName']: row['EvalDecision'] for row in rows}
        for action in actions:
            checks.append({'check': action, 'passed': by_action.get(action) == 'allowed',
                           'resource': arn,
                           'simulation_decision': by_action.get(action, 'UNVERIFIED')})
    return checks


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for cluster, env in [('mgmt', 'MGMT'), ('karmada', 'KARMADA'), ('aws', 'AWS'), ('onpre1', 'ONPRE1')]:
        p.add_argument('--'+cluster+'-kubeconfig', default=os.environ.get(env+'_KUBECONFIG'), required=False)
    p.add_argument('--fis-profile'); p.add_argument('--account'); p.add_argument('--region', default='ap-northeast-2')
    p.add_argument('--fis-authorization-tag'); p.add_argument('--out', type=Path, required=True)
    a = p.parse_args()
    configs = {c: getattr(a, c+'_kubeconfig') for c in ('mgmt', 'karmada', 'aws', 'onpre1')}
    if not all(configs.values()): p.error('all four kubeconfigs are required')
    reader = ReadOnly(); checks = inspect_clusters(reader, configs)
    if a.fis_profile:
        try: checks += inspect_fis(reader, a.fis_profile, a.account or '', a.region, a.fis_authorization_tag or '')
        except (RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired):
            checks.append({'check': 'fis/permission-query', 'passed': False, 'error_code': 'UNVERIFIED'})
    result = {'observed_at': datetime.now(timezone.utc).isoformat(), 'read_only': True,
              'passed': all(c['passed'] for c in checks), 'checks': checks,
              'limits': 'Prerequisites only; no live GPU, C/R, role PassRole/trust/resource-policy, FIS start or billing proof.'}
    os.umask(0o077)
    # Preserve previous receipts instead of silently replacing an earlier check.
    with a.out.open('x') as f: f.write(json.dumps(result, indent=2)+'\n')
    print(json.dumps({'passed': result['passed'], 'checks': len(checks), 'receipt': str(a.out)}))
    return 0 if result['passed'] else 1


if __name__ == '__main__': raise SystemExit(main())
