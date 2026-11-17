import json,unittest
from subprocess import CompletedProcess
from e1_experiment import evidence_resource
from ieee_v2_ledger import AWSReader,ownership
from ieee_v2_lifecycle import analyze

class EvidenceTests(unittest.TestCase):
    def test_projection_preserves_decision_and_verified_receipt_but_no_credentials(self):
        x=evidence_resource({'status':{'policy':{'vmPolicyStage':'ThroughputOnlyBootstrap','onDemandHoldUntil':'2026-11-18T01:00:00Z'},'source':{'forecastSHA256':'a'*64,'replayHour':0,'token':'secret'},'verification':{'requestUID':'rr','operation':'op','verifiedAt':'now','sourceFenced':True}},'data':{'password':'secret'},'spec':{'credentialsRef':{'name':'private'}}})
        self.assertEqual(x['status']['source'],{'forecastSHA256':'a'*64,'replayHour':0})
        self.assertEqual(x['status']['verification']['requestUID'],'rr')
        self.assertEqual(x['status']['policy']['vmPolicyStage'],'ThroughputOnlyBootstrap')
        self.assertNotIn('secret',json.dumps(x));self.assertNotIn('credentialsRef',x['spec'])
    def test_exact_provider_queries_and_account_guard(self):
        np={'metadata':{'name':'vm','uid':'np','labels':{'training.dcnlab.com/policy-uid':'policy'}},'spec':{'marketType':'Spot'},'status':{'instanceId':'i-0123456789abcdef0'}}
        owned=ownership([np],'policy');calls=[]
        def run(args,**kw):
            calls.append(args)
            out={'Account':'935317843233'} if 'sts' in args else {'Reservations':[{'Instances':[{'InstanceId':'i-0123456789abcdef0','State':{'Name':'terminated'},'LaunchTime':'now'}]}]}
            return CompletedProcess(args,0,json.dumps(out),'')
        rows=AWSReader('new-account-v2','935317843233','ap-northeast-2',run).observe(owned,'now')
        self.assertEqual(rows[0]['state'],'terminated');self.assertIn('--instance-ids',calls[-1]);self.assertNotIn('--filters',calls[-1])
        with self.assertRaises(ValueError):AWSReader('new-account-v2','000000000000','ap-northeast-2',run).observe(owned,'now')
        np['metadata']['uid']='recreated'
        with self.assertRaises(ValueError):ownership([np],'policy',owned)
    def test_pause_union_and_missing_evidence_are_explicit(self):
        def event(rank,kind,at):return {'checkpoint_id':'cp','rank':rank,'event':kind,'timestamp':'2026-11-17T00:'+at+'Z','pod_uid':'p'+str(rank),'attempt_id':'attempt'}
        events=[event(0,'checkpoint_pause','00:00'),event(1,'checkpoint_pause','00:02'),event(0,'training_resumed','01:00'),event(1,'training_resumed','01:10')]
        out=analyze(events+events);self.assertTrue(out['complete']);self.assertEqual(out['observed_unavailability_seconds'],70)
        self.assertFalse(analyze(events[:-1])['complete'])

    def test_final_state_requires_two_rank_matching_audit_and_real_goal_steps(self):
        from ieee_v2 import metrics
        info={'protocol':'ieee-access-v2','arm':'N0','run_id':'run','goal_steps':2,'tokens_per_committed_step':4096,'completion_audit_required':True}
        events=[]
        for r in (0,1):
            base={'rank':r,'run_id':'run','pod_uid':'p'+str(r),'attempt_id':'attempt','world_size':2}
            for event,step,at in [('step',1,'00:01'),('step',2,'00:02'),('completion_audit',2,'00:05'),('completed',2,'00:06')]:
                row=dict(base,event=event,global_step=step,timestamp='2026-11-17T00:'+at+'Z')
                if event=='completion_audit':row.update(passed=True,model_sha256='a'*64,optimizer_sha256='b'*64,data_contract_sha256='c'*64)
                events.append(row)
        result=metrics(info,events);self.assertTrue(result['final_state_audit_passed']);self.assertEqual(result['T_training_seconds'],1)
        events[6]['model_sha256']='d'*64
        self.assertFalse(metrics(info,events)['final_state_audit_passed'])
        self.assertFalse(metrics(info,[e for e in events if not(e['rank']==1 and e['event']=='step' and e['global_step']==2)])['all_rank_completion_observed'])

    def test_policy_logs_require_exact_uid_namespace_and_strip_other_fields(self):
        from ieee_v2_observe import policy_events
        row={'msg':'EvaluationPolicyDecision','policyUID':'uid','policyNamespace':'eval-v2-run','vmPolicyStage':'ThroughputOnlyBootstrap','password':'secret'}
        lines=['2026-11-17T00:00:00Z '+json.dumps(row), '2026-11-17T00:00:00Z '+json.dumps(dict(row,policyUID='other'))]
        result=list(policy_events('\n'.join(lines),'uid','eval-v2-run'))
        self.assertEqual(len(result),1);self.assertNotIn('password',result[0])

    def test_prices_use_read_only_lookup_and_reject_stale_public_receipt(self):
        from ieee_v2_prices import refresh
        from datetime import datetime,timezone,timedelta
        from pathlib import Path
        import tempfile
        now=datetime(2026,11,17,tzinfo=timezone.utc)
        row={'TermType':'OnDemand','Unit':'Hrs','Currency':'USD','Instance Type':'g5.xlarge','Tenancy':'Shared','Operating System':'Linux','Pre Installed S/W':'NA','Region Code':'ap-northeast-2','PricePerUnit':'1.237'}
        class Reader:
            def observe(self,owned,at):self.owned=owned
            def command(self,*args):
                assert args[:2]==('ec2','describe-spot-price-history')
                return {'SpotPriceHistory':[{'AvailabilityZone':'ap-northeast-2c','InstanceType':'g5.xlarge','ProductDescription':'Linux/UNIX','Timestamp':now.isoformat(),'SpotPrice':'.6'}]}
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'receipt.json';p.write_text(json.dumps({'utc':now.isoformat(),'row':row}))
            reader=Reader();result=refresh(reader,p,'ap-northeast-2c',now)
            self.assertEqual(reader.owned,{});self.assertEqual(result['onDemandPricePerHour'],1.237)
            with self.assertRaises(ValueError):refresh(reader,p,'ap-northeast-2c',now+timedelta(days=2))

    def test_missing_provider_record_is_never_termination_proof(self):
        def run(args,**kwargs):return CompletedProcess(args,0,json.dumps({'Account':'935317843233'} if 'sts' in args else {'Reservations':[]}),'')
        owned={'i-0123456789abcdef0':{'policy_uid':'p','nodeprovision_uid':'np'}}
        rows=AWSReader('new-account-v2','935317843233','ap-northeast-2',run).observe(owned,'now')
        self.assertEqual(rows[0]['state'],'NOT_OBSERVED')
