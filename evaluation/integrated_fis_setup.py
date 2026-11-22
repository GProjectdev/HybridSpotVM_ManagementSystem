"""Render the existing four-rank C role/alarm setup without AWS operations.

Actual application is a separately authorized host operation. This module
neither creates an alarm/policy nor starts FIS, and never broadens to a fleet.
"""
import re
from .ieee_v2_ledger import ID
from .integrated_protocol import PROTOCOL


def build_setup(run, providers, role_arn):
    account=run.get('expected_aws_account','')
    rid=run.get('run_id','')
    uid=run.get('policy_uid')
    ns=run.get('namespace')
    if (run.get('protocol')!=PROTOCOL or run.get('arm')!='C' or run.get('world_size')!=4
        or not re.fullmatch('[0-9]{12}',account) or not re.fullmatch('[a-z][a-z0-9-]{0,15}[a-z0-9]|[a-z]',rid)
        or ns!='eval-v3-'+rid or not uid):
        raise ValueError('exact four-rank C Run with current policy UID required')
    if (run.get('region'),run.get('availability_zone'),run.get('instance_type'))!=('ap-northeast-2','ap-northeast-2c','g5.2xlarge'):
        raise ValueError('frozen C hardware/location required')
    if role_arn!='arn:aws:iam::'+account+':role/HybridSpotFIS-20261117':
        raise ValueError('reviewed same-account FIS role required')
    if len(providers)!=4:
        raise ValueError('complete initial four-worker world required')
    nodes=[]
    for p in providers:
        m=p.get('metadata',{});spec=p.get('spec',{});s=p.get('status',{})
        if (m.get('namespace')!=ns or not m.get('uid') or m.get('deletionTimestamp')
            or m.get('labels',{}).get('training.dcnlab.com/policy-uid')!=uid
            or s.get('phase')!='Ready' or not s.get('nodeName') or not ID.fullmatch(s.get('instanceId',''))
            or spec.get('marketType') not in ('Spot','OnDemand')):
            raise ValueError('current UID-owned Ready provider required')
        nodes.append(p)
    if (len({n['metadata']['uid'] for n in nodes})!=4
        or len({n['status']['instanceId'] for n in nodes})!=4
        or len({n['status']['nodeName'] for n in nodes})!=4):
        raise ValueError('four distinct provider, Node and CR identities required')
    spots=sorted((p for p in nodes if p['spec']['marketType']=='Spot'),key=lambda p:p['metadata']['name'])
    ods=[p for p in nodes if p['spec']['marketType']=='OnDemand']
    if len(spots)!=3 or len(ods)!=1:
        raise ValueError('pre-fault initial 1OD+3Spot required; never retarget after transition')
    spot,od=spots[0],ods[0]
    instance=spot['status']['instanceId'];node_uid=spot['metadata']['uid']
    resource='arn:aws:ec2:ap-northeast-2:'+account+':instance/'+instance
    # Existing dead-instance policy is preserved. A new per-Run inline policy
    # grants only this provider ARN AND the already-existing NP UID/ns tags.
    policy={'Version':'2012-10-17','Statement':[
        {'Effect':'Allow','Action':'ec2:DescribeInstances','Resource':'*'},
        {'Effect':'Allow','Action':'ec2:SendSpotInstanceInterruptions','Resource':resource,
         'Condition':{'StringEquals':{'ec2:ResourceTag/node-provision-uid':node_uid,
                                     'ec2:ResourceTag/node-provision-namespace':ns}}}]}
    alarm_name='HybridSpotFIS-'+rid+'-od-health'
    alarm={'AlarmName':alarm_name,'Namespace':'AWS/EC2','MetricName':'StatusCheckFailed',
           'Dimensions':[{'Name':'InstanceId','Value':od['status']['instanceId']}],
           'Statistic':'Maximum','Period':60,'EvaluationPeriods':1,'DatapointsToAlarm':1,
           'Threshold':1,'ComparisonOperator':'GreaterThanOrEqualToThreshold','TreatMissingData':'breaching'}
    return {'run_id':rid,'policy_uid':uid,'role_arn':role_arn,'role_name':'HybridSpotFIS-20261117',
            'policy_name':'E2-'+rid,'policy_document':policy,'alarm':alarm,
            'alarm_arn':'arn:aws:cloudwatch:ap-northeast-2:'+account+':alarm:'+alarm_name,
            'initial_instance_ids':sorted(n['status']['instanceId'] for n in nodes),
            'fis_target_instance_id':instance,'fis_target_nodeprovision_uid':node_uid,
            'fis_stop_alarm_instance_id':od['status']['instanceId'],
            'fis_stop_alarm_nodeprovision_uid':od['metadata']['uid'],
            'fis_authorization_tag':'v6-readiness-20261117',
            'selection_rule':'lexicographically first initial owned Spot NodeProvision; fixed before half-step gate',
            'mutations_performed':False,'paid_execution_authorized_by_this_function':False}
