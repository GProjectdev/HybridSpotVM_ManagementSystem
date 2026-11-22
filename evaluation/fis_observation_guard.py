"""Pure identity check for an unavailable log on the one approved FIS target.

This never starts/stops FIS or changes a run. A healthy VM, another rank, an
unverified provider, or an unbound fault receipt cannot relax log monitoring.
"""
from datetime import datetime
import re


def at(value):
    t = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if t.tzinfo is None:
        raise ValueError('timezone required')
    return t


def expected_lost_pod_uids(run, auth, plan, started, experiment, pods, nps, owned, rows, now):
    scope = plan.get('scope', {})
    if (run.get('protocol') != 'HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118'
        or run.get('purpose') != 'formal' or run.get('arm') != 'C'
        or run.get('world_size') != 4 or run.get('scheduled_fis') is not True
        or not re.fullmatch('[0-9a-f]{64}', run.get('frozen_manifest_sha256', ''))
        or run.get('paid_authorization') is not True or auth.get('authorized') is not True
        or auth.get('approved_scope', {}).get('fis') is not True
        or any(auth.get(k) != run.get(v) for k, v in [
            ('run_id', 'run_id'), ('frozen_manifest_sha256', 'frozen_manifest_sha256'),
            ('hard_deadline_utc', 'hard_deadline_utc'), ('budget_usd', 'approved_budget_usd')])
        or not auth.get('researcher_message_reference')
        or not at(run['started_at']) <= now < at(run['cleanup_start_utc'])):
        raise ValueError('approved exact C fault window required')
    if any(not run.get(k) or scope.get(k) != run[k] for k in
           ('run_id', 'policy_uid', 'workload_uid', 'comparison_lock_sha256', 'live_spec_sha256')):
        raise ValueError('fault scope identity differs')
    target = run.get('fis_target_instance_id')
    if not target or scope.get('instance_id') != target:
        raise ValueError('pinned fault target differs')
    source = scope.get('source_pods', {})
    if (set(source) != {'0', '1', '2', '3'} or len(set(source.values())) != 4
        or not all(source.values())):
        raise ValueError('exact initial four-rank Pod identities required')
    if (not started.get('experiment', {}).get('id')
        or experiment.get('id') != started['experiment']['id']
        or experiment.get('tags', {}).get('evaluation-run') != run['run_id']
        or experiment.get('tags', {}).get('policy-uid') != run['policy_uid']
        or not at(run['started_at']) <= at(experiment['startTime']) <= now):
        raise ValueError('fresh experiment receipt differs')
    expected = f"arn:aws:ec2:{run['region']}:{run['expected_aws_account']}:instance/{target}"
    targets = experiment.get('targets', {})
    actions = experiment.get('actions', {})
    if (len(targets) != 1 or next(iter(targets.values())).get('resourceArns') != [expected]
        or next(iter(targets.values())).get('resourceType') != 'aws:ec2:spot-instance'
        or len(actions) != 1
        or next(iter(actions.values())).get('actionId') != 'aws:ec2:send-spot-instance-interruptions'
        or next(iter(actions.values())).get('parameters', {}).get('durationBeforeInterruption') != 'PT2M'
        or next(iter(actions.values())).get('state', {}).get('status') != 'completed'
        or experiment.get('state', {}).get('status') != 'completed'):
        raise ValueError('one completed exact-target interruption action required')
    owner = owned.get(target, {})
    selected = [r for r in rows if r.get('instance_id') == target]
    if (len(selected) != 1 or not scope.get('nodeprovision_uid')
        or owner.get('nodeprovision_uid') != scope['nodeprovision_uid']
        or owner.get('policy_uid') != run['policy_uid']
        or owner.get('nodeprovision_namespace') != run['namespace']
        or owner.get('market') != 'Spot'):
        raise ValueError('exact owned Spot provider required')
    row = selected[0]
    if (row.get('nodeprovision_uid') != scope['nodeprovision_uid']
        or row.get('policy_uid') != run['policy_uid']
        or row.get('nodeprovision_namespace') != run['namespace']
        or not row.get('provider_ownership_verified')
        or row.get('instance_lifecycle') != 'spot'
        or row.get('state') not in {'shutting-down', 'terminated'}
        or not 0 <= (now-at(row['observed_at'])).total_seconds() <= 90):
        raise ValueError('fresh positive target loss required; running is not expected loss')
    matched = [n for n in nps if n['metadata'].get('uid') == scope['nodeprovision_uid']
               and n['metadata'].get('labels', {}).get('training.dcnlab.com/policy-uid') == run['policy_uid']
               and n.get('status', {}).get('instanceId') == target]
    if len(matched) != 1 or not matched[0].get('status', {}).get('nodeName'):
        raise ValueError('snapshot target NodeProvision identity required')
    node = matched[0]['status']['nodeName']
    allowed = {p['metadata']['uid'] for p in pods
               if p['metadata'].get('namespace') == run['namespace']
               and p.get('spec', {}).get('nodeName') == node
               and any(p['metadata'].get('name') == run['app']+'-'+rank
                       and p['metadata']['uid'] == uid for rank, uid in source.items())}
    if len(allowed) != 1:
        raise ValueError('one original target Pod UID required; recreated Pods stay strict')
    return allowed
