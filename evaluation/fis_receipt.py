"""Read-only, exact-experiment FIS receipt collection for E2; never starts faults."""
import argparse
from datetime import datetime,timezone
import json
from pathlib import Path
import re
try:
    from .ieee_v2_ledger import AWSReader,ID
except ImportError:
    from ieee_v2_ledger import AWSReader,ID


def collect(reader, run, experiment_id, instance_id, observed_at, ownership):
    if run.get('protocol')!='ieee-access-final-20261117' or run.get('arm')!='E2-B':
        raise ValueError('final E2-B run required')
    if not re.fullmatch('[A-Za-z0-9_-]{1,64}',experiment_id) or not ID.fullmatch(instance_id):
        raise ValueError('explicit experiment and instance identities required')
    if run.get('expected_aws_account')!=reader.account or run.get('region')!='ap-northeast-2':
        raise ValueError('run/account/region mismatch')
    owned=ownership.get(instance_id,{})
    if not run.get("policy_uid") or owned.get("policy_uid")!=run["policy_uid"] or not owned.get("nodeprovision_uid") or owned.get("market")!="Spot":
        raise ValueError("UID-bound owned Spot ledger required")
    reader.observe({},observed_at)
    exp=reader.command('fis','get-experiment','--id',experiment_id)['experiment']
    if exp.get('id')!=experiment_id or exp.get('tags',{}).get('evaluation-run')!=run['run_id']:
        raise ValueError('foreign FIS experiment')
    arn='arn:aws:ec2:'+run['region']+':'+reader.account+':instance/'+instance_id
    targets=exp.get('targets',{})
    if len(targets)!=1:raise ValueError('exact single target required')
    target=next(iter(targets.values()))
    if (target.get('resourceArns')!=[arn] or target.get('resourceType')!='aws:ec2:spot-instance'
        or target.get('selectionMode')!='ALL' or target.get('resourceTags') or target.get('filters')):
        raise ValueError('FIS target differs from exact owned Spot')
    actions=exp.get('actions',{})
    if len(actions)!=1 or next(iter(actions.values())).get('actionId')!='aws:ec2:send-spot-instance-interruptions':
        raise ValueError('unexpected fault action')
    return {'run_id':run['run_id'],'observed_at':observed_at,'instance_id':instance_id,
            'experiment_id':experiment_id,'experiment':exp,
            'semantics':'FIS action times are not actual EC2 termination or application recovery times; join exact instance/CR UID and rank events'}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ('profile','account','experiment-id','instance-id'):p.add_argument('--'+name,required=True)
    p.add_argument('--ownership',type=Path,required=True);p.add_argument('--run',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args()
    if a.out.exists():raise ValueError('preserve prior receipt; choose fresh timestamped output')
    reader=AWSReader(a.profile,a.account,'ap-northeast-2')
    result=collect(reader,json.loads(a.run.read_text()),a.experiment_id,a.instance_id,datetime.now(timezone.utc).isoformat(),json.loads(a.ownership.read_text()))
    import os
    os.umask(0o077)
    with a.out.open('x') as f:json.dump(result,f,indent=2)
    print('Read-only FIS receipt saved; no fault sent.')

if __name__=='__main__':main()
