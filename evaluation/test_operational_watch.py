"""CPU-only monitoring amendment tests; no AWS, Kubernetes, SSH or GPU calls."""
import copy
from datetime import datetime,timedelta
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from operational_watch import (decision,GIB,stamp,enter_cleanup,cleanup_phase,cleanup_policy,
    completion_proof,validate_current_ledger,ledger_sample,observe,observe_cleanup)
from ieee_v2_observe import trainer_log_skip_reason,Observer,save_trainer_log_frame
p=Path(__file__).resolve().parents[1]/'experiments/professor-feedback-20261119/frozen/tools/test_operational_watch.py'
s=importlib.util.spec_from_file_location('original_watch_tests',p);old=importlib.util.module_from_spec(s);s.loader.exec_module(old)
WatchTests=old.WatchTests  # all six original authorization/safety boundary checks

class CleanupTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup);self.d=Path(self.tmp.name)
        self.now=stamp('2026-11-20T01:56:00Z')
        self.run=dict(run_id='fixture',policy_uid='old-uid',namespace='eval-v3-fixture',frozen_manifest_sha256='a'*64,
                      started_at='2026-11-20T00:00:00Z',goal_steps=4800,world_size=4,policy='p',max_expected_worker_instances=4)
        self.owned={'i-00000000000000001':dict(policy_uid='old-uid',nodeprovision_uid='np-uid',nodeprovision_namespace=self.run['namespace'])}
        self.proof=dict(at='2026-11-20T01:55:00Z',rank_steps={str(r):4800 for r in range(4)},completed_ranks=list(range(4)),audits=[
            dict(rank=r,passed=True,event='completion_audit',run_id='fixture',global_step=4800,world_size=4,timestamp='2026-11-20T01:54:42Z',
                 pod_uid='pod'+str(r),attempt_id='attempt'+str(r),model_sha256='a'*64,optimizer_sha256='b'*64,data_contract_sha256='c'*64) for r in range(4)])
        self.write('instance-ownership.json',self.owned);self.write('formal-completion-observed.json',self.proof)
        self.row=dict(next(iter(self.owned.values())),instance_id=next(iter(self.owned)),state='shutting-down',
                      provider_ownership_verified=True,observed_at=self.now.isoformat(),launch_time=self.run['started_at'])
        self.write('price-history/1.json',dict(observedAt=self.now.isoformat(),spotPricePerHour=.6,onDemandPricePerHour=1.4903))
    def write(self,name,value):
        p=self.d/name;p.parent.mkdir(exist_ok=True,parents=True);p.write_text(json.dumps(value))
    def enter(self):return enter_cleanup(self.d,self.run,self.now)
    def test_verified_completion_accepts_deleted_source(self):
        self.enter();self.assertIsNone(cleanup_policy(None,self.run,cleanup_phase(self.d,self.run,self.now)))
    def test_unbound_old_cleanup_request_keeps_training_gate(self):
        self.write('cleanup-requested.json',dict(state='REQUESTED_NOT_VERIFIED'))
        self.assertIsNone(cleanup_phase(self.d,self.run,self.now))
        with self.assertRaises(ValueError):cleanup_policy(None,self.run,None)
    def test_receipt_cannot_be_replaced(self):
        self.enter()
        with self.assertRaises(FileExistsError):self.enter()
    def test_equivalent_observer_and_cleanup_json_encodings_keep_phase_valid(self):
        receipt=self.enter()
        self.assertEqual(receipt['schema'],'success-cleanup-phase-v2')
        path=self.d/'instance-ownership.json'
        for sort in (False,True):
            for indent in (None,2,4):
                for newline in ('','\n'):
                    path.write_text(json.dumps(self.owned,sort_keys=sort,indent=indent)+newline)
                    self.assertEqual(cleanup_phase(self.d,self.run,self.now),receipt)
    def test_equivalent_format_does_not_allow_changed_ownership(self):
        self.enter()
        changed=copy.deepcopy(self.owned)
        changed[next(iter(changed))]['nodeprovision_uid']='foreign'
        self.write('instance-ownership.json',changed)
        with self.assertRaises(ValueError):cleanup_phase(self.d,self.run,self.now)
    def test_legacy_byte_bound_receipt_remains_strict_and_unchanged(self):
        import hashlib
        receipt=self.enter()
        receipt['schema']='success-cleanup-phase-v1'
        path=self.d/'instance-ownership.json'
        receipt['ownership_sha256']=hashlib.sha256(path.read_bytes()).hexdigest()
        self.write('cleanup-phase.json',receipt)
        self.assertEqual(cleanup_phase(self.d,self.run,self.now),receipt)
        path.write_text(path.read_text()+'\n')
        with self.assertRaises(ValueError):cleanup_phase(self.d,self.run,self.now)
    def test_recreated_source_rejected(self):
        with self.assertRaises(ValueError):cleanup_policy({'metadata':{'uid':'foreign'}},self.run,True)
    def test_receipt_or_bound_inputs_tampering_fails(self):
        self.enter();r=json.loads((self.d/'cleanup-phase.json').read_text())
        for k,v in [('policy_uid','new'),('run_id','other'),('ownership_sha256','x'),('at','2026-11-21T00:00:00Z')]:
            self.write('cleanup-phase.json',dict(r,**{k:v}))
            with self.assertRaises(ValueError):cleanup_phase(self.d,self.run,self.now)
        self.write('cleanup-phase.json',r);self.write('instance-ownership.json',{})
        with self.assertRaises(ValueError):cleanup_phase(self.d,self.run,self.now)
    def test_incomplete_failed_or_inconsistent_audits_rejected(self):
        for change in [dict(rank_steps={'0':4800}),dict(completed_ranks=[0,1,2])]:
            with self.assertRaises(ValueError):completion_proof(self.run,dict(self.proof,**change),self.now)
        for field,value in [('passed',False),('model_sha256','d'*64),('run_id','other')]:
            x=copy.deepcopy(self.proof);x['audits'][0][field]=value
            with self.assertRaises(ValueError):completion_proof(self.run,x,self.now)
    def test_current_ledger_rejects_unknown_stale_foreign_missing_duplicates(self):
        for rows in [[],[self.row,self.row],[dict(self.row,state='NOT_OBSERVED')],[dict(self.row,provider_ownership_verified=False)],
                     [dict(self.row,nodeprovision_uid='foreign')],[dict(self.row,observed_at=(self.now-timedelta(seconds=91)).isoformat())]]:
            with self.assertRaises(ValueError):validate_current_ledger(self.run,self.owned,rows,self.now)
    def test_shutting_down_still_costs_money(self):
        x=ledger_sample(self.d,self.run,self.now,{},[self.row]);self.assertEqual(x['nonterminal_owned_instances'],1)
        self.assertGreater(x['estimated_GPU_and_root_cost_usd'],2)
    def test_stale_price_still_fails_during_cleanup(self):
        self.write('price-history/1.json',dict(observedAt='2026-11-20T00:00:00Z',spotPricePerHour=.6,onDemandPricePerHour=1.4903))
        with self.assertRaises(ValueError):ledger_sample(self.d,self.run,self.now,{},[self.row])
    def test_cleanup_sensor_works_without_training_snapshot_or_live_source(self):
        self.enter();self.run.update(kubeconfigs={},aws_profile='fixture')
        with patch('integrated_control.Control') as ctl,patch('operational_watch.storage_sample',return_value={'local_free_bytes':180*GIB}),patch('operational_watch.datetime',wraps=datetime) as dt:
            dt.now.return_value=self.now;ctl.return_value.get.return_value=None;ctl.return_value.cloud.observe.return_value=[self.row]
            x=observe_cleanup(self.d,self.run,'unused',self.now)
        self.assertEqual(x['phase'],'cleanup');self.assertEqual(x['nonterminal_owned_instances'],1)
    def control_fixture(self):
        from integrated_control import Control
        from unittest.mock import Mock
        c=Control.__new__(Control);c.d=self.d;c.run=dict(self.run,app='trainer')
        c.owned=Mock(return_value=[]);c.kub=Mock();c.delete_owned=Mock()
        c.get=Mock(side_effect=lambda cluster,kind,name: {'metadata':{'uid':self.run['policy_uid']}} if kind=='noderequest' else None)
        return c
    def test_normal_cleanup_binds_phase_before_first_mutation(self):
        c=self.control_fixture()
        def mutate(*args,**kwargs):self.assertIsNotNone(cleanup_phase(self.d,self.run,self.now))
        # Use the actual wall clock for binding, while all cloud actions are mocks.
        with patch('integrated_control.datetime',wraps=datetime) as dt:
            dt.now.return_value=self.now;c.kub.side_effect=mutate;c.quiesce()
        self.assertTrue((self.d/'cleanup-phase.json').exists());c.delete_owned.assert_called_once()
    def test_invalid_completion_keeps_abort_cleanup_available_and_training_strict(self):
        self.proof['audits'][0]['passed']=False;self.write('formal-completion-observed.json',self.proof)
        c=self.control_fixture();c.quiesce()
        self.assertFalse((self.d/'cleanup-phase.json').exists());self.assertTrue((self.d/'cleanup-phase-rejected.json').exists())
        c.delete_owned.assert_called_once()
    def test_training_stale_heartbeat_still_fails(self):
        self.write('snapshots/old/manifest.json',dict(observed_at=(self.now-timedelta(seconds=91)).isoformat()))
        self.write('snapshots/old/aws-pods.json',dict(items=[]))
        with patch('operational_watch.storage_sample',return_value={}):
            with self.assertRaisesRegex(ValueError,'heartbeat stale'):observe(self.d,self.run,'unused',self.now)
    def test_running_log_error_is_still_essential(self):
        self.write('snapshots/current/manifest.json',dict(observed_at=self.now.isoformat(),errors=[dict(error='trainer_logs_unavailable',pod_uid='active')]))
        self.write('snapshots/current/aws-pods.json',dict(items=[dict(metadata=dict(uid='active'),status=dict(phase='Running'))]))
        with patch('operational_watch.storage_sample',return_value={}):
            with self.assertRaisesRegex(ValueError,'essential'):observe(self.d,self.run,'unused',self.now)

class ObserverTests(unittest.TestCase):
    def test_running_and_unknown_still_collect(self):
        for phase in ['Running','Unknown','Succeeded','Failed',None]:self.assertIsNone(trainer_log_skip_reason(dict(metadata={},status=dict(phase=phase))))
    def test_deleting_pending_terminal_skip_explicitly(self):
        self.assertEqual(trainer_log_skip_reason(dict(metadata=dict(deletionTimestamp='now'),status=dict(phase='Running'))),'deleting')
        for phase in ['Pending']:self.assertEqual(trainer_log_skip_reason(dict(metadata={},status=dict(phase=phase))),'phase-'+phase)
    def test_full_captured_stdout_frame_roundtrip_preserves_duplicate_lines(self):
        import gzip,hashlib
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);text='UTC 원시 기록\nUTC duplicate\nUTC duplicate\n'
            r=save_trainer_log_frame(d,'pod-uid',text);p=d/r['path']
            self.assertEqual(gzip.decompress(p.read_bytes()),text.encode())
            self.assertEqual(r['captured_utf8_sha256'],hashlib.sha256(text.encode()).hexdigest())
            self.assertEqual(p.stat().st_mode&0o777,0o600)
            with self.assertRaises(FileExistsError):save_trainer_log_frame(d,'pod-uid',text)
            with self.assertRaises(ValueError):save_trainer_log_frame(d,'../../escape',text)
    def test_capture_skips_retired_rpc_but_keeps_resource_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);info=dict(protocol='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118',arm='A',world_size=4,namespace='eval-v3-fixture',run_id='fixture',workload_lock_sha256='x')
            (d/'run.json').write_text(json.dumps(info));o=Observer(d,{'aws':'fixture'})
            pod=dict(metadata=dict(uid='retired',labels={'training.dcnlab.com/evaluation-run':'fixture'},deletionTimestamp='now'))
            def command(cluster,*args):
                if args[0]=='logs':raise AssertionError('retired log RPC')
                return json.dumps(dict(items=[pod] if args[1]=='pods' else []))
            o.command=command;x=o.capture();m=json.loads((Path(x['snapshot'])/'manifest.json').read_text())
            self.assertEqual(m['trainer_logs_skipped'],[dict(pod_uid='retired',reason='deleting')]);self.assertTrue((Path(x['snapshot'])/'aws-pods.json').exists())

if __name__=='__main__':unittest.main()
