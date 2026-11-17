"""Final 2026-11-17 E1/E2 preparation and offline metrics; no cloud mutations.

E2 compares identical Hybrid settings with/without one fault. Historical replay
and Static/Proposed ablations are not required. E3 is gated on real 4-rank proof.
"""
import argparse
import copy
import csv
import json
import math
from pathlib import Path
import re
try:
    from . import run as legacy
    from .ieee_v2 import canonical_hash, IMAGE, SHA, metrics as previous_metrics
    from .collect import timestamp
except ImportError:
    import run as legacy
    from ieee_v2 import canonical_hash, IMAGE, SHA, metrics as previous_metrics
    from collect import timestamp

PROTOCOL='ieee-access-final-20261117'
ARMS={'E1-A','E1-B','E2-A','E2-B'}
SCHEMA={
 'training_rank0.csv':['model','useful_unit','samples','run_id','utc','rank','global_step','tokens','iteration_time','loss','valid_step','pod_uid','attempt_id'],
 'training_rank1.csv':['model','useful_unit','samples','run_id','utc','rank','global_step','tokens','iteration_time','loss','valid_step','pod_uid','attempt_id'],
 'vm_events.csv':['run_id','utc','instance_id','nodeprovision_uid','event','market','launch_time','observation_semantics'],
 'risk_policy.csv':['run_id','utc','policy_uid','risk_observed_at','lambda_per_hour','decision','checkpoint_interval_seconds','input_source'],
 'checkpoint_events.csv':['run_id','utc','checkpoint_uid','checkpoint_id','checkpoint_step','event','bytes','stall_seconds','periodic_or_emergency'],
 'interruption_events.csv':['run_id','utc','fis_experiment_id','instance_id','event','notice_time','actual_interruption_time'],
 'restore_events.csv':['run_id','utc','request_uid','checkpoint_id','rank','event','first_valid_step'],
 'price_snapshot.csv':['run_id','utc','instance_type','availability_zone','market','price_per_hour','currency','source'],
 'results.csv':['model','useful_unit','useful_samples','useful_samples_per_second','cost_per_useful_sample_usd','run_id','arm','quality','gpu_vm_compute_cost_usd','T_startup_seconds','T_E2E_seconds','T_training_seconds','useful_tokens','useful_tokens_per_second','cost_per_useful_token_usd','checkpoint_stall_seconds','fault_resume_gap_seconds','rollback_steps','fault_window_nonproductive_vm_seconds','evidence'],
}


def render(config, source, run_id, arm):
    if arm not in ARMS:
        raise ValueError('only E1-A/E1-B/E2-A/E2-B; no verified 4-rank E3 execution')
    c=copy.deepcopy(config)
    ns='eval-final-'+run_id
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,15}[a-z0-9]|[a-z]',run_id) or c.get('namespace',ns)!=ns:
        raise ValueError('fresh derived namespace required')
    if c.get('cluster')!='aws1' or c.get('region')!='ap-northeast-2' or c.get('availability_zone')!='ap-northeast-2c':
        raise ValueError('final plan execution location mismatch')
    if not re.fullmatch(r'[a-z][a-z0-9]*\.[a-z0-9]+',c.get('instance_type','')):
        raise ValueError('explicit candidate/selected instance type required')
    selection=c.get('hardware_selection',{})
    if selection.get('instance_type')!=c['instance_type'] or selection.get('gpus_per_worker')!=1 or selection.get('architecture')!='x86_64':
        raise ValueError('reviewed 1-GPU x86_64 hardware manifest required')
    if selection.get('status') not in {'PROVISIONAL','SELECTED'}:
        raise ValueError('hardware selection status missing')
    model=c.get('model')
    if model not in {'gpt2','resnet50'}:
        raise ValueError('explicit gpt2 or resnet50 model required')
    lock=c.get('training_lock',{})
    if lock.get('model') != model:
        raise ValueError('model must match explicit training lock')
    fields=('model','goal_steps','batch_size','sequence_length','seed','learning_rate','training_image','asset_manifest_sha256')
    if model=='resnet50':
        fields+=('momentum','weight_decay')
        if c.get('sequence_length') is not None or not c.get('dataset',{}).get('name')=='cifar10':
            raise ValueError('ResNet-50 requires CIFAR-10 and no token sequence length')
        if c.get('target_provenance',{}).get('model')!='resnet50':
            raise ValueError('ResNet-50 target must not reuse GPT-2 pilot')
    if any(lock.get(k)!=c.get(k) for k in fields):
        raise ValueError('training fields differ from explicit run lock')
    if not IMAGE.fullmatch(c.get('training_image','')) or not SHA.fullmatch(c.get('asset_manifest_sha256','')):
        raise ValueError('pinned image and asset hash required')
    if type(c.get('max_seconds')) is not int or not 0<c['max_seconds']<=18000:
        raise ValueError('deadline within daily 5-hour limit required')
    c.update(namespace=ns,scenario='constant',restore_audit=True)
    c.pop('risk_schedule',None)
    info,member,objects,policy=legacy.render(c,source,run_id,'checkpoint','D',namespace=ns)
    policy['kind']='NodeRequest'
    policy['spec']['workloadRef'].pop('uid',None)
    policy['spec']['policy']=copy.deepcopy(c['existing_policy'])
    policy['spec']['policy']['fixedOnDemand']=2 if arm=='E1-A' else 1
    policy['spec']['checkpoint']=copy.deepcopy(c['existing_checkpoint'])
    policy['spec']['replacement']=copy.deepcopy(c['existing_replacement'])
    policy['spec']['capacity'].pop('instanceType',None)
    policy['spec']['capacity']['aws']['instanceType']=c['instance_type']
    policy['spec']['capacity']['aws']['credentialsRef']['namespace']=ns
    annotations=policy['metadata']['annotations']
    annotations['training.dcnlab.com/suspend']='true'
    annotations['training.dcnlab.com/planned-partial']=c['planned_partial_mode']
    if arm=='E1-A':annotations['training.dcnlab.com/periodic-checkpoint']='disabled'
    else:annotations.pop('training.dcnlab.com/periodic-checkpoint',None)
    risk=next(o for o in objects if o['kind']=='SpotRiskProfile')
    risk['spec']=copy.deepcopy(c['existing_risk_spec'])
    if 'paperEstimator' in risk['spec'] and risk['spec']['paperEstimator'].get('method')!='sarima':
        raise ValueError('paperEstimator.method=sarima required by the installed CRD')
    if any(risk['spec'].get(k)!=c[v] for k,v in [('region','region'),('availabilityZone','availability_zone'),('instanceType','instance_type')]):
        raise ValueError('existing risk profile must match chosen hardware/location')
    sts=next(o for o in objects if o['kind']=='StatefulSet')
    sts['spec']['template']['spec']['nodeSelector']={'ml.dcn.ssu.ac.kr/provider':'AWS','ml.dcn.ssu.ac.kr/node-provision-namespace':ns}
    env=sts['spec']['template']['spec']['containers'][0]['env']
    controls={'EVAL_COMPLETION_AUDIT':1,'EVAL_LIFECYCLE_EVENTS':1,'NCCL_IB_DISABLE':1,'FLUIDCR_CHECKPOINT_WATCHDOG_TIMEOUT':240,'CTRL_CHECKPOINT_TIMEOUT':300,'FLUIDCR_DISABLE_ENUMERATE_PATCH':1,'FLUIDCR_DISABLE_ITER_PATCH':1,'FLUIDCR_DISABLE_EPOCH_RANGE_PATCH':1}
    env[:]=[e for e in env if e['name'] not in controls]
    env.extend({'name':k,'value':str(v)} for k,v in controls.items())
    if model=='resnet50':
        env.extend({'name':'EVAL_'+k.upper(),'value':str(c[k])} for k in ('momentum','weight_decay'))
    workload_lock=canonical_hash({k:c.get(k) for k in fields+('instance_type','nfs_server','nfs_model_path','nfs_dataset_path','dataset','target_provenance')})
    # E1-B may serve as E2-A only when this full operational contract matches.
    comparison=canonical_hash({'workload':workload_lock,'policy':policy['spec']['policy'],'checkpoint':policy['spec']['checkpoint'],'replacement':policy['spec']['replacement'],'risk':risk['spec'],'route':c['planned_partial_mode'],'annotations':{'periodic':arm!='E1-A'},'training_code':info['training_code_sha256'],'system_lock':c['system_lock'],'aws_account':c['expected_aws_account'],'ami':policy['spec']['capacity']['aws'].get('ami'),'storage_class':c['storage_class'],'checkpoint_size':c['checkpoint_size']})
    info.update(protocol=PROTOCOL,arm=arm,experiment=arm.split('-')[0],namespace=ns,
                phase='rendered-suspended',ready_for_paid_execution=False,world_size=2,
                completion_audit_required=True,
                tokens_per_committed_step=2*c['batch_size']*c['sequence_length'] if model=='gpt2' else None,
                samples_per_committed_step=2*c['batch_size'] if model=='resnet50' else None,
                useful_unit='tokens' if model=='gpt2' else 'samples',
                workload_lock_sha256=workload_lock,comparison_lock_sha256=comparison,
                checkpoint_lock_sha256=canonical_hash(policy['spec']['checkpoint']),
                external_trace_required=False,policy_ablation=False,
                scheduled_fis=arm=='E2-B',fault_trigger={'type':'committed_global_step','value':math.ceil(c['goal_steps']/2),'status':lock.get('status','PROVISIONAL')} if arm=='E2-B' else None,
                event_after_first_committed_step_seconds=None,hardware_selection=selection,
                required_gates=['selected hardware with actual supply/AMI compatibility','verified assets and frozen target','new namespace/UID/PVC and admission','same 1OD+1Spot baseline for E1-B/E2-A/E2-B','bounded deadline and normal cleanup','clock synchronization','exact owned Spot target if E2-B','both-rank final state and actual provider termination'])
    info['render_sha256']=canonical_hash({'member':member,'objects':objects,'policy':policy})
    return info,member,objects,policy


def compatible_baseline_config(base,other):
    return (base.get('arm') in {'E1-B','E2-A'} and other.get('arm') in {'E2-A','E2-B'}
            and base.get('protocol')==other.get('protocol')==PROTOCOL
            and bool(base.get('comparison_lock_sha256'))
            and base['comparison_lock_sha256']==other.get('comparison_lock_sha256'))


def reusable_baseline(base,other,result):
    return (compatible_baseline_config(base,other)
            and result.get('all_rank_completion_observed') is True
            and result.get('final_state_audit_passed') is True
            and result.get('quality') == 'REVIEWED_VALID_NO_INTERRUPTION'
            and result.get('interruption_count') == 0
            and result.get('estimated_gpu_compute_cost_usd') is not None)


def compare_e2(base_info,fault_info,base_result,fault_result):
    if not reusable_baseline(base_info,fault_info,base_result):
        raise ValueError('baseline is incompatible, incomplete or not reviewed')
    if (fault_info.get('arm')!='E2-B' or fault_result.get('quality')!='REVIEWED_VALID_SINGLE_INTERRUPTION'
        or fault_result.get('interruption_count')!=1 or fault_result.get('all_rank_completion_observed') is not True
        or fault_result.get('final_state_audit_passed') is not True):
        raise ValueError('fault outcome requires explicit single-interruption quality review')
    delta=lambda key: None if base_result.get(key) is None or fault_result.get(key) is None else fault_result[key]-base_result[key]
    return {'extra_completion_seconds':delta('T_E2E_seconds'),
            'extra_gpu_compute_cost_usd':delta('estimated_gpu_compute_cost_usd'),
            'interpretation':'single controlled interruption operating penalty; not prediction accuracy or statistical significance'}


def metrics(info,events,**kwargs):
    if info.get('protocol')!=PROTOCOL or info.get('arm') not in ARMS:raise ValueError('not final E1/E2')
    unit='samples' if info['model']=='resnet50' else 'tokens'
    per_step=info['samples_per_committed_step'] if unit=='samples' else info['tokens_per_committed_step']
    adapted=dict(info,protocol='ieee-access-v2',arm='N0' if info['arm']=='E1-A' else 'N1',tokens_per_committed_step=per_step)
    result=previous_metrics(adapted,events,**kwargs);result['arm']=info['arm'];result['protocol']=PROTOCOL
    cost,tokens=result['estimated_gpu_compute_cost_usd'],result['useful_tokens']
    result.update(model=info['model'],useful_unit=unit)
    result['cost_per_useful_token_usd']=cost/tokens if unit=='tokens' and cost is not None and tokens else None
    result['useful_samples']=tokens if unit=='samples' else None
    result['UsefulThroughput_samples_per_second']=result['UsefulThroughput_tokens_per_second'] if unit=='samples' else None
    result['cost_per_useful_sample_usd']=cost/tokens if unit=='samples' and cost is not None and tokens else None
    if unit=='samples':
        result['useful_tokens']=None
        result['UsefulThroughput_tokens_per_second']=None
    result['cost_scope']='GPU VM Compute Cost; excludes NFS/network/storage'
    return result


def nonproductive_vm_seconds(windows,running):
    # Reviewed no-unique-progress windows, clipped to each VM's actual running
    # billing intervals. No pod/runtime status is substituted for VM state.
    if windows is None or running is None:return None
    if not windows or not running:return 0.0
    def merge(values):
        result=[]
        for a,b in sorted((timestamp(a),timestamp(b)) for a,b in values):
            if b<=a:raise ValueError('nonpositive interval')
            if result and a<=result[-1][1]:result[-1]=(result[-1][0],max(b,result[-1][1]))
            else:result.append((a,b))
        return result
    gaps=merge(windows);total=0.0
    for intervals in running.values():
        for a,b in merge(intervals):
            for x,y in gaps:total+=max(0.0,(min(b,y)-max(a,x)).total_seconds())
    return total


def rollback_steps(last_before,restored):
    if last_before is None or restored is None:return None
    if any(type(v)is not int or v<0 for v in (last_before,restored)) or restored>last_before:raise ValueError('invalid rollback evidence')
    return last_before-restored


def write_prepared(out,info,member,objects,policy):
    out=Path(out)
    if out.exists():raise ValueError('preserve prior attempt; choose a new output')
    out.mkdir(parents=True,mode=0o700)
    risk=next(o for o in objects if o['kind']=='SpotRiskProfile');workload=next(o for o in objects if o['kind']=='StatefulSet')
    values={'run.json':info,'policy.unbound.json':policy,'risk.json':risk,'workload.stopped.json':workload,
            'member-storage.json':{'apiVersion':'v1','kind':'List','items':member},
            'karmada-prerequisites.json':{'apiVersion':'v1','kind':'List','items':[o for o in objects if o['kind'] not in {'SpotRiskProfile','StatefulSet'}]}}
    for filename,obj in values.items():legacy.save(out/filename,obj)
    for filename,fields in SCHEMA.items():
        with (out/filename).open('w',newline='') as f:csv.DictWriter(f,fieldnames=fields).writeheader()
    legacy.save(out/'log-schema.json',{'empty_templates_only':True,'missing_value':'N/A','timestamp_basis':'UTC','schemas':SCHEMA})


def main():
    p=argparse.ArgumentParser(description=__doc__);sub=p.add_subparsers(dest='command',required=True)
    q=sub.add_parser('prepare');q.add_argument('--config',type=Path,required=True);q.add_argument('--policy-source',type=Path,required=True);q.add_argument('--run-id',required=True);q.add_argument('--arm',choices=sorted(ARMS),required=True);q.add_argument('--out',type=Path,required=True)
    q=sub.add_parser('metrics');q.add_argument('--run',type=Path,required=True);q.add_argument('--events',type=Path,required=True);q.add_argument('--billing',type=Path);q.add_argument('--request-at');q.add_argument('--out',type=Path,required=True)
    a=p.parse_args()
    if a.command=='prepare':
        args=render(json.loads(a.config.read_text()),json.loads(a.policy_source.read_text()),a.run_id,a.arm)
        write_prepared(a.out,*args);print('Final-plan draft only: no API calls, no cloud actions; CSVs contain headers, not measurements.')
    else:
        if a.out.exists():raise ValueError('preserve prior analysis output')
        ledger=json.loads(a.billing.read_text()) if a.billing else {}
        events=[json.loads(line) for line in a.events.read_text().splitlines() if line.strip()]
        result=metrics(json.loads(a.run.read_text()),events,request_at=a.request_at,billing=ledger.get('segments'),owned_instances=ledger.get('instances'))
        a.out.mkdir(parents=True,mode=0o700);legacy.save(a.out/'metrics.json',result)
        row={'model':result['model'],'useful_unit':result['useful_unit'],
             'useful_samples':result['useful_samples'],'useful_samples_per_second':result['UsefulThroughput_samples_per_second'],
             'cost_per_useful_sample_usd':result['cost_per_useful_sample_usd'],'run_id':result['run_id'],'arm':result['arm'],'quality':result['quality'],
             'gpu_vm_compute_cost_usd':result['estimated_gpu_compute_cost_usd'],
             'T_startup_seconds':result['T_startup_seconds'],'T_E2E_seconds':result['T_E2E_seconds'],
             'T_training_seconds':result['T_training_seconds'],'useful_tokens':result['useful_tokens'],
             'useful_tokens_per_second':result['UsefulThroughput_tokens_per_second'],
             'cost_per_useful_token_usd':result['cost_per_useful_token_usd'],
             'checkpoint_stall_seconds':result['checkpoint_stall_seconds'],
             'evidence':str(a.events)}
        with (a.out/'results.csv').open('w',newline='') as f:
            writer=csv.DictWriter(f,fieldnames=SCHEMA['results.csv']);writer.writeheader();writer.writerow({k:row.get(k) if row.get(k) is not None else 'N/A' for k in SCHEMA['results.csv']})
        print(json.dumps(result))


if __name__=='__main__':main()
