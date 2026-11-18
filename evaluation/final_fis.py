"""Final E2 dispatcher: exact live UID/Spot target, fresh step and one intent.

plan performs read-only cloud calls and writes a draft. start requires the same
Run, reviewed role/alarm, live checks again after template creation, and a
30-minute recovery reserve. No IAM, VM, or alarm creation is performed here.
"""
import argparse
from datetime import datetime,timezone
import json,os,re,subprocess,uuid
from pathlib import Path
try:
    from .final_fault_gate import check
    from .ieee_v2 import canonical_hash
    from .ieee_v2_ledger import AWSReader,AWSLookupError,ownership
    from .ieee_v2_observe import log_events
    from .collect import timestamp
except ImportError:
    from final_fault_gate import check
    from ieee_v2 import canonical_hash
    from ieee_v2_ledger import AWSReader,AWSLookupError,ownership
    from ieee_v2_observe import log_events
    from collect import timestamp

LABEL='training.dcnlab.com/'

def validate_run(run,now):
    v3=run.get('protocol')=='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118'
    valid_identity=(run.get('arm')=='C' and run.get('world_size')==4) if v3 else (run.get('protocol')=='ieee-access-final-20261117' and run.get('arm')=='E2-B' and run.get('world_size')==2)
    if not valid_identity or run.get('phase')!='running':
        raise ValueError('running final E2-B with two ranks required')
    if not re.fullmatch(r'[0-9]{12}',run.get('expected_aws_account','')):raise ValueError('explicit 12-digit account required')
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,15}[a-z0-9]|[a-z]',run.get('run_id','')):
        raise ValueError('bounded Run identity required')
    if run.get('namespace')!=('eval-v3-' if v3 else 'eval-final-')+run['run_id'] or not run.get('policy_uid') or not run.get('workload_uid'):
        raise ValueError('exact namespace/policy/workload UIDs required')
    if (run.get('region')!='ap-northeast-2' or run.get('availability_zone')!='ap-northeast-2c'
        or run.get('ready_for_paid_execution') is not True):
        raise ValueError('reviewed location and explicit readiness required')
    if not re.fullmatch(r'[a-f0-9]{64}',run.get('comparison_lock_sha256','') or ''):
        raise ValueError('frozen comparison hash required even for separate acceptance')
    if not re.fullmatch(r'[a-f0-9]{64}',run.get('live_spec_sha256','') or ''):raise ValueError('reviewed live policy/workload spec hash required')
    if not re.fullmatch(r'[A-Za-z0-9._:/=+@-]{1,128}',run.get('fis_authorization_tag','') or ''):raise ValueError('reviewed account FIS authorization tag required')
    if not run.get('started_at') or not run.get('hard_deadline_utc'):
        raise ValueError('absolute start/deadline required')
    if timestamp(now)<timestamp(run['started_at']) or (timestamp(run['hard_deadline_utc'])-timestamp(now)).total_seconds()<1800:
        raise ValueError('30-minute recovery/cleanup reserve required')


def template(run,scope,role,alarm):
    account=run['expected_aws_account'];region=run['region']
    if not re.fullmatch('arn:aws:iam::'+account+r':role/[A-Za-z0-9+=,.@_/-]+',role):raise ValueError('same-account role required')
    if not alarm.startswith('arn:aws:cloudwatch:'+region+':'+account+':alarm:') or not alarm.rsplit(':',1)[-1]:raise ValueError('same-account/region stop alarm required')
    return {'description':'Final E2 '+run['run_id'],'roleArn':role,
            'targets':{'oneSpot':{'resourceType':'aws:ec2:spot-instance','resourceArns':['arn:aws:ec2:'+region+':'+account+':instance/'+scope['instance_id']],'selectionMode':'ALL'}},
            'actions':{'interrupt':{'actionId':'aws:ec2:send-spot-instance-interruptions','parameters':{'durationBeforeInterruption':'PT2M'},'targets':{'SpotInstances':'oneSpot'}}},
            'stopConditions':[{'source':'aws:cloudwatch:alarm','value':alarm}],
            'tags':{'evaluation-run':run['run_id'],'policy-uid':run['policy_uid'],'HybridSpotValidationRun':run['fis_authorization_tag']}}


class Live:
    def __init__(self,run,profile,kubeconfigs,runner=subprocess.run):
        self.run=run;self.cloud=AWSReader(profile,run['expected_aws_account'],run['region'],runner);self.configs=kubeconfigs;self.runner=runner
    def kub(self,cluster,*args):
        p=self.runner(['kubectl','--kubeconfig='+self.configs[cluster],'--request-timeout=15s','-n',self.run['namespace'],*args],text=True,capture_output=True,timeout=25)
        if p.returncode:raise RuntimeError('live Kubernetes identity/progress unavailable')
        return p.stdout
    def snapshot(self,now):
        r=self.run;validate_run(r,now)
        v3=r.get('world_size')==4;world=4 if v3 else 2
        self.cloud.observe({},now)
        policy=json.loads(self.kub('karmada','get','noderequest',r['policy'],'-o','json'))
        workload=json.loads(self.kub('karmada','get','statefulset',r['app'],'-o','json'))
        if (policy['metadata']['uid']!=r['policy_uid'] or policy['metadata'].get('deletionTimestamp')
            or policy['spec']['workloadRef']['uid']!=r['workload_uid'] or workload['metadata']['uid']!=r['workload_uid']
            or workload['metadata'].get('deletionTimestamp') or (policy['spec']['policy'].get('fixedOnDemand') is not None if v3 else policy['spec']['policy'].get('fixedOnDemand')!=1)
            or workload['spec'].get('replicas')!=world or policy['metadata'].get('annotations',{}).get(LABEL+'suspend')=='true'):
            raise ValueError('live policy/workload/composition differs or is suspended')
        if canonical_hash({'policy_spec':policy['spec'],'workload_spec':workload['spec']})!=r['live_spec_sha256']:raise ValueError('frozen live policy/workload specs changed')
        nodes=json.loads(self.kub('aws','get','nodeprovisions','-o','json'))['items']
        if len(nodes)!=world or any(n['metadata'].get('labels',{}).get(LABEL+'policy-uid')!=r['policy_uid'] or n['metadata'].get('deletionTimestamp') or n.get('status',{}).get('phase')!='Ready' for n in nodes):
            raise ValueError('stable owned two-worker world required')
        spots=[n for n in nodes if n.get('spec',{}).get('marketType')=='Spot'];ods=[n for n in nodes if n.get('spec',{}).get('marketType')=='OnDemand']
        if (not spots or not ods) if v3 else (len(spots)!=1 or len(ods)!=1):raise ValueError('owned Spot and healthy OD required')
        if v3:
            spots=[n for n in spots if n.get('status',{}).get('instanceId')==r.get('fis_target_instance_id')]
            ods=[n for n in ods if n.get('status',{}).get('instanceId')==r.get('fis_stop_alarm_instance_id')]
            if len(spots)!=1 or len(ods)!=1:raise ValueError('pinned exact Spot/OD identity changed; never retarget')
        spot=spots[0];owned=ownership(nodes,r['policy_uid']);rows=self.cloud.observe(owned,now)
        if len(rows)!=world or any(x['state']!='running' or not x.get('provider_ownership_verified') for x in rows):raise ValueError('running provider UID ownership required')
        if v3 and (len(r.get('initial_instance_ids',[]))!=world or set(owned)!=set(r['initial_instance_ids'])):
            raise ValueError('pre-fault world changed; record deviation, never retarget')
        iid=spot['status']['instanceId'];odid=ods[0]['status']['instanceId']
        all_ids=[n['status']['instanceId'] for n in nodes] if v3 else [iid,odid]
        raw=self.cloud.command('ec2','describe-instances','--instance-ids',*all_ids)['Reservations']
        xs=[x for reservation in raw for x in reservation['Instances']]
        if {x['InstanceId'] for x in xs}!=set(all_ids) or len(xs)!=world or any(x['State']['Name']!='running' or x['Placement']['AvailabilityZone']!=r['availability_zone'] or x['InstanceType']!=r['instance_type'] for x in xs):
            raise ValueError('exact running worker types/AZ required')
        markets={x['InstanceId']:x.get('InstanceLifecycle','on-demand') for x in xs}
        if markets[iid]!='spot' or markets[odid]!='on-demand':raise ValueError('provider market composition differs')
        if v3 and any(markets[n['status']['instanceId']]!=('spot' if n['spec']['marketType']=='Spot' else 'on-demand') for n in nodes):
            raise ValueError('one or more provider markets differ from owned NP')
        member=json.loads(self.kub('aws','get','statefulset',r['app'],'-o','json'));member_uid=member['metadata']['uid']
        pods=json.loads(self.kub('aws','get','pods','-l','app='+r['app'],'-o','json'))['items'];current={};events=[]
        for pod in pods:
            m=pod['metadata'];name=m['name']
            if m.get('deletionTimestamp') or name not in [r['app']+'-'+str(i) for i in range(world)]:raise ValueError('unexpected or terminating rank Pod')
            rank=int(name.rsplit('-',1)[1]);owners=m.get('ownerReferences',[])
            if m.get('labels',{}).get(LABEL+'workload-uid')!=r['workload_uid'] or not any(o.get('controller') is True and o['uid']==member_uid for o in owners):raise ValueError('rank workload ownership required')
            if rank in current:raise ValueError('duplicate rank')
            if v3:
                cs=[c for c in pod['spec']['containers'] if c['name']=='trainer']
                if len(cs)!=1 or any(str(cs[0].get('resources',{}).get(k,{}).get('nvidia.com/gpu'))!='1' for k in ('requests','limits')):
                    raise ValueError('one actual requested and limited GPU per rank required')
            current[rank]=m['uid'];events.extend(log_events(self.kub('aws','logs',name,'-c','trainer','--since=120s','--tail=500'),r,pod))
        if v3:
            assigned=[p['spec'].get('nodeName') for p in pods]
            if len(assigned)!=4 or len(set(assigned))!=4 or set(assigned)!={n['status']['nodeName'] for n in nodes}:
                raise ValueError('four distinct current owned worker nodes required')
            try:
                from .integrated_protocol import durable_group_checkpoint
            except ImportError:
                from integrated_protocol import durable_group_checkpoint
            cps=json.loads(self.kub('karmada','get','fluidcrmigrations','-o','json'))['items']
            if not any(durable_group_checkpoint(x,r,current) for x in cps):raise ValueError('durable complete current four-rank checkpoint required')
        # Re-evaluate after bounded reads rather than granting freshness at read start.
        observed=datetime.now(timezone.utc).isoformat();validate_run(r,observed)
        gate=check(r,events,now=observed,current_pods=current)
        scope={'run_id':r['run_id'],'policy_uid':r['policy_uid'],'workload_uid':r['workload_uid'],'nodeprovision_uid':spot['metadata']['uid'],'instance_id':iid,'od_instance_id':odid,'comparison_lock_sha256':r['comparison_lock_sha256'],'live_spec_sha256':r['live_spec_sha256'],'policy_generation':policy['metadata'].get('generation'),'workload_generation':workload['metadata'].get('generation'),'member_workload_uid':member_uid,'source_pods':{str(rank):uid for rank,uid in current.items()}}
        return scope,gate
    def alarm_ok(self,arn,scope):
        xs=self.cloud.command('cloudwatch','describe-alarms','--alarm-names',arn.split(':alarm:',1)[1])['MetricAlarms']
        if len(xs)!=1:return False
        a=xs[0]
        return (a['AlarmArn']==arn and a['StateValue']=='OK' and a.get('Namespace')=='AWS/EC2' and a.get('MetricName')=='StatusCheckFailed'
                and a.get('Dimensions')==[{'Name':'InstanceId','Value':scope['od_instance_id']}]
                and a.get('ComparisonOperator')=='GreaterThanOrEqualToThreshold' and a.get('Threshold')==1)


def save(path,value,exclusive=False):
    flags=os.O_WRONLY|os.O_CREAT|(os.O_EXCL if exclusive else os.O_TRUNC)
    fd=os.open(path,flags,0o600)
    with os.fdopen(fd,'w') as f:json.dump(value,f,indent=2);f.flush();os.fsync(f.fileno())


def plan(directory,run,live,role,alarm,now):
    validate_run(run,now)
    if (directory/'fis-start-intent.json').exists():raise ValueError('one FIS attempt per Run; inspect existing intent')
    scope,gate=live.snapshot(now);t=template(run,scope,role,alarm)
    if not live.alarm_ok(alarm,scope):raise ValueError('healthy owned OD stop alarm required')
    receipt={'scope':scope,'template':t,'gate_at_plan':gate,'planned_at':now,'template_sha256':canonical_hash(t)}
    save(directory/'fis-final-plan.json',receipt,exclusive=True)
    return receipt


def start(directory,run,live,confirm,now):
    validate_run(run,now)
    if confirm!=run['run_id']:raise ValueError('exact Run confirmation required')
    if (directory/'fis-start-intent.json').exists():raise ValueError('existing intent: reconcile, never blindly retry')
    p=json.loads((directory/'fis-final-plan.json').read_text());scope,gate=live.snapshot(now)
    expected=template(run,scope,p['template']['roleArn'],p['template']['stopConditions'][0]['value'])
    if p['scope']!=scope or p['template']!=expected or p['template_sha256']!=canonical_hash(expected):raise ValueError('plan or exact target changed')
    alarm=expected['stopConditions'][0]['value']
    if not gate['ready'] or not live.alarm_ok(alarm,scope):raise ValueError('fresh step and healthy OD gates required')
    token=str(uuid.uuid5(uuid.NAMESPACE_URL,run['namespace']+'/'+run['policy_uid']))
    intent={'state':'creating_template','scope':scope,'client_token':token,'gate':gate,'requested_at':now}
    save(directory/'fis-start-intent.json',intent,exclusive=True)
    try:
        created=live.cloud.command('fis','create-experiment-template','--cli-input-json',json.dumps(expected))
    except AWSLookupError as error:
        intent.update(state='template_create_failed_or_unknown',error_code=error.code,
                      denied_action=error.denied_action,denied_resource=error.denied_resource);save(directory/'fis-start-intent.json',intent)
        raise RuntimeError('FIS template creation failed: '+error.code+'; reconcile existing intent before any new Run') from None
    save(directory/'fis-created.json',created,exclusive=True)
    # Template creation can be slow: repeat current UID/provider/progress/deadline
    # checks before irreversible dispatch, with no cached authorization.
    current=datetime.now(timezone.utc).isoformat();validate_run(run,current)
    next_scope,next_gate=live.snapshot(current)
    if next_scope!=scope or not next_gate['ready'] or not live.alarm_ok(alarm,scope):raise ValueError('target/progress/health changed during template creation; not started')
    dispatch_at=datetime.now(timezone.utc).isoformat();validate_run(run,dispatch_at)
    if not next_gate.get('progress_valid_until_utc') or timestamp(dispatch_at)>timestamp(next_gate['progress_valid_until_utc']):raise ValueError('progress expired during final health check; not started')
    intent.update(state='start_requested',template_id=created['experimentTemplate']['id'],gate=next_gate)
    save(directory/'fis-start-intent.json',intent)
    try:
        result=live.cloud.command('fis','start-experiment','--experiment-template-id',intent['template_id'],'--client-token',token,'--tags',json.dumps(expected['tags']))
    except AWSLookupError as error:
        intent.update(state='start_response_failed_or_unknown',error_code=error.code,
                      denied_action=error.denied_action,denied_resource=error.denied_resource);save(directory/'fis-start-intent.json',intent)
        raise RuntimeError('FIS start response failed: '+error.code+'; do not retry') from None
    save(directory/'fis-started.json',result,exclusive=True)
    return result


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('command',choices=['plan','start']);p.add_argument('--directory',type=Path,required=True);p.add_argument('--profile',required=True);p.add_argument('--karmada-kubeconfig',required=True);p.add_argument('--member-kubeconfig',required=True);p.add_argument('--role-arn');p.add_argument('--alarm-arn');p.add_argument('--confirm')
    a=p.parse_args();run=json.loads((a.directory/'run.json').read_text());live=Live(run,a.profile,{'karmada':a.karmada_kubeconfig,'aws':a.member_kubeconfig});now=datetime.now(timezone.utc).isoformat()
    if a.command=='plan':
        if not a.role_arn or not a.alarm_arn:p.error('plan requires reviewed role/alarm ARNs')
        result=plan(a.directory,run,live,a.role_arn,a.alarm_arn,now);print(json.dumps({'plan_only':True,'scope':result['scope'],'step_ready':result['gate_at_plan']['ready']}))
    else:
        result=start(a.directory,run,live,a.confirm,now);print(json.dumps({'fis_experiment_id':result['experiment']['id'],'one_exact_spot':True}))

if __name__=='__main__':main()
