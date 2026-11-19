import copy,csv,json,tempfile,unittest
from pathlib import Path
import final_protocol as final
from ieee_v2_observe import validate_run

class FinalProtocolTests(unittest.TestCase):
    def test_metrics_cli_accepts_reviewed_rank_pause_intervals_without_double_count(self):
        import subprocess, sys
        info=final.render(self.c,self.source,'stall-cli','E1-B')[0]
        with tempfile.TemporaryDirectory() as tmp:
            d=Path(tmp)
            (d/'run.json').write_text(json.dumps(info));(d/'events.jsonl').write_text('')
            (d/'stalls.json').write_text(json.dumps([
                ['2026-11-18T00:00:00Z','2026-11-18T00:00:03Z'],
                ['2026-11-18T00:00:02Z','2026-11-18T00:00:05Z']]))
            subprocess.run([sys.executable,final.__file__,'metrics','--run',str(d/'run.json'),
                            '--events',str(d/'events.jsonl'),'--checkpoint-stalls',str(d/'stalls.json'),
                            '--out',str(d/'out')],check=True,capture_output=True,text=True)
            result=json.loads((d/'out'/'metrics.json').read_text())
            self.assertEqual(result['checkpoint_stall_seconds'],5)
            self.assertEqual(result['quality'],'INCOMPLETE')

    def setUp(self):
        root=Path(__file__).resolve().parents[1]
        self.c=json.loads((root/'experiments/ieee-access-final-20261117/config/run_config.provisional.json').read_text())
        self.source={'spec':{'capacity':{'aws':{'instanceType':'g5.xlarge','ami':'ami-reviewed','subnetId':'subnet-a','vpcId':'vpc-a','securityGroupIds':['sg-a'],'credentialsRef':{'name':'credentials','namespace':'old'}}}}}
    def resnet(self):
        c=copy.deepcopy(self.c)
        c.update(model='resnet50',sequence_length=None,batch_size=8,goal_steps=100,
                 learning_rate=.1,momentum=.9,weight_decay=.0005,
                 dataset={'name':'cifar10','split':'train','examples':50000},
                 target_provenance={'model':'resnet50','status':'PILOT_REQUIRED'},
                 nfs_dataset_path='/mnt/nfs/reviewed-cifar10',nfs_model_path=None)
        c['training_lock'].update({k:c[k] for k in ('model','sequence_length','batch_size','goal_steps','learning_rate','momentum','weight_decay')})
        return c

    def test_resnet_uses_cifar_samples_and_cannot_mix_gpt_baseline(self):
        c=self.resnet()
        values={a:final.render(c,self.source,'rn-'+a.lower(),a) for a in final.ARMS}
        for info,member,objects,policy in values.values():
            self.assertEqual(info['model'],'resnet50');self.assertEqual(info['useful_unit'],'samples')
            self.assertIsNone(info['tokens_per_committed_step']);self.assertEqual(info['samples_per_committed_step'],16)
            sts=next(o for o in objects if o['kind']=='StatefulSet')
            env={v['name']:v.get('value') for v in sts['spec']['template']['spec']['containers'][0]['env']}
            self.assertEqual(env['EVAL_MODEL'],'resnet50');self.assertEqual(env['EVAL_MOMENTUM'],'0.9')
            self.assertNotIn('EVAL_MODEL_ROOT',env)
            self.assertFalse(any(p['metadata']['name'].endswith('-model') for p in member))
        self.assertTrue(final.compatible_baseline_config(values['E1-B'][0],values['E2-B'][0]))
        gpt=final.render(self.c,self.source,'gpt','E1-B')[0]
        self.assertFalse(final.compatible_baseline_config(gpt,values['E2-B'][0]))
        c['target_provenance']['model']='gpt2'
        with self.assertRaises(ValueError):final.render(c,self.source,'bad','E1-B')

    def test_model_mismatch_and_resnet_unit_reporting_fail_closed(self):
        c=self.resnet();c['training_lock']['model']='gpt2'
        with self.assertRaises(ValueError):final.render(c,self.source,'bad','E1-A')
        info=final.render(self.resnet(),self.source,'rn-metrics','E1-A')[0]
        result=final.metrics(info,[])
        self.assertEqual(result['useful_unit'],'samples');self.assertIsNone(result['useful_tokens'])
        self.assertIsNone(result['cost_per_useful_token_usd']);self.assertIsNone(result['useful_samples'])

    def test_resnet_completion_reports_samples_and_keeps_rank_local_bn_buffers(self):
        info=final.render(self.resnet(),self.source,'rn-complete','E1-B')[0]
        events=[]
        for rank in (0,1):
            for kind,step,second in [('step',1,1),('step',100,2),('completion_audit',100,3),('completed',100,4)]:
                e=dict(run_id=info['run_id'],rank=rank,world_size=2,pod_uid='p'+str(rank),attempt_id='attempt',event=kind,global_step=step,timestamp='2026-11-17T00:00:0'+str(second)+'Z')
                if kind=='completion_audit':e.update(passed=True,model_sha256='a'*64,optimizer_sha256='b'*64,data_contract_sha256='c'*64,model_digest_scope='parameters',rank_local_buffers_sha256=str(rank)*64)
                events.append(e)
        result=final.metrics(info,events)
        self.assertTrue(result['final_state_audit_passed']);self.assertEqual(result['useful_samples'],1600)
        self.assertEqual(result['UsefulThroughput_samples_per_second'],1600);self.assertIsNone(result['useful_tokens'])
        events[6].pop('model_digest_scope')
        self.assertFalse(final.metrics(info,events)['final_state_audit_passed'])

    def test_final_pair_has_same_operational_contract_and_no_trace_requirement(self):
        values={a:final.render(self.c,self.source,a.lower(),a) for a in final.ARMS}
        for a,(info,storage,objects,policy) in values.items():
            self.assertEqual(info['protocol'],final.PROTOCOL);self.assertFalse(info['ready_for_paid_execution']);self.assertNotIn('uid',policy['spec']['workloadRef'])
            self.assertFalse(info['external_trace_required']);self.assertEqual(policy['metadata']['annotations']['training.dcnlab.com/suspend'],'true')
            self.assertEqual(policy['spec']['policy']['fixedOnDemand'],2 if a=='E1-A' else 1)
            self.assertEqual('training.dcnlab.com/periodic-checkpoint' in policy['metadata']['annotations'],a=='E1-A')
            validate_run(info)
        hashes={values[a][0]['comparison_lock_sha256'] for a in ['E1-B','E2-A','E2-B']}
        self.assertEqual(len(hashes),1)
        self.assertTrue(final.compatible_baseline_config(values['E1-B'][0],values['E2-B'][0]))
        self.assertFalse(final.compatible_baseline_config(values['E1-A'][0],values['E2-B'][0]))
        self.assertEqual(values['E2-B'][0]['fault_trigger']['value'],2374)
        self.assertIsNone(values['E2-B'][0]['event_after_first_committed_step_seconds'])
    def test_alternative_gpu_and_explicit_new_target_are_supported_without_algorithm_change(self):
        c=copy.deepcopy(self.c);c['instance_type']='g6.xlarge';c['hardware_selection'].update(instance_type='g6.xlarge',status='SELECTED');c['existing_risk_spec']['instanceType']='g6.xlarge';c['goal_steps']=3000;c['training_lock'].update(goal_steps=3000,status='LOCKED')
        info,_,_,policy=final.render(c,self.source,'g6-draft','E2-B')
        self.assertNotIn('instanceType',policy['spec']['capacity']);self.assertEqual(policy['spec']['capacity']['aws']['instanceType'],'g6.xlarge');self.assertEqual(info['fault_trigger']['value'],1500)
        self.assertEqual(self.source['spec']['capacity']['aws']['instanceType'],'g5.xlarge')
        c['goal_steps']=3001
        with self.assertRaises(ValueError):final.render(c,self.source,'bad','E1-B')
    def test_paper_risk_source_requires_actual_crd_method(self):
        source=json.loads((Path(__file__).resolve().parents[1]/'experiments/ieee-access-v2/config/risk_replay_manifest.json').read_text())['profile_spec']
        self.assertEqual(source['paperEstimator']['method'],'sarima')
        c=copy.deepcopy(self.c);c['existing_risk_spec']=copy.deepcopy(source)
        self.assertEqual(final.render(c,self.source,'paper','E1-B')[0]['existing_risk_spec']['paperEstimator']['method'],'sarima')
        c['existing_risk_spec']['paperEstimator'].pop('method')
        with self.assertRaises(ValueError):final.render(c,self.source,'bad','E1-B')

    def test_e3_and_mismatched_profile_are_blocked(self):
        with self.assertRaises(ValueError):final.render(self.c,self.source,'e3','E3')
        c=copy.deepcopy(self.c);c['existing_risk_spec']['instanceType']='g6.xlarge'
        with self.assertRaises(ValueError):final.render(c,self.source,'bad','E2-B')
    def test_output_is_inert_and_immutable_with_empty_csv_headers(self):
        args=final.render(self.c,self.source,'draft','E1-B')
        with tempfile.TemporaryDirectory() as d:
            out=Path(d)/'new';final.write_prepared(out,*args)
            prerequisites=json.loads((out/'karmada-prerequisites.json').read_text())['items']
            self.assertFalse(any(o['kind'] in {'StatefulSet','SpotRiskProfile'} for o in prerequisites))
            for filename in final.SCHEMA:
                with (out/filename).open() as f:self.assertEqual(len(list(csv.reader(f))),1)
            with self.assertRaises(ValueError):final.write_prepared(out,*args)
    def test_terminated_spot_is_not_counted_beyond_its_running_lifetime(self):
        t=lambda s:'2026-11-17T00:00:'+str(s).zfill(2)+'Z'
        result=final.nonproductive_vm_seconds([(t(10),t(30)),(t(12),t(25))],{'spot':[(t(0),t(15))],'od':[(t(0),t(40))],'replacement':[(t(20),t(40))]})
        self.assertEqual(result,35)
        self.assertIsNone(final.nonproductive_vm_seconds(None,{}))
        self.assertEqual(final.rollback_steps(100,80),20)
        self.assertIsNone(final.rollback_steps(None,80))
        with self.assertRaises(ValueError):final.rollback_steps(10,20)
    def test_collector_rejects_mixing_old_namespace_or_unverified_world_size(self):
        info=final.render(self.c,self.source,'draft','E1-B')[0]
        for patch in [{'namespace':'eval-v2-draft'},{'world_size':4},{'arm':'P0'}]:
            with self.assertRaises(ValueError):validate_run(dict(info,**patch))

    def test_baseline_reuse_requires_reviewed_complete_uninterrupted_outcome(self):
        b=final.render(self.c,self.source,'base','E1-B')[0];f=final.render(self.c,self.source,'fault','E2-B')[0]
        result={'all_rank_completion_observed':True,'final_state_audit_passed':True,'quality':'REVIEWED_VALID_NO_INTERRUPTION','interruption_count':0,'estimated_gpu_compute_cost_usd':2,'T_E2E_seconds':100}
        self.assertFalse(final.reusable_baseline(b,f,{}));self.assertTrue(final.reusable_baseline(b,f,result))
        fault=dict(result,quality='REVIEWED_VALID_SINGLE_INTERRUPTION',interruption_count=1,estimated_gpu_compute_cost_usd=3,T_E2E_seconds=150)
        compared=final.compare_e2(b,f,result,fault);self.assertEqual(compared['extra_completion_seconds'],50);self.assertEqual(compared['extra_gpu_compute_cost_usd'],1)
        with self.assertRaises(ValueError):final.compare_e2(b,f,result,dict(fault,interruption_count=2))
