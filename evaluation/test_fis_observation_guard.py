"""No cloud/GPU calls: positive loss proof and negative safety boundaries."""
import copy
from datetime import datetime, timezone, timedelta
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from fis_observation_guard import expected_lost_pod_uids
from operational_watch import observe, ledger_sample, validate_current_ledger


class FaultObservationTests(unittest.TestCase):
    def setUp(self):
        self.now=datetime(2026,11,22,5,4,tzinfo=timezone.utc)
        self.target='i-00000000000000001'
        self.run=dict(protocol='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118',purpose='formal',
            arm='C',world_size=4,scheduled_fis=True,paid_authorization=True,run_id='fixture',
            namespace='eval-v3-fixture',app='train',policy_uid='policy',workload_uid='world',
            frozen_manifest_sha256='a'*64,hard_deadline_utc='2026-11-22T09:00:00Z',
            cleanup_start_utc='2026-11-22T08:40:00Z',started_at='2026-11-22T04:00:00Z',
            approved_budget_usd=75,region='ap-northeast-2',expected_aws_account='000000000000',
            fis_target_instance_id=self.target,aws_profile='fixture',max_expected_worker_instances=7)
        self.run.update(comparison_lock_sha256='b'*64,live_spec_sha256='c'*64)
        self.auth=dict(authorized=True,approved_scope=dict(fis=True),run_id='fixture',
            frozen_manifest_sha256='a'*64,hard_deadline_utc=self.run['hard_deadline_utc'],
            budget_usd=75,researcher_message_reference='explicit fixture approval')
        self.scope=dict(run_id='fixture',policy_uid='policy',workload_uid='world',
            instance_id=self.target,nodeprovision_uid='np0',source_pods={str(i):'pod'+str(i) for i in range(4)})
        self.scope.update(comparison_lock_sha256='b'*64,live_spec_sha256='c'*64)
        self.plan=dict(scope=self.scope)
        self.started=dict(experiment=dict(id='EXP-fixture'))
        self.experiment=dict(id='EXP-fixture',state=dict(status='completed'),startTime='2026-11-22T05:01:00Z',
            tags={'evaluation-run':'fixture','policy-uid':'policy'},
            targets=dict(one=dict(resourceArns=['arn:aws:ec2:ap-northeast-2:000000000000:instance/'+self.target],resourceType='aws:ec2:spot-instance')),
            actions=dict(interrupt=dict(actionId='aws:ec2:send-spot-instance-interruptions',parameters=dict(durationBeforeInterruption='PT2M'),state=dict(status='completed'))))
        self.owned={};self.rows=[];self.nps=[];self.pods=[]
        for i in range(4):
            iid='i-'+format(i+1,'017x');owner=dict(nodeprovision_uid='np'+str(i),nodeprovision_namespace=self.run['namespace'],policy_uid='policy',market='Spot' if i==0 else 'OnDemand')
            self.owned[iid]=owner
            self.rows.append(dict(owner,instance_id=iid,state='terminated' if i==0 else 'running',instance_lifecycle='spot' if i==0 else 'on-demand',provider_ownership_verified=True,observed_at=self.now.isoformat(),launch_time=self.run['started_at']))
            self.nps.append(dict(metadata=dict(uid='np'+str(i),labels={'training.dcnlab.com/policy-uid':'policy'}),status=dict(instanceId=iid,nodeName='node'+str(i))))
            self.pods.append(dict(metadata=dict(uid='pod'+str(i),name='train-'+str(i),namespace=self.run['namespace']),spec=dict(nodeName='node'+str(i)),status=dict(phase='Running')))

    def allowed(self):
        return expected_lost_pod_uids(self.run,self.auth,self.plan,self.started,self.experiment,
                                     self.pods,self.nps,self.owned,self.rows,self.now)

    def test_only_original_dead_target_allowed(self):
        self.assertEqual(self.allowed(),{'pod0'})
    def test_running_target_is_never_excused(self):
        self.rows[0]['state']='running'
        with self.assertRaises(ValueError):self.allowed()
    def test_missing_or_unverified_provider_never_excused(self):
        for field,value in [('state','NOT_OBSERVED'),('provider_ownership_verified',False),('nodeprovision_uid','foreign'),('instance_lifecycle','on-demand'),('observed_at',(self.now-timedelta(seconds=91)).isoformat())]:
            original=copy.deepcopy(self.rows);self.rows[0][field]=value
            with self.assertRaises(ValueError):self.allowed()
            self.rows=original
    def test_other_or_recreated_pod_stays_strict(self):
        self.pods[0]['metadata']['uid']='new-pod'
        with self.assertRaises(ValueError):self.allowed()
    def test_wrong_run_source_or_provider_rejected(self):
        for key in ['run_id','policy_uid','workload_uid','nodeprovision_uid','instance_id','comparison_lock_sha256','live_spec_sha256']:
            old=self.scope[key];self.scope[key]='foreign'
            with self.assertRaises(ValueError):self.allowed()
            self.scope[key]=old
    def test_no_authorization_for_b_or_other_manifest(self):
        for obj,key,value in [(self.run,'arm','B'),(self.auth,'authorized',False),(self.auth,'frozen_manifest_sha256','b'*64)]:
            old=obj[key];obj[key]=value
            with self.assertRaises(ValueError):self.allowed()
            obj[key]=old
    def test_unconfirmed_or_foreign_fis_fails(self):
        original=copy.deepcopy(self.experiment)
        for mutation in ['id','tag','action','extra_target','not_complete','duration','future_time']:
            self.experiment=copy.deepcopy(original)
            if mutation=='id':self.experiment['id']='EXP-other'
            elif mutation=='tag':self.experiment['tags']['policy-uid']='other'
            elif mutation=='action':self.experiment['actions']['interrupt']['actionId']='aws:ec2:terminate-instances'
            elif mutation=='extra_target':self.experiment['targets']['two']=self.experiment['targets']['one']
            elif mutation=='duration':self.experiment['actions']['interrupt']['parameters']['durationBeforeInterruption']='PT15M'
            elif mutation=='future_time':self.experiment['startTime']=(self.now+timedelta(seconds=1)).isoformat()
            else:self.experiment['actions']['interrupt']['state']['status']='running'
            with self.assertRaises(ValueError):self.allowed()
    def test_after_cleanup_deadline_is_not_a_fault_allowance(self):
        self.now=datetime(2026,11,22,8,40,tzinfo=timezone.utc)
        with self.assertRaises(ValueError):self.allowed()

    def files(self,d,errors):
        def write(n,v):
            p=d/n;p.parent.mkdir(exist_ok=True,parents=True);p.write_text(json.dumps(v))
        write('snapshots/current/manifest.json',dict(observed_at=self.now.isoformat(),errors=errors))
        write('snapshots/current/aws-pods.json',dict(items=self.pods))
        write('snapshots/current/aws-nodeprovisions.json',dict(items=self.nps))
        write('instance-ownership.json',self.owned)
        write('authorization.json',self.auth);write('fis-final-plan.json',self.plan);write('fis-started.json',self.started)
        write('price-history/1.json',dict(observedAt=self.now.isoformat(),spotPricePerHour=.6,onDemandPricePerHour=1.49))
        return write

    def observe_with_fake_cloud(self,d):
        with patch('operational_watch.storage_sample',return_value={}),patch('ieee_v2_ledger.AWSReader') as cls,patch('operational_watch.datetime',wraps=datetime) as dt:
            dt.now.return_value=self.now;cls.return_value.observe.return_value=self.rows
            cls.return_value.command.return_value=dict(experiment=self.experiment)
            return observe(d,self.run,'unused',self.now)
    def test_expected_loss_preserves_cost_and_other_sensors(self):
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);self.files(d,[dict(error='trainer_logs_unavailable',pod_uid='pod0')])
            result=self.observe_with_fake_cloud(d)
            self.assertEqual(result['nonterminal_owned_instances'],3)
            self.assertGreater(result['estimated_GPU_and_root_cost_usd'],5)
            self.assertEqual(result['expected_FIS_target_log_unavailable'][0]['pod_uid'],'pod0')
            self.assertTrue((d/'training-watch-ledger.jsonl').exists())
    def test_other_running_rank_missing_log_still_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);self.files(d,[dict(error='trainer_logs_unavailable',pod_uid='pod1')])
            with self.assertRaisesRegex(ValueError,'non-target'):self.observe_with_fake_cloud(d)
    def test_new_registered_vm_missing_old_snapshot_is_counted(self):
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);write=self.files(d,[])
            write('snapshots/current/aws-instance-ledger.json',self.rows)
            owner=dict(self.owned[self.target],nodeprovision_uid='new',market='OnDemand')
            iid='i-00000000000000005';self.owned[iid]=owner;write('instance-ownership.json',self.owned)
            self.rows.append(dict(owner,instance_id=iid,state='pending',provider_ownership_verified=True,observed_at=self.now.isoformat(),launch_time=self.now.isoformat()))
            self.assertEqual(self.observe_with_fake_cloud(d)['nonterminal_owned_instances'],4)
    def test_unknown_provider_still_fails_and_is_not_zero_cost(self):
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);self.files(d,[]);self.rows[0]['state']='NOT_OBSERVED'
            with self.assertRaises(ValueError):self.observe_with_fake_cloud(d)
    def test_unrelated_collection_failure_and_stale_heartbeat_still_fail(self):
        for error in [dict(operation='exact_instance_ledger',error='unavailable'),dict(cluster='karmada',kind='pods',error='unavailable')]:
            with tempfile.TemporaryDirectory() as temp:
                d=Path(temp);self.files(d,[error])
                with self.assertRaisesRegex(ValueError,'essential'):self.observe_with_fake_cloud(d)
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);write=self.files(d,[])
            write('snapshots/current/manifest.json',dict(observed_at=(self.now-timedelta(seconds=91)).isoformat(),errors=[]))
            with self.assertRaisesRegex(ValueError,'heartbeat'):self.observe_with_fake_cloud(d)
    def test_later_history_cannot_corrupt_earlier_bound_ownership(self):
        with tempfile.TemporaryDirectory() as temp:
            d=Path(temp);write=self.files(d,[]);cutoff=self.now
            future=dict(self.rows[0],instance_id='i-00000000000000009',nodeprovision_uid='later',observed_at=(self.now+timedelta(seconds=1)).isoformat())
            write('snapshots/current/aws-instance-ledger.json',self.rows+[future])
            out=ledger_sample(d,self.run,self.now,{},self.rows,owned_override=self.owned,history_cutoff=cutoff)
            self.assertEqual(out['nonterminal_owned_instances'],3)
            future['observed_at']=self.now.isoformat();write('snapshots/current/aws-instance-ledger.json',self.rows+[future])
            with self.assertRaisesRegex(ValueError,'UID'):ledger_sample(d,self.run,self.now,{},self.rows,owned_override=self.owned,history_cutoff=cutoff)


if __name__=='__main__':unittest.main()
