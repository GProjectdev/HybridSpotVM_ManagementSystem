"""Four-rank live gate: use real progress, ownership and durable-archive validators."""
import copy,json,unittest
from unittest.mock import Mock,patch
from datetime import datetime,timezone
import final_fis as f
from integrated_protocol import PROTOCOL

NOW='2026-11-18T07:00:00Z'
class FourRankFISGateTests(unittest.TestCase):
    def setUp(self):
        ids=['i-0123456789abcdef'+str(i) for i in range(4)]
        self.r={'protocol':PROTOCOL,'arm':'C','phase':'running','world_size':4,'run_id':'case4','namespace':'eval-v3-case4','policy':'p','policy_uid':'p-uid','workload_uid':'w-uid','app':'train','region':'ap-northeast-2','availability_zone':'ap-northeast-2c','instance_type':'g5.xlarge','ready_for_paid_execution':True,'comparison_lock_sha256':'a'*64,'started_at':'2026-11-18T06:00:00Z','hard_deadline_utc':'2026-11-18T14:00:00Z','expected_aws_account':'935317843233','fis_authorization_tag':'reviewed-run','goal_steps':100,'fault_trigger':{'type':'committed_global_step','value':50},'initial_instance_ids':ids,'fis_target_instance_id':ids[1],'fis_stop_alarm_instance_id':ids[0],'cluster':'aws1'}
        r=self.r;label=f.LABEL
        self.policy={'metadata':{'uid':r['policy_uid']},'spec':{'workloadRef':{'uid':r['workload_uid']},'policy':{'method':'paper-eq6-7'}}}
        self.workload={'metadata':{'uid':r['workload_uid']},'spec':{'replicas':4}}
        self.nodes=[{'metadata':{'uid':'np'+str(i),'name':'n'+str(i),'namespace':r['namespace'],'labels':{label+'policy-uid':r['policy_uid']}},'spec':{'marketType':'OnDemand' if i==0 else 'Spot'},'status':{'phase':'Ready','instanceId':ids[i],'nodeName':'n'+str(i)}} for i in range(4)]
        self.pods=[{'metadata':{'name':'train-'+str(i),'uid':'pod'+str(i),'creationTimestamp':r['started_at'],'labels':{label+'workload-uid':r['workload_uid']},'ownerReferences':[{'uid':'member-uid','controller':True}]},'spec':{'nodeName':'n'+str(i),'containers':[{'name':'trainer','env':[{'name':'EVAL_RUN_ID','value':r['run_id']}],'resources':{'requests':{'nvidia.com/gpu':'1'},'limits':{'nvidia.com/gpu':'1'}}}]}} for i in range(4)]
        reports=[{'rank':i,'podUID':'pod'+str(i),'nodeName':'n'+str(i),'phase':'Resumed','checkpointID':'round','checkpointFiles':[{'containerName':'trainer','sha256':str(i)*64,'checkpointID':'round','durableRef':'file-store:'+r['namespace']+'/sha256/'+str(i)*64,'exportedAt':'2026-11-18T06:59:00Z'}]} for i in range(4)]
        self.cp={'metadata':{'uid':'cp','namespace':r['namespace'],'generation':1,'labels':{label+'policy-uid':r['policy_uid']},'annotations':{label+'checkpoint-id':'round'}},'spec':{'workloadRef':{'uid':r['workload_uid']}},'status':{'clusters':[{'clusterName':'aws1','phase':'Completed','observedGeneration':1,'pods':reports}]}}
        self.steps=[50]*4
        self.live=f.Live(r,'profile',{'karmada':'/k','aws':'/a'});self.live.cloud=Mock();self.live.kub=self.kub
        self.live.cloud.observe.return_value=[{'state':'running','provider_ownership_verified':True}]*4
        self.live.cloud.command.return_value={'Reservations':[{'Instances':[{'InstanceId':ids[i],'InstanceLifecycle':'on-demand' if i==0 else 'spot','State':{'Name':'running'},'Placement':{'AvailabilityZone':r['availability_zone']},'InstanceType':r['instance_type']} for i in range(4)]}]}
        r['live_spec_sha256']=f.canonical_hash({'policy_spec':self.policy['spec'],'workload_spec':self.workload['spec']})
    def kub(self,cluster,*args):
        kind=args[1]
        if args[0]=='logs':
            rank=int(kind[-1]);return 'EVAL_EVENT '+json.dumps({'event':'step','run_id':self.r['run_id'],'rank':rank,'world_size':4,'global_step':self.steps[rank],'attempt_id':'current','timestamp':'2026-11-18T06:59:58Z'})
        if kind=='noderequest':return json.dumps(self.policy)
        if kind=='statefulset':return json.dumps(self.workload if cluster=='karmada' else {'metadata':{'uid':'member-uid'}})
        return json.dumps({'items':{'nodeprovisions':self.nodes,'pods':self.pods,'fluidcrmigrations':[self.cp]}[kind]})
    def snapshot(self):
        with patch.object(f,'datetime') as dt:
            dt.now.return_value.isoformat.return_value=NOW
            return self.live.snapshot(NOW)
    def test_exact_one_spot_from_four_and_rank3_progress_is_required(self):
        scope,gate=self.snapshot();self.assertTrue(gate['ready']);self.assertEqual(scope['instance_id'],self.r['initial_instance_ids'][1]);self.assertEqual(len(scope['source_pods']),4)
        self.steps[3]=49;self.assertFalse(self.snapshot()[1]['ready'])
    def test_prior_replacement_or_changed_target_cannot_be_silently_substituted(self):
        self.r['initial_instance_ids'][3]='i-0123456789abcdef9'
        with self.assertRaisesRegex(ValueError,'pre-fault world changed'):self.snapshot()
        self.r['initial_instance_ids'][3]=self.nodes[3]['status']['instanceId'];self.r['fis_target_instance_id']='i-0123456789abcdef9'
        with self.assertRaisesRegex(ValueError,'never retarget'):self.snapshot()
    def test_old_pod_checkpoint_or_missing_rank_archive_blocks_fault(self):
        self.cp['status']['clusters'][0]['pods'][3]['podUID']='oldpod3'
        with self.assertRaisesRegex(ValueError,'current four-rank'):self.snapshot()
        self.cp['status']['clusters'][0]['pods'].pop()
        with self.assertRaisesRegex(ValueError,'current four-rank'):self.snapshot()
    def test_two_ranks_on_one_vm_or_unowned_gpu_allocation_blocks_fault(self):
        self.pods[3]['spec']['nodeName']='n2'
        with self.assertRaisesRegex(ValueError,'distinct'):self.snapshot()
        self.pods[3]['spec']['nodeName']='n3';self.pods[3]['spec']['containers'][0]['resources']['limits']['nvidia.com/gpu']='2'
        with self.assertRaisesRegex(ValueError,'one actual'):self.snapshot()
if __name__=='__main__':unittest.main()
