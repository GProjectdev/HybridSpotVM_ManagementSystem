import copy,json,tempfile,unittest
from pathlib import Path
from unittest.mock import Mock,patch
import integrated_protocol as protocol
import final_fis as fis
import post_risk_observe as observer
import test_integrated_protocol as historical_tests
from test_post_risk_evidence import raw_fixture
from test_post_risk_fault_gate import fixture,NOW


class IntegrationTests(unittest.TestCase):
    def test_renderer_explicit_new_mode_and_historical_contract_unchanged(self):
        base=historical_tests.IntegratedProtocolTests();base.setUp();c=base.c
        old=protocol.render(c,base.source,'old','C')[0]
        self.assertEqual(old['fault_trigger']['value'],4000)
        run,_=fixture();c.update(goal_steps=4800,risk_lock_sha256=run['risk_lock_sha256'],post_risk_gate=run['post_risk_gate'])
        c['training_lock']['goal_steps']=4800;c.pop('fault_window',None)
        b=protocol.render(c,base.source,'newb','B')[0];z=protocol.render(c,base.source,'newc','C')[0]
        self.assertEqual(b['comparison_lock_sha256'],z['comparison_lock_sha256'])
        self.assertIsNone(z['fault_trigger']);self.assertFalse(z['ready_for_paid_execution'])
        self.assertNotIn('fault_window',z)
        c['fault_window']={'old':True}
        with self.assertRaisesRegex(ValueError,'historical'):protocol.render(c,base.source,'mixed','C')

    def prepare(self,d):
        r,o,s,decisions=raw_fixture();r['other_cost_reserve_usd']=5
        s['manifest']['namespace']=r['namespace']
        snapshot=d/'snapshot';snapshot.mkdir()
        for name,obj in dict(s,**{'training-events':o['training_events'],'policy-decisions':decisions}).items():
            (snapshot/(name+'.json')).write_text(json.dumps(obj))
        watch=dict(run_id=r['run_id'],at=NOW,phase='training',stop_requested=False,sample=dict(
            nfs_free_bytes=200*1024**3,local_free_bytes=120*1024**3,
            estimated_GPU_and_root_cost_usd=10,cleanup_compute_reserve_usd=3))
        return r,o,snapshot,watch

    def test_file_based_shadow_latches_without_cloud_clients_or_source_mutation(self):
        with tempfile.TemporaryDirectory() as t:
            d=Path(t);r,o,s,w=self.prepare(d);original={p.name:p.read_bytes() for p in s.iterdir()}
            first,bundle=observer.evaluate(d,r,s,w,NOW)
            self.assertTrue(first['ready']);self.assertEqual(bundle['safety']['cleanup_cost_reserve_usd'],8)
            self.assertFalse(json.loads((d/'post-risk-first-gate.json').read_text())['paid_execution_authorized_by_this_receipt'])
            again,_=observer.evaluate(d,r,s,w,NOW)
            self.assertTrue(again['shadow_final'])
            self.assertEqual(original,{p.name:p.read_bytes() for p in s.iterdir()})
            self.assertFalse((d/'fis-start-intent.json').exists())

    def test_first_target_or_later_archive_cannot_replace_eligible_record(self):
        with tempfile.TemporaryDirectory() as t:
            d=Path(t);r,o,s,w=self.prepare(d);r['arm']='C'
            first,_=observer.evaluate(d,r,s,w,NOW)
            with self.assertRaisesRegex(ValueError,'never retarget'):
                observer.enforce_first(r,first,dict(first,checkpoint_uid='later'),NOW)
            expired=observer.enforce_first(r,first,first,'2026-11-23T01:12:00Z')
            self.assertFalse(expired['ready']);self.assertTrue(expired['terminal'])

    def test_namespace_and_safety_failure_never_grant_gate(self):
        with tempfile.TemporaryDirectory() as t:
            d=Path(t);r,o,s,w=self.prepare(d);w['run_id']='other'
            with self.assertRaises(ValueError):observer.evaluate(d,r,s,w,NOW)
            self.assertFalse((d/'post-risk-first-gate.json').exists())

    def test_dispatch_uses_new_admission_deadline_not_historical_50percent_window(self):
        for expired in (False,True):
            with self.subTest(expired=expired),tempfile.TemporaryDirectory() as t:
                d=Path(t);r,o=fixture();r.update(arm='C',phase='running',purpose='formal',
                    expected_aws_account='935317843233',region='ap-northeast-2',availability_zone='ap-northeast-2c',
                    ready_for_paid_execution=True,comparison_lock_sha256='a'*64,live_spec_sha256='b'*64,
                    fis_authorization_tag='test',started_at=r['source_created_at'])
                live=Mock();live.snapshot.return_value=({},dict(ready=True,progress_valid_until_utc='2026-11-23T01:10:30Z',
                    admission_valid_until_utc=NOW if expired else '2026-11-23T01:10:20Z'))
                live.alarm_ok.return_value=True
                live.cloud.command.side_effect=lambda *a: {'experimentTemplate':{'id':'template'}} if a[1]=='create-experiment-template' else {'experiment':{'id':'experiment'}}
                with patch.object(fis,'template',return_value={'roleArn':'role','stopConditions':[{'value':'alarm'}],'tags':{}}),patch.object(fis,'datetime') as clock:
                    clock.now.return_value.isoformat.return_value=NOW
                    fis.plan(d,r,live,'role','alarm',NOW)
                    if expired:
                        with self.assertRaisesRegex(ValueError,'admission expired'):fis.start(d,r,live,r['run_id'],NOW)
                        self.assertEqual(live.cloud.command.call_count,1)
                    else:
                        fis.start(d,r,live,r['run_id'],NOW)
                        self.assertEqual(live.cloud.command.call_count,2)

    def test_live_hook_refreshes_archive_and_rejects_new_operation(self):
        from post_risk_live import PostRiskLive
        from datetime import datetime
        for conflict in (False,True):
            with self.subTest(conflict=conflict),tempfile.TemporaryDirectory() as t:
                d=Path(t);r,o,s,w=self.prepare(d);r['arm']='C'
                _,_,raw,_=raw_fixture()
                policy=raw['karmada-noderequests']['items'][0]
                r.update(expected_aws_account='935317843233',region='ap-northeast-2')
                live=PostRiskLive(r,'unused',{},d)
                def kub(cluster,*args):
                    kind=args[1]
                    if kind=='spotriskprofile':return json.dumps(raw['karmada-spotriskprofiles']['items'][0])
                    if kind=='fluidcrmigrations':return json.dumps(raw['aws-fluidcrmigrations'])
                    items=[]
                    if conflict and kind=='restorerequests':
                        items=[dict(metadata=dict(uid='active',labels={'training.dcnlab.com/policy-uid':r['policy_uid']}),status=dict(phase='Restoring'))]
                    return json.dumps(dict(items=items))
                live.kub=kub
                with patch.object(observer,'latest_inputs',return_value=(s,w)),patch('datetime.datetime') as clock:
                    clock.now.return_value.isoformat.return_value=NOW
                    result=live.final_gate(policy,o['current_pods'],[],NOW)
                self.assertEqual(result['ready'],not conflict)
                if conflict:self.assertEqual(result['reason'],'active_operation_conflict')

    def test_gate_failure_requests_existing_uid_safe_cleanup_only(self):
        import operational_watch as safety
        with tempfile.TemporaryDirectory() as t:
            d=Path(t);r,_=fixture()
            self.assertFalse(safety.external_gate_stop(d,r))
            observer.request_stop(d,r,'synthetic sensor failure',NOW)
            self.assertTrue(safety.external_gate_stop(d,r))
            historical=dict(r);historical.pop('post_risk_gate')
            self.assertFalse(safety.external_gate_stop(d,historical))
            other=dict(r,policy_uid='foreign')
            with self.assertRaises(ValueError):safety.external_gate_stop(d,other)

    def test_watch_sensor_failure_bounded_and_no_cloud_mutation(self):
        with tempfile.TemporaryDirectory() as t:
            d=Path(t);r,_=fixture();r['phase']='running'
            (d/'run.json').write_text(json.dumps(r))
            with patch.object(observer.time,'sleep'),patch.object(observer,'datetime') as clock:
                clock.now.return_value.isoformat.return_value=NOW
                observer.watch(d,30)
            self.assertEqual(observer.read(d/'post-risk-stop-request.json')['reason'],'two_post_risk_evidence_failures')
            self.assertEqual(len((d/'post-risk-observer-errors.jsonl').read_text().splitlines()),2)
            self.assertFalse((d/'fis-start-intent.json').exists())

    def test_shadow_cannot_enter_fis_dispatcher(self):
        r,_=fixture();r['phase']='running'
        with self.assertRaises(ValueError):fis.validate_run(r,NOW)


if __name__=='__main__':unittest.main()
