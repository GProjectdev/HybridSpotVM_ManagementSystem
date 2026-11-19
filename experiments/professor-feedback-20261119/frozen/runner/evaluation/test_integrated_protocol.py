import copy,json,unittest
from pathlib import Path
import integrated_protocol as v3
from final_fault_gate import check
from ieee_v2_observe import log_events,validate_run

class IntegratedProtocolTests(unittest.TestCase):
    def setUp(self):
        root=Path(__file__).resolve().parents[1]
        self.c=json.loads((root/'experiments/ieee-access-final-20261117/config/run_config.provisional.json').read_text())
        self.c.update(world_size=4,purpose='acceptance',batch_size=2,goal_steps=8000)
        self.c['training_lock'].update(batch_size=2,goal_steps=8000)
        self.source={'spec':{'capacity':{'aws':{'ami':'ami-review','subnetId':'subnet-a','vpcId':'vpc-a','securityGroupIds':['sg-a'],'credentialsRef':{'name':'credentials','namespace':'old'}}}}}
    def test_renderer_four_rank_and_shared_bc(self):
        runs={a:v3.render(self.c,self.source,'check-'+a.lower(),a) for a in ('A','B','C')}
        self.assertEqual(runs['B'][0]['comparison_lock_sha256'],runs['C'][0]['comparison_lock_sha256'])
        self.assertNotEqual(runs['A'][0]['comparison_lock_sha256'],runs['B'][0]['comparison_lock_sha256'])
        for a,(info,storage,objects,policy) in runs.items():
            validate_run(info);self.assertEqual(info['world_size'],4)
            self.assertEqual(policy['spec']['targetWorkers'],4)
            sts=next(o for o in objects if o['kind']=='StatefulSet')
            env={e['name']:e.get('value') for e in sts['spec']['template']['spec']['containers'][0]['env']}
            self.assertEqual(env['WORLD_SIZE'],'4');self.assertEqual(env['EVAL_BATCH_SIZE'],'2')
            self.assertEqual(policy['spec']['policy'].get('fixedOnDemand'),4 if a=='A' else None)
            self.assertEqual('training.dcnlab.com/periodic-checkpoint' in policy['metadata']['annotations'],a=='A')
            self.assertEqual(info['tokens_per_committed_step'],4096)
        self.assertEqual(runs['C'][0]['fault_trigger']['value'],4000)
    def test_batch_size_is_shared_bc_condition_and_cost_overlap_lock(self):
        baseline=v3.render(self.c,self.source,'serial','B')[0]
        self.assertEqual(baseline['group_replacement_batch_size'],1)
        self.assertEqual(baseline['max_expected_worker_instances'],5)
        self.c['group_replacement_batch_size']=3
        b,_,_,policy=v3.render(self.c,self.source,'batch','B')
        c=v3.render(self.c,self.source,'batchc','C')[0]
        self.assertEqual(policy['metadata']['annotations']['training.dcnlab.com/group-replacement-batch-size'],'3')
        self.assertEqual(b['comparison_lock_sha256'],c['comparison_lock_sha256'])
        self.assertNotEqual(baseline['comparison_lock_sha256'],b['comparison_lock_sha256'])
        self.assertEqual(b['max_expected_worker_instances'],7)
        self.assertEqual(v3.render(self.c,self.source,'batcha','A')[0]['max_expected_worker_instances'],4)
        for value in (0,5,True,2.5,'3',None):
            bad=dict(self.c,group_replacement_batch_size=value)
            with self.assertRaisesRegex(ValueError,'batch_size'):v3.render(bad,self.source,'badbatch','B')
    def test_checkpoint_buffer_io_mode_is_locked_between_bc(self):
        b=v3.render(self.c,self.source,'reuseb','B')[0]
        c,_,objects,_=v3.render(self.c,self.source,'reusec','C')
        self.assertEqual(b['comparison_lock_sha256'],c['comparison_lock_sha256'])
        env={e['name']:e.get('value') for o in objects if o['kind']=='StatefulSet' for e in o['spec']['template']['spec']['containers'][0]['env']}
        self.assertEqual(env['FLUIDCR_REUSE_UNCHANGED_CHECKPOINT'],'1')
        self.c['checkpoint_buffer_reuse']=0
        self.assertNotEqual(b['comparison_lock_sha256'],v3.render(self.c,self.source,'noreuse','C')[0]['comparison_lock_sha256'])
        for bad in (True,'1',2,None):
            self.c['checkpoint_buffer_reuse']=bad
            with self.assertRaisesRegex(ValueError,'buffer_reuse'):v3.render(self.c,self.source,'badreuse','B')
    def test_static_or_unfrozen_formal_config_rejected(self):
        self.c['purpose']='formal'
        with self.assertRaises(ValueError):v3.render(self.c,self.source,'bad','C')
    def test_omitted_checkpoint_maximum_survives_render_and_bc_lock(self):
        self.c['existing_checkpoint'].pop('maxIntervalSeconds',None)
        b,_,_,policy=v3.render(self.c,self.source,'uncappedb','B')
        c=v3.render(self.c,self.source,'uncappedc','C')[0]
        self.assertNotIn('maxIntervalSeconds',policy['spec']['checkpoint'])
        self.assertEqual(b['comparison_lock_sha256'],c['comparison_lock_sha256'])
    def test_run_scoped_feed_transport_preserves_common_bc_contract(self):
        risk=self.c['existing_risk_spec'];risk.pop('staticLambdaPerHour',None)
        template='https://risk-replay.{namespace}.svc:8443/risk'
        risk['paperEstimator']={'method':'sarima','feedEndpoint':template}
        b,_,objects_b,_=v3.render(self.c,self.source,'feedb','B')
        c,_,objects_c,_=v3.render(self.c,self.source,'feedc','C')
        self.assertEqual(b['comparison_lock_sha256'],c['comparison_lock_sha256'])
        for info,objects in ((b,objects_b),(c,objects_c)):
            endpoint=next(o for o in objects if o['kind']=='SpotRiskProfile')['spec']['paperEstimator']['feedEndpoint']
            self.assertEqual(endpoint,template.replace('{namespace}',info['namespace']))
            self.assertEqual(info['risk_transport_binding']['feed_endpoint'],endpoint)
        risk['paperEstimator']['feedEndpoint']=template.replace('/risk','/different-input')
        self.assertNotEqual(b['comparison_lock_sha256'],v3.render(self.c,self.source,'otherfeed','B')[0]['comparison_lock_sha256'])
    def test_external_feed_requires_no_fabricated_static_risk_fields(self):
        for key in ('initial_risk','changed_risk','risk_change_after_seconds'):
            self.c.pop(key,None)
        risk=self.c['existing_risk_spec'];risk.pop('staticLambdaPerHour',None)
        risk['paperEstimator']={'method':'sarima','feedEndpoint':'https://risk-replay.{namespace}.svc:8443/risk'}
        self.c['fault_window']={'anchor':'source NodeRequest creationTimestamp',
            'transition_after_seconds':3600,'notice_seconds':120,'margin_seconds':120}
        b,_,objects,_=v3.render(self.c,self.source,'nativexb','B')
        c=v3.render(self.c,self.source,'nativexc','C')[0]
        self.assertEqual(b['comparison_lock_sha256'],c['comparison_lock_sha256'])
        self.assertEqual(c['fault_window'],self.c['fault_window'])
        self.assertNotIn('staticLambdaPerHour',next(o for o in objects if o['kind']=='SpotRiskProfile')['spec'])
        self.assertNotIn('initial_risk',b)
        self.c['fault_window']['margin_seconds']=121
        self.assertNotEqual(b['comparison_lock_sha256'],v3.render(self.c,self.source,'otherwindow','C')[0]['comparison_lock_sha256'])
    def test_changed_replacement_or_storage_cannot_reuse_bc_baseline(self):
        b=v3.render(self.c,self.source,'base','B')[0]['comparison_lock_sha256']
        for change in ({'existing_replacement':{'enabled':False}},{'checkpoint_size':'20Gi'},{'storage_class':'other-nfs'}):
            changed=copy.deepcopy(self.c);changed.update(change)
            self.assertNotEqual(b,v3.render(changed,self.source,'changed','C')[0]['comparison_lock_sha256'])
    def test_manual_model_acceptance_never_disables_formal_bc_checkpointing(self):
        self.c['acceptance_manual_checkpoint']=True
        info,_,_,p=v3.render(self.c,self.source,'manual','B')
        self.assertTrue(info['manual_checkpoint_diagnostic']);self.assertFalse(info['formal_E1_E2_result'])
        self.assertEqual(p['metadata']['annotations']['training.dcnlab.com/periodic-checkpoint'],'disabled')
        self.c['purpose']='formal'
        with self.assertRaisesRegex(ValueError,'manual checkpoint'):v3.render(self.c,self.source,'manual','B')
    def test_wrong_batch_or_world_rejected(self):
        for change in ({'batch_size':4},{'world_size':2}):
            with self.assertRaises(ValueError):v3.render(dict(self.c,**change),self.source,'bad','B')
    def test_all_four_current_ranks_required_at_half_goal(self):
        run={'protocol':v3.PROTOCOL,'world_size':4,'arm':'C','run_id':'run','goal_steps':100,'fault_trigger':{'type':'committed_global_step','value':50},'policy_uid':'p','workload_uid':'w','started_at':'2026-11-18T07:00:00Z'}
        pods={i:'pod'+str(i) for i in range(4)}
        events=[dict(run_id='run',rank=i,world_size=4,global_step=50,timestamp='2026-11-18T07:00:01Z',event='step',pod_uid=pods[i],attempt_id='a') for i in range(4)]
        kw={'now':'2026-11-18T07:00:03Z','current_pods':pods}
        self.assertTrue(check(run,events,**kw)['ready'])
        self.assertFalse(check(run,events[:3],**kw)['ready'])
        events[2]['global_step']=49;self.assertFalse(check(run,events,**kw)['ready'])
        events[2]['global_step']=50;events[2]['timestamp']='2026-11-18T06:59:00Z';self.assertFalse(check(run,events,**kw)['ready'])
        run['fault_trigger']['value']=40
        with self.assertRaises(ValueError):check(run,events,**kw)
    def test_observer_retains_rank3_and_rejects_foreign_world(self):
        info=v3.render(self.c,self.source,'obs','B')[0]
        pod={'metadata':{'uid':'pod3','name':'eval-obs-3','creationTimestamp':'2026-11-18T07:00:00Z'},'spec':{'nodeName':'n3','containers':[{'name':'trainer','env':[{'name':'EVAL_RUN_ID','value':'obs'}]}]}}
        e={'run_id':'obs','rank':3,'world_size':4,'global_step':1,'timestamp':'2026-11-18T07:00:01Z','event':'step','attempt_id':'a'}
        self.assertEqual(list(log_events('EVAL_EVENT '+json.dumps(e),info,pod))[0]['rank'],3)
        e['world_size']=2;self.assertEqual(list(log_events('EVAL_EVENT '+json.dumps(e),info,pod)),[])
    def test_only_complete_same_round_four_rank_durable_checkpoint(self):
        run={'namespace':'eval-v3-test','policy_uid':'p','workload_uid':'w','cluster':'aws1','world_size':4}
        pods=[{'rank':i,'podUID':'pod'+str(i),'nodeName':'n'+str(i),'phase':'Resumed','checkpointID':'round','checkpointFiles':[{'containerName':'trainer','sha256':str(i)*64,'checkpointID':'round','durableRef':'file-store:eval-v3-test/sha256/'+str(i)*64,'exportedAt':'2026-11-18T07:00:00Z'}]} for i in range(4)]
        obj={'metadata':{'uid':'cp','namespace':run['namespace'],'generation':1,'labels':{'training.dcnlab.com/policy-uid':'p'},'annotations':{'training.dcnlab.com/checkpoint-id':'round'}},'spec':{'workloadRef':{'uid':'w'}},'status':{'clusters':[{'clusterName':'aws1','phase':'Completed','observedGeneration':1,'pods':pods}]}}
        self.assertTrue(v3.durable_group_checkpoint(obj,run))
        for change in ('missing','round','sha'):
            bad=copy.deepcopy(obj);ps=bad['status']['clusters'][0]['pods']
            if change=='missing':ps.pop()
            elif change=='round':ps[3]['checkpointID']='other'
            else:ps[3]['checkpointFiles'][0]['sha256']='bad'
            self.assertFalse(v3.durable_group_checkpoint(bad,run))
    def test_four_rank_e2e_metrics_do_not_count_replay_or_hashing_as_training(self):
        info=v3.render(self.c,self.source,'metric','B')[0];info['goal_steps']=2
        events=[]
        for rank in range(4):
            for kind,step,second in [('step',1,10),('step',2,70),('completion_audit',2,80),('completed',2,81)]:
                e={'run_id':'metric','rank':rank,'world_size':4,'pod_uid':'pod'+str(rank),'attempt_id':'a','event':kind,'global_step':step,'timestamp':'2026-11-18T07:01:%02dZ'%(second-60) if second>=60 else '2026-11-18T07:00:%02dZ'%second}
                if kind=='completion_audit':e.update(passed=True,model_sha256='a'*64,optimizer_sha256='b'*64,data_contract_sha256='c'*64,model_digest_scope='parameters',rank_local_buffers_sha256=str(rank)*64)
                events.append(e)
        m=v3.metrics(info,events+events,request_at='2026-11-18T07:00:00Z')
        self.assertEqual(m['committed_useful_units'],8192)
        self.assertEqual(m['T_E2E_seconds'],70);self.assertEqual(m['T_training_seconds'],60)
        self.assertAlmostEqual(m['useful_units_per_E2E_second'],8192/70)
        self.assertTrue(m['final_state_audit_passed']);self.assertFalse(m['formal_E1_E2_result'])
        self.assertFalse(v3.metrics(info,[e for e in events if e['rank']!=3])['all_rank_completion_observed'])
        resnet=dict(info,model='resnet50',tokens_per_committed_step=None,samples_per_committed_step=64)
        m=v3.metrics(resnet,events,request_at='2026-11-18T07:00:00Z')
        self.assertEqual(m['committed_useful_units'],128);self.assertIsNone(m['useful_tokens']);self.assertEqual(m['useful_unit'],'samples')
        self.assertIsNone(m['UsefulThroughput_tokens_per_second']);self.assertAlmostEqual(m['UsefulThroughput_samples_per_second'],128/60)
if __name__=='__main__':unittest.main()
