"""Four-rank A/B/C execution contracts; existing controllers remain unchanged."""
import argparse
import copy
import json
import math
from pathlib import Path
import re
try:
    from . import run as legacy
    from .ieee_v2 import canonical_hash, IMAGE, SHA
    from .collect import timestamp
except ImportError:
    import run as legacy
    from ieee_v2 import canonical_hash, IMAGE, SHA
    from collect import timestamp

PROTOCOL = 'HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118'
PREFIX = 'eval-v3-'


def render(config, source, run_id, condition):
    c=copy.deepcopy(config)
    if condition not in {'A','B','C'} or c.get('world_size')!=4:
        raise ValueError('v3 A/B/C world_size=4 required')
    if c.get('purpose') not in {'acceptance','formal'}:
        raise ValueError('explicit acceptance/formal purpose required')
    manual=c.get('acceptance_manual_checkpoint',False)
    if manual and c['purpose']!='acceptance':raise ValueError('manual checkpoint diagnostic cannot be a formal condition')
    if (c.get('cluster'),c.get('region'),c.get('availability_zone'))!=('aws1','ap-northeast-2','ap-northeast-2c'):
        raise ValueError('reviewed member/region/AZ required')
    if not IMAGE.fullmatch(c.get('training_image','')) or not SHA.fullmatch(c.get('asset_manifest_sha256','')):
        raise ValueError('pinned training image and asset hash required')
    sel=c.get('hardware_selection',{})
    if (sel.get('instance_type')!=c.get('instance_type') or sel.get('gpus_per_worker')!=1
        or sel.get('architecture')!='x86_64' or sel.get('status') not in {'PROVISIONAL','SELECTED'}):
        raise ValueError('reviewed one-GPU hardware selection required')
    if c.get('planned_partial_mode')!='disabled':raise ValueError('existing full-group route required')
    batch=c.get('group_replacement_batch_size',1)
    if type(batch) is not int or not 1<=batch<=4:
        raise ValueError('group_replacement_batch_size must be an integer from 1 to 4')
    if not 0<c.get('max_seconds',0)<=18000:raise ValueError('bounded execution <=5h required')
    model=c.get('model');expected={'gpt2':(2,512),'resnet50':(16,None)}
    if model not in expected or (c.get('batch_size'),c.get('sequence_length'))!=expected[model]:
        raise ValueError('reviewed four-rank model/global-batch contract required')
    lock_fields=('model','goal_steps','batch_size','sequence_length','seed','learning_rate','training_image','asset_manifest_sha256')
    if model=='resnet50':lock_fields+=('momentum','weight_decay')
    if any(c.get('training_lock',{}).get(k)!=c.get(k) for k in lock_fields):
        raise ValueError('explicit training lock mismatch')
    cp=c['existing_checkpoint']
    if cp.get('paperProfile',{}).get('enabled') or not cp.get('resume'):
        raise ValueError('resume required; unverified async profile is not enabled')
    if c['purpose']=='formal':
        if c.get('protocol_lock_status')!='FROZEN' or sel['status']!='SELECTED':raise ValueError('formal run requires frozen settings and selected GPU')
        if condition!='A' and cp['minIntervalSeconds']>=cp['maxIntervalSeconds']:raise ValueError('formal B/C must use adaptive bounds')
        risk=c['existing_risk_spec']
        if not risk.get('paperEstimator',{}).get('feedEndpoint') or 'staticLambdaPerHour' in risk:
            raise ValueError('formal run requires frozen SARIMA replay feed')
        if not SHA.fullmatch(c.get('risk_lock_sha256','')):raise ValueError('formal risk/clock lock required')
    ns=PREFIX+run_id;c.update(namespace=ns,scenario='constant',restore_audit=True)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,15}[a-z0-9]|[a-z]',run_id):
        raise ValueError('bounded run identity required for all control stages')
    info,storage,objects,policy=legacy.render(c,source,run_id,'checkpoint','D',namespace=ns)
    policy['kind']='NodeRequest';policy['spec']['workloadRef'].pop('uid',None)
    policy['spec']['policy']=copy.deepcopy(c['existing_policy'])
    policy['spec']['policy'].pop('fixedOnDemand',None)
    if condition=='A':policy['spec']['policy']['fixedOnDemand']=4
    policy['spec']['checkpoint']=copy.deepcopy(cp)
    policy['spec']['replacement']=copy.deepcopy(c['existing_replacement'])
    policy['spec']['capacity'].pop('instanceType',None)
    policy['spec']['capacity']['aws']['credentialsRef']['namespace']=ns
    policy['metadata']['annotations'].update({'training.dcnlab.com/suspend':'true','training.dcnlab.com/planned-partial':'disabled','training.dcnlab.com/group-replacement-batch-size':str(batch)})
    if condition=='A' or manual:policy['metadata']['annotations']['training.dcnlab.com/periodic-checkpoint']='disabled'
    risk=next(x for x in objects if x['kind']=='SpotRiskProfile');risk['spec']=copy.deepcopy(c['existing_risk_spec'])
    if any(risk['spec'].get(k)!=c[v] for k,v in [('region','region'),('availabilityZone','availability_zone'),('instanceType','instance_type')]):
        raise ValueError('risk execution product mismatch')
    sts=next(x for x in objects if x['kind']=='StatefulSet')
    sts['spec']['template']['spec']['nodeSelector']={'ml.dcn.ssu.ac.kr/provider':'AWS','ml.dcn.ssu.ac.kr/node-provision-namespace':ns}
    env=sts['spec']['template']['spec']['containers'][0]['env']
    controls={'EVAL_COMPLETION_AUDIT':1,'EVAL_LIFECYCLE_EVENTS':1,'NCCL_IB_DISABLE':1,'FLUIDCR_CHECKPOINT_WATCHDOG_TIMEOUT':240,'CTRL_CHECKPOINT_TIMEOUT':300,'FLUIDCR_DISABLE_ENUMERATE_PATCH':1,'FLUIDCR_DISABLE_ITER_PATCH':1,'FLUIDCR_DISABLE_EPOCH_RANGE_PATCH':1}
    if model=='resnet50':controls.update(EVAL_MOMENTUM=c['momentum'],EVAL_WEIGHT_DECAY=c['weight_decay'])
    env[:]=[e for e in env if e['name'] not in controls];env.extend({'name':k,'value':str(v)} for k,v in controls.items())
    workload=canonical_hash({k:c.get(k) for k in lock_fields+('world_size','instance_type','nfs_server','nfs_model_path','nfs_dataset_path')})
    comparison=canonical_hash({'workload':workload,'training_code':info['training_code_sha256'],'policy':policy['spec']['policy'],'checkpoint':cp,'replacement':policy['spec']['replacement'],'risk':risk['spec'],'risk_lock':c.get('risk_lock_sha256'),'route':'full-group','group_replacement_batch_size':batch,'system':c['system_lock'],'account':c['expected_aws_account'],'ami':policy['spec']['capacity']['aws'].get('ami'),'storage_class':c['storage_class'],'checkpoint_size':c['checkpoint_size'],'periodic':condition!='A' and not manual})
    info.update(protocol=PROTOCOL,arm=condition,condition=condition,experiment='E1/E2' if condition!='C' else 'E2',
                world_size=4,group_replacement_batch_size=batch,max_expected_worker_instances=4 if condition=='A' else 4+batch,namespace=ns,phase='rendered-suspended',ready_for_paid_execution=False,
                formal_E1_E2_result=False,purpose=c['purpose'],manual_checkpoint_diagnostic=manual,completion_audit_required=True,
                workload_lock_sha256=workload,comparison_lock_sha256=comparison,
                tokens_per_committed_step=4*c['batch_size']*c['sequence_length'] if model=='gpt2' else None,
                samples_per_committed_step=4*c['batch_size'] if model=='resnet50' else None,
                checkpoint_lock_sha256=canonical_hash(cp),scheduled_fis=condition=='C',
                fault_trigger={'type':'committed_global_step','value':math.ceil(c['goal_steps']/2),'status':'PREDECLARED'} if condition=='C' else None)
    info['render_sha256']=canonical_hash({'member':storage,'objects':objects,'policy':policy})
    return info,storage,objects,policy


def durable_group_checkpoint(obj,run,current_pods=None):
    m=obj.get('metadata',{});spec=obj.get('spec',{})
    if (m.get('namespace')!=run['namespace'] or not m.get('uid') or m.get('deletionTimestamp')
        or m.get('labels',{}).get('training.dcnlab.com/policy-uid')!=run['policy_uid']
        or spec.get('workloadRef',{}).get('uid')!=run['workload_uid'] or spec.get('partialCheckpoint')):return False
    cid=m.get('annotations',{}).get('training.dcnlab.com/checkpoint-id')
    reports=[x for x in obj.get('status',{}).get('clusters',[]) if x.get('clusterName')==run['cluster']]
    if not cid or len(reports)!=1:return False
    s=reports[0];pods=s.get('pods',[]);world=run['world_size']
    if s.get('phase')!='Completed' or s.get('observedGeneration')!=m.get('generation') or len(pods)!=world:return False
    if ({p.get('rank') for p in pods}!=set(range(world)) or len({p.get('podUID') for p in pods})!=world
        or len({p.get('nodeName') for p in pods})!=world):return False
    if current_pods is not None and {p['rank']:p.get('podUID') for p in pods}!=current_pods:return False
    for p in pods:
        if not p.get('podUID') or not p.get('nodeName') or p.get('phase') not in {'Resumed','ContainerCheckpointed'} or p.get('checkpointID')!=cid:return False
        files=p.get('checkpointFiles',[])
        if not files:return False
        for a in files:
            digest=a.get('sha256','')
            if (not SHA.fullmatch(digest) or a.get('durableRef')!='file-store:'+run['namespace']+'/sha256/'+digest
                or a.get('checkpointID')!=cid or a.get('containerName')!='trainer' or not a.get('exportedAt')):return False
            try:timestamp(a['exportedAt'])
            except (TypeError,ValueError):return False
    return True


def metrics(info, events, **kwargs):
    try:
        from .ieee_v2 import metrics as existing
    except ImportError:
        from ieee_v2 import metrics as existing
    if info.get('protocol')!=PROTOCOL or info.get('world_size')!=4 or info.get('arm') not in {'A','B','C'}:
        raise ValueError('explicit four-rank A/B/C metrics contract required')
    samples=info['model']=='resnet50'
    adapted=dict(info,tokens_per_committed_step=info['samples_per_committed_step'] if samples else info['tokens_per_committed_step'])
    result=existing(adapted,events,**kwargs)
    result['useful_unit']='samples' if samples else 'tokens'
    result['committed_useful_units']=result['useful_tokens']
    if samples:
        result['useful_tokens']=None
        result['UsefulThroughput_samples_per_second']=result['UsefulThroughput_tokens_per_second']
        result['UsefulThroughput_tokens_per_second']=None
    result['primary_throughput_denominator']='T_E2E_seconds'
    result['formal_E1_E2_result']=False # requires external protocol/deviation review
    return result


def main():
    p=argparse.ArgumentParser(description=__doc__);sub=p.add_subparsers(dest='command',required=True)
    q=sub.add_parser('prepare');q.add_argument('--config',type=Path,required=True);q.add_argument('--policy-source',type=Path,required=True);q.add_argument('--run-id',required=True);q.add_argument('--condition',choices=['A','B','C'],required=True);q.add_argument('--out',type=Path,required=True)
    q=sub.add_parser('metrics');q.add_argument('--run',type=Path,required=True);q.add_argument('--events',type=Path,required=True);q.add_argument('--request-at',required=True);q.add_argument('--billing',type=Path);q.add_argument('--checkpoint-stalls',type=Path);q.add_argument('--out',type=Path,required=True)
    a=p.parse_args()
    if a.out.exists():p.error('preserve existing artifacts; choose a new output')
    read=lambda path:json.loads(path.read_text())
    if a.command=='prepare':
        result=render(read(a.config),read(a.policy_source),a.run_id,a.condition)
        a.out.mkdir(mode=0o700,parents=True,exist_ok=False)
        for name,value in zip(('run.json','member-resources.json','karmada-resources.json','policy.json'),result):legacy.save(a.out/name,value)
        print('Rendered suspended manifests only; no API calls or VM/FIS operations.')
    else:
        bill=read(a.billing) if a.billing else {}
        result=metrics(read(a.run),[json.loads(line) for line in a.events.read_text().splitlines() if line.strip()],request_at=a.request_at,billing=bill.get('segments'),owned_instances=bill.get('instances'),checkpoint_stalls=read(a.checkpoint_stalls) if a.checkpoint_stalls else None)
        legacy.save(a.out,result);print(json.dumps(result))

if __name__=='__main__':main()
