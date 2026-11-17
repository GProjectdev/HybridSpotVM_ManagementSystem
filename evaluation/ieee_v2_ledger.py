"""Read-only, exact-instance ledger. Never lists an account's instances."""
import json
import re
import subprocess

ID = re.compile(r'i-[0-9a-f]{8}(?:[0-9a-f]{9})?\Z')


def ownership(nodeprovisions, policy_uid, previous=None):
    if not policy_uid:
        raise ValueError('exact policy UID required')
    owned = dict(previous or {})
    for obj in nodeprovisions:
        m, s = obj['metadata'], obj.get('status', {})
        if m.get('labels', {}).get('training.dcnlab.com/policy-uid') != policy_uid:
            continue
        instance = s.get('instanceId', s.get('instanceID'))
        if not instance:
            continue
        if not m.get('uid') or not ID.fullmatch(instance):
            raise ValueError('invalid provider/NodeProvision identity')
        entry = {'nodeprovision_uid':m['uid'], 'nodeprovision_name':m['name'],
                 'policy_uid':policy_uid, 'market':obj.get('spec', {}).get('marketType'),
                 'provisioning_started_at':s.get('startTime'), 'gpu_ready_at':s.get('completionTime')}
        if instance in owned and any(owned[instance].get(k) != entry[k] for k in ('nodeprovision_uid','policy_uid')):
            raise ValueError('provider ID ownership changed')
        owned[instance] = entry
    if len(owned) > 16:
        raise ValueError('unexpected experiment capacity churn; review required')
    return owned


class AWSReader:
    def __init__(self, profile, account, region, runner=subprocess.run):
        if not profile or not re.fullmatch(r'[0-9]{12}', account) or not region:
            raise ValueError('explicit profile, 12-digit account and region required')
        self.base=['aws','--profile',profile,'--region',region,'--no-cli-pager']
        self.account, self.runner, self.checked = account, runner, False

    def command(self, *args):
        p=self.runner(self.base+list(args)+['--output','json'],text=True,capture_output=True,timeout=30)
        if p.returncode:
            raise RuntimeError('read-only AWS lookup failed')
        return json.loads(p.stdout)

    def observe(self, owned, observed_at):
        if not self.checked:
            if self.command('sts','get-caller-identity')['Account'] != self.account:
                raise ValueError('unexpected AWS account')
            self.checked=True
        if not owned:
            return []
        if len(owned)>16 or any(not ID.fullmatch(i) for i in owned):
            raise ValueError('invalid exact-instance ledger')
        result=self.command('ec2','describe-instances','--instance-ids',*sorted(owned))
        seen=set(); rows=[]
        for reservation in result['Reservations']:
            for i in reservation['Instances']:
                identity=i['InstanceId']
                if identity not in owned or identity in seen:
                    raise ValueError('unexpected or duplicate provider response')
                seen.add(identity)
                rows.append(dict(owned[identity],instance_id=identity,observed_at=observed_at,
                                 state=i['State']['Name'],launch_time=i.get('LaunchTime'),
                                 instance_lifecycle=i.get('InstanceLifecycle','on-demand'),
                                 state_transition_reason=i.get('StateTransitionReason'),
                                 termination_time_semantics='terminated observed at polling time; not exact AWS billing end'))
        # Missing records are not positive termination evidence.
        rows.extend(dict(owned[i],instance_id=i,observed_at=observed_at,state='NOT_OBSERVED') for i in sorted(set(owned)-seen))
        return rows
