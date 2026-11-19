import copy,json,tempfile,unittest
from pathlib import Path
from unittest.mock import Mock,patch
import final_fis as f

NOW='2026-11-18T01:00:00Z'
ROLE='arn:aws:iam::935317843233:role/ReviewedFIS'
ALARM='arn:aws:cloudwatch:ap-northeast-2:935317843233:alarm:owned-od'

class FinalFISTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);self.d=Path(self.temp.name)
        self.run={'protocol':'ieee-access-final-20261117','arm':'E2-B','phase':'running','world_size':2,'run_id':'case18','namespace':'eval-final-case18','policy':'policy','policy_uid':'policy-uid','workload_uid':'workload-uid','app':'trainer','region':'ap-northeast-2','availability_zone':'ap-northeast-2c','instance_type':'g5.xlarge','ready_for_paid_execution':True,'comparison_lock_sha256':'a'*64,'live_spec_sha256':'b'*64,'started_at':'2026-11-18T00:00:00Z','hard_deadline_utc':'2026-11-18T09:00:00Z','expected_aws_account':'935317843233','fis_authorization_tag':'v6-readiness-20261117'}
        self.scope={'run_id':'case18','policy_uid':'policy-uid','workload_uid':'workload-uid','nodeprovision_uid':'np-uid','instance_id':'i-0123456789abcdef0','od_instance_id':'i-0123456789abcdef1','comparison_lock_sha256':'a'*64}
        self.live=Mock();self.live.snapshot.return_value=(self.scope,{'ready':True,'progress_valid_until_utc':'2026-11-18T01:00:20Z'});self.live.alarm_ok.return_value=True
        self.live.cloud.command.side_effect=lambda *args:{'experimentTemplate':{'id':'EXT-one'}} if args[1]=='create-experiment-template' else {'experiment':{'id':'EXP-one'}}
    def plan(self):return f.plan(self.d,self.run,self.live,ROLE,ALARM,NOW)
    def start(self):
        with patch.object(f,'datetime') as dt:
            dt.now.return_value.isoformat.return_value=NOW
            return f.start(self.d,self.run,self.live,'case18',NOW)
    def test_plan_is_read_only_and_exact_target_not_a_broad_tag_selector(self):
        p=self.plan();self.live.cloud.command.assert_not_called()
        self.assertEqual(p['template']['tags']['HybridSpotValidationRun'],'v6-readiness-20261117')
        self.assertEqual(p['template']['targets'],{'oneSpot':{'resourceType':'aws:ec2:spot-instance','resourceArns':['arn:aws:ec2:ap-northeast-2:935317843233:instance/'+self.scope['instance_id']],'selectionMode':'ALL'}})
    def test_start_rechecks_after_template_creation_and_claims_single_intent(self):
        self.plan();self.start();self.assertEqual(self.live.snapshot.call_count,3)
        args=[c.args[1] for c in self.live.cloud.command.call_args_list];self.assertEqual(args,['create-experiment-template','start-experiment'])
        with self.assertRaisesRegex(ValueError,'existing intent'):self.start()
        self.assertEqual(self.live.cloud.command.call_count,2)
    def test_formal_four_rank_window_expiry_prevents_dispatch_with_fresh_progress(self):
        self.run.update(protocol='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118',
                        purpose='formal',world_size=4,arm='C',namespace='eval-v3-case18')
        self.live.snapshot.return_value=(self.scope,{'ready':True,
            'progress_valid_until_utc':'2026-11-18T01:00:20Z',
            'risk_window_valid_until_utc':NOW})
        self.plan()
        with self.assertRaisesRegex(ValueError,'native risk window expired'):
            self.start()
        self.assertEqual(self.live.cloud.command.call_count,1)
        self.assertEqual(self.live.cloud.command.call_args.args[1],'create-experiment-template')
    def test_target_replacement_after_template_creation_never_dispatches(self):
        self.plan();self.live.snapshot.side_effect=[(self.scope,{'ready':True,'progress_valid_until_utc':'2026-11-18T01:00:20Z'}),(dict(self.scope,instance_id='i-0123456789abcdef2'),{'ready':True,'progress_valid_until_utc':'2026-11-18T01:00:20Z'})]
        with self.assertRaisesRegex(ValueError,'changed during'):self.start()
        self.assertEqual(self.live.cloud.command.call_count,1)
        self.assertTrue((self.d/'fis-start-intent.json').exists())
    def test_rank_rollback_or_completion_after_template_creation_never_dispatches(self):
        self.plan();self.live.snapshot.side_effect=[(self.scope,{'ready':True,'progress_valid_until_utc':'2026-11-18T01:00:20Z'}),(self.scope,{'ready':False})]
        with self.assertRaisesRegex(ValueError,'changed during'):self.start()
        self.assertEqual(self.live.cloud.command.call_count,1)
    def test_ambiguous_api_failure_keeps_intent_and_prevents_retry(self):
        self.plan();self.live.cloud.command.side_effect=RuntimeError('ambiguous timeout')
        with self.assertRaises(RuntimeError):self.start()
        with self.assertRaisesRegex(ValueError,'existing intent'):self.start()
        self.assertEqual(self.live.cloud.command.call_count,1)
    def test_tampered_template_wrong_run_and_unhealthy_alarm_fail_before_cloud_mutation(self):
        p=self.plan();p['template']['targets']['oneSpot']['resourceArns'].append('other')
        (self.d/'fis-final-plan.json').write_text(json.dumps(p))
        with self.assertRaisesRegex(ValueError,'changed'):self.start()
        self.live.cloud.command.assert_not_called()
        with self.assertRaisesRegex(ValueError,'confirmation'):f.start(self.d,self.run,self.live,'different',NOW)
    def test_invalid_scope_account_role_and_deadline_rejected(self):
        for change in [{'ready_for_paid_execution':False},{'namespace':'default'},{'arm':'E1-A'},{'comparison_lock_sha256':None},{'hard_deadline_utc':'2026-11-18T01:20:00Z'}]:
            with self.subTest(change=change),self.assertRaises(ValueError):f.validate_run(dict(self.run,**change),NOW)
        with self.assertRaises(ValueError):f.template(self.run,self.scope,ROLE.replace('935317843233','111111111111'),ALARM)
        with self.assertRaises(ValueError):f.template(self.run,self.scope,ROLE,ALARM.replace('ap-northeast-2','us-east-1'))
        self.live.alarm_ok.return_value=False
        with self.assertRaisesRegex(ValueError,'healthy'):self.plan()
        self.live.cloud.command.assert_not_called()

    def test_live_snapshot_rejects_foreign_policy_and_has_no_mutations(self):
        live=f.Live(self.run,'explicit-profile',{'karmada':'/k','aws':'/a'})
        live.cloud=Mock();live.kub=Mock(side_effect=[json.dumps({'metadata':{'uid':'foreign'},'spec':{'workloadRef':{'uid':'workload-uid'}}}),json.dumps({'metadata':{'uid':'workload-uid'},'spec':{'replicas':2}})])
        with self.assertRaisesRegex(ValueError,'differs'):live.snapshot(NOW)
        live.cloud.command.assert_not_called()
    def test_live_alarm_is_bound_to_owned_od_not_an_unrelated_healthy_alarm(self):
        live=f.Live(self.run,'explicit-profile',{'karmada':'/k','aws':'/a'});live.cloud=Mock()
        obj={'AlarmArn':ALARM,'StateValue':'OK','Namespace':'AWS/EC2','MetricName':'StatusCheckFailed','Dimensions':[{'Name':'InstanceId','Value':self.scope['od_instance_id']}],'ComparisonOperator':'GreaterThanOrEqualToThreshold','Threshold':1}
        live.cloud.command.return_value={'MetricAlarms':[obj]};self.assertTrue(live.alarm_ok(ALARM,self.scope))
        obj['Dimensions'][0]['Value']='i-unrelated';self.assertFalse(live.alarm_ok(ALARM,self.scope))

    def test_complete_live_path_uses_current_owned_pods_and_real_progress_parser(self):
        r=dict(self.run,goal_steps=100,fault_trigger={'type':'committed_global_step','value':50})
        live=f.Live(r,'explicit-profile',{'karmada':'/k','aws':'/a'});live.cloud=Mock()
        nodes=[]
        for rank,market in [(0,'OnDemand'),(1,'Spot')]:
            nodes.append({'metadata':{'uid':'np'+str(rank),'name':'worker'+str(rank),'namespace':r['namespace'],'labels':{f.LABEL+'policy-uid':r['policy_uid']}},'spec':{'marketType':market},'status':{'phase':'Ready','instanceId':'i-0123456789abcdef'+str(rank)}})
        live.cloud.observe.return_value=[{'state':'running','provider_ownership_verified':True}]*2
        live.cloud.command.return_value={'Reservations':[{'Instances':[{'InstanceId':'i-0123456789abcdef'+str(rank),'InstanceLifecycle':'spot' if rank else 'on-demand','State':{'Name':'running'},'Placement':{'AvailabilityZone':'ap-northeast-2c'},'InstanceType':'g5.xlarge'} for rank in [0,1]]}]}
        pods=[]
        for rank in [0,1]:
            pods.append({'metadata':{'name':'trainer-'+str(rank),'uid':'pod'+str(rank),'creationTimestamp':r['started_at'],'labels':{f.LABEL+'workload-uid':r['workload_uid']},'ownerReferences':[{'uid':'member-uid','controller':True}]},'spec':{'nodeName':'worker'+str(rank),'containers':[{'name':'trainer','env':[{'name':'EVAL_RUN_ID','value':r['run_id']}]}]}})
        def kub(cluster,*args):
            if args[:2]==('get','noderequest'):return json.dumps({'metadata':{'uid':r['policy_uid']},'spec':{'workloadRef':{'uid':r['workload_uid']},'policy':{'fixedOnDemand':1}}})
            if args[:2]==('get','statefulset'):return json.dumps({'metadata':{'uid':r['workload_uid'] if cluster=='karmada' else 'member-uid'},'spec':{'replicas':2}})
            if args[:2]==('get','nodeprovisions'):return json.dumps({'items':nodes})
            if args[:2]==('get','pods'):return json.dumps({'items':pods})
            rank=int(args[1][-1]);return 'EVAL_EVENT '+json.dumps({'event':'step','run_id':r['run_id'],'rank':rank,'world_size':2,'global_step':50,'attempt_id':'current','timestamp':'2026-11-18T00:59:58Z'})
        live.kub=kub
        r['live_spec_sha256']=f.canonical_hash({'policy_spec':json.loads(kub('karmada','get','noderequest'))['spec'],'workload_spec':json.loads(kub('karmada','get','statefulset'))['spec']})
        with patch.object(f,'datetime') as dt:
            dt.now.return_value.isoformat.return_value=NOW
            scope,gate=live.snapshot(NOW)
            self.assertTrue(gate['ready']);self.assertEqual(scope['instance_id'],'i-0123456789abcdef1');self.assertEqual(scope['od_instance_id'],'i-0123456789abcdef0')
            r['live_spec_sha256']='c'*64
            with self.assertRaisesRegex(ValueError,'specs changed'):live.snapshot(NOW)
            r['live_spec_sha256']=scope['live_spec_sha256']
            pods[1]['metadata']['ownerReferences'][0]['uid']='another-member-workload'
            with self.assertRaisesRegex(ValueError,'ownership'):live.snapshot(NOW)

    def test_progress_expiring_while_final_alarm_query_runs_never_dispatches(self):
        self.plan()
        with patch.object(f,'datetime') as dt:
            dt.now.return_value.isoformat.side_effect=[NOW,'2026-11-18T01:00:30Z']
            with self.assertRaisesRegex(ValueError,'expired'):f.start(self.d,self.run,self.live,'case18',NOW)
        self.assertEqual(self.live.cloud.command.call_count,1)

    def test_denied_template_creation_retains_actionable_sanitized_code_and_intent(self):
        self.plan();self.live.cloud.command.side_effect=f.AWSLookupError('AccessDeniedException','fis:CreateExperimentTemplate','arn:aws:fis:ap-northeast-2:935317843233:experiment-template/*')
        with self.assertRaisesRegex(RuntimeError,'AccessDeniedException'):self.start()
        intent=json.loads((self.d/'fis-start-intent.json').read_text())
        self.assertEqual(intent['error_code'],'AccessDeniedException');self.assertEqual(intent['state'],'template_create_failed_or_unknown')
        self.assertEqual(intent['denied_action'],'fis:CreateExperimentTemplate')
        self.assertTrue(intent['denied_resource'].endswith(':experiment-template/*'))
        with self.assertRaisesRegex(ValueError,'existing intent'):self.start()
        self.assertEqual(self.live.cloud.command.call_count,1)
    def test_authorization_tag_is_explicit_and_never_an_implicit_account_default(self):
        run=dict(self.run);run.pop('fis_authorization_tag')
        with self.assertRaisesRegex(ValueError,'authorization tag'):f.validate_run(run,NOW)
