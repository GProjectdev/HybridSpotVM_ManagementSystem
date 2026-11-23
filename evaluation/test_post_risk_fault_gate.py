import copy
import tempfile
import unittest
from pathlib import Path

import post_risk_fault_gate as gate

NOW = '2026-11-23T01:10:00Z'
L = gate.LABEL
CID = 'periodic-'+__import__('hashlib').sha256(b'parent').hexdigest()[:8]+'-g2-t'+str(int(gate.stamp('2026-11-23T01:04:00Z').timestamp()*1e9))


def fixture(model='gpt2'):
    goal, saved, current, fast, slow = ((4800, 3200, 3300, .7, 1.6) if model=='gpt2'
                                      else (26700, 20000, 20500, .14, .20))
    run = dict(protocol='HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118',arm='B',world_size=4,
        model=model,goal_steps=goal,run_id='synthetic-l',namespace='eval-v3-synthetic-l',
        policy_uid='policy',workload_uid='workload',risk_lock_sha256='a'*64,
        source_created_at='2026-11-23T00:00:00Z',hard_deadline_utc='2026-11-23T05:00:00Z',
        cleanup_start_utc='2026-11-23T04:40:00Z',post_risk_gate=dict(
            revision=gate.REVISION,start_hour=9,update_hour=10,refresh_seconds=3600,
            risk_lock_sha256='a'*64,updated_lambda_per_hour=.024690337473306213,
            trace_sha256='c'*64,forecast_sha256='d'*64,
            notice_seconds=120,notice_margin_seconds=60,recovery_reserve_seconds=1800,
            additional_operations_reserve_seconds=600,cleanup_reserve_seconds=1200,
            fast_seconds_per_step=fast,slow_seconds_per_step=slow,max_progress_age_seconds=30,
            max_observation_age_seconds=90,nfs_reserve_bytes=64*1024**3,
            next_checkpoint_reserve_bytes=32*1024**3,local_reserve_bytes=80*1024**3,
            budget_usd=75,target_selection='lowest-rank-owned-spot',first_eligible_only=True))
    pods = {i:'pod-'+str(i) for i in range(4)}
    parent = dict(metadata=dict(name='periodic',namespace=run['namespace'],uid='parent',generation=2,
        labels={L+'policy-uid':'policy'}),spec=dict(workloadRef=dict(uid='workload'),
        schedule=dict(enabled=True,intervalSeconds=3358)),status=dict(observedGeneration=2))
    cp = dict(metadata=dict(name='periodic-child',namespace=run['namespace'],uid='child',generation=1,
        creationTimestamp='2026-11-23T01:04:00Z',labels={L+'scheduled-parent':'parent'},
        annotations={L+'scheduled-parent-name':'periodic',L+'scheduled-parent-uid':'parent',L+'checkpoint-id':CID}),
        spec=dict(workloadRef=dict(uid='workload'),resume=True),status=dict(phase='Completed',observedGeneration=1,
        startTime='2026-11-23T01:04:01Z',completionTime='2026-11-23T01:06:00Z',pods=[]))
    events = []; workers = []
    for rank in range(4):
        h = str(rank+1)*64
        cp['status']['pods'].append(dict(rank=rank,podUID=pods[rank],checkpointID=CID,phase='Resumed',
            checkpointFiles=[dict(sha256=h,durableRef='file-store:'+run['namespace']+'/sha256/'+h,
                containerName='trainer',checkpointID=CID,exportedAt='2026-11-23T01:07:00Z')]))
        for event,step,at in [('checkpoint_saved',saved,'2026-11-23T01:05:00Z'),
                              ('training_resumed',saved,'2026-11-23T01:06:01Z'),
                              ('step',current,'2026-11-23T01:09:58Z')]:
            events.append(dict(run_id=run['run_id'],world_size=4,rank=rank,pod_uid=pods[rank],
                attempt_id='attempt-1',event=event,global_step=step,timestamp=at,checkpoint_id=CID))
        workers.append(dict(rank=rank,pod_uid=pods[rank],instance_id='i-'+str(rank),nodeprovision_uid='np-'+str(rank),
            policy_uid='policy',market='OnDemand' if rank==0 else 'Spot',state='running',ownership_verified=True))
    observation=dict(run_id=run['run_id'],policy_uid='policy',workload_uid='workload',captured_at=NOW,
        safety=dict(observed_at=NOW,stop_requested=False,nfs_free_bytes=200*1024**3,local_free_bytes=150*1024**3,
            estimated_cost_usd=5,cleanup_cost_reserve_usd=2),current_pods=pods,training_events=events,
        risk_application=dict(policy_uid='policy',workload_uid='workload',risk_lock_sha256='a'*64,source_hour=10,
            evidence_sha256='b'*64,applied_at='2026-11-23T01:00:10Z',lambda_per_hour=.024690337473306213,
            schedule_uid='parent',schedule_generation=2,interval_seconds=3358),
        schedule=parent,checkpoint=cp,active_operation_uids=[],workers=workers)
    return run,observation


class PostRiskTests(unittest.TestCase):
    def test_old_reservation_materialized_after_risk_is_rejected(self):
        r,o=fixture()
        o['checkpoint']['metadata']['annotations'][L+'checkpoint-id']=CID.replace('-g2-','-g1-')
        self.assertEqual(gate.check(r,o,NOW)['reason'],'checkpoint_reservation_generation_invalid')

    def test_two_models_and_B_C_use_same_gate(self):
        for model in ('gpt2','resnet50'):
            run,o=fixture(model);b=gate.check(run,o,NOW);run['arm']='C';c=gate.check(run,o,NOW)
            self.assertEqual(b,c);self.assertTrue(c['ready']);self.assertEqual(c['target_rank'],1)

    def test_historical_step_or_time_gate_cannot_be_silently_disabled(self):
        for key,value in [('fault_trigger',{'type':'committed_global_step','value':2400}),('fault_window',{})]:
            run,o=fixture();run[key]=value
            with self.assertRaisesRegex(ValueError,'historical'):gate.check(run,o,NOW)

    def test_no_risk_application_even_past_half_steps_is_not_eligible(self):
        run,o=fixture();o.pop('risk_application')
        self.assertEqual(gate.check(run,o,NOW)['reason'],'waiting_risk_application')

    def test_old_checkpoint_and_unacknowledged_parent_wait(self):
        run,o=fixture();o['checkpoint']['metadata']['creationTimestamp']='2026-11-23T00:10:00Z'
        self.assertEqual(gate.check(run,o,NOW)['reason'],'waiting_checkpoint_created_after_risk')
        run,o=fixture();o['schedule']['status']['observedGeneration']=1
        self.assertEqual(gate.check(run,o,NOW)['reason'],'schedule_not_matching_applied_risk')

    def test_partial_or_foreign_or_planned_child_never_passes(self):
        for change in ('partial','uid','planned'):
            run,o=fixture()
            if change=='partial':o['checkpoint']['spec']['partialCheckpoint']={'ranks':[1]}
            if change=='uid':o['checkpoint']['spec']['workloadRef']['uid']='other'
            if change=='planned':o['checkpoint']['metadata']['labels'].clear()
            self.assertFalse(gate.check(run,o,NOW)['ready'])

    def test_complete_is_not_durable(self):
        run,o=fixture();o['checkpoint']['status']['pods'][2]['checkpointFiles'][0].pop('exportedAt')
        self.assertEqual(gate.check(run,o,NOW)['reason'],'waiting_valid_durable_receipts')

    def test_content_binding_and_export_time_checked(self):
        for field,value in [('durableRef','file-store:foreign/sha256/'+'3'*64),('sha256','bad'),
                             ('exportedAt','2026-11-23T01:11:00Z')]:
            run,o=fixture();o['checkpoint']['status']['pods'][2]['checkpointFiles'][0][field]=value
            self.assertFalse(gate.check(run,o,NOW)['ready'])

    def test_saved_common_step_and_source_world_required(self):
        run,o=fixture();o['training_events'][0]['global_step']+=1
        self.assertEqual(gate.check(run,o,NOW)['reason'],'waiting_common_saved_step')
        run,o=fixture();o['checkpoint']['status']['pods'][0]['podUID']='retired'
        self.assertFalse(gate.check(run,o,NOW)['ready'])

    def test_training_progress_after_durable_not_just_resumed(self):
        run,o=fixture();o['training_events'][2]['timestamp']='2026-11-23T01:06:30Z'
        # At an earlier now the archive is still future; at current now progress is stale.
        self.assertFalse(gate.check(run,o,NOW)['ready'])

    def test_latest_rollback_not_historical_maximum_used(self):
        run,o=fixture();e=copy.deepcopy(o['training_events'][2]);e.update(timestamp='2026-11-23T01:09:59Z',global_step=3190)
        o['training_events'].append(e)
        self.assertEqual(gate.check(run,o,NOW)['reason'],'waiting_all_rank_post_durable_progress')

    def test_goal_completion_is_terminal_not_retimed(self):
        run,o=fixture();o['training_events'][2]['global_step']=4800
        r=gate.check(run,o,NOW);self.assertTrue(r['terminal']);self.assertFalse(r['ready'])

    def test_notice_margin_blocks_near_goal(self):
        run,o=fixture();o['training_events'][2]['global_step']=4790
        r=gate.check(run,o,NOW);self.assertEqual(r['reason'],'goal_may_finish_during_notice');self.assertTrue(r['eligible'])

    def test_no_spot_or_competing_restore_is_terminal_at_first_candidate(self):
        for op in ('noSpot','restore'):
            run,o=fixture()
            if op=='noSpot':
                for w in o['workers']:w['market']='OnDemand'
            else:o['active_operation_uids']=['rr-uid']
            r=gate.check(run,o,NOW);self.assertTrue(r['terminal']);self.assertTrue(r['eligible']);self.assertFalse(r['ready'])

    def test_budget_storage_stale_future_guards(self):
        for key,value in [('nfs_free_bytes',65*1024**3),('local_free_bytes',79*1024**3),
                          ('estimated_cost_usd',74),('estimated_cost_usd',float('nan')),
                          ('stop_requested',True),('observed_at','2026-11-23T00:00:00Z')]:
            run,o=fixture();o['safety'][key]=value
            r=gate.check(run,o,NOW);self.assertFalse(r['ready']);self.assertTrue(r['terminal'])

    def test_time_reserve_not_just_thirty_minutes(self):
        run,o=fixture();run['cleanup_start_utc']='2026-11-23T01:45:00Z'
        r=gate.check(run,o,NOW);self.assertEqual(r['reason'],'insufficient_recovery_and_remaining_work_budget')

    def test_receipt_first_only_and_not_paid_authorization(self):
        run,o=fixture();r=gate.check(run,o,NOW)
        with tempfile.TemporaryDirectory() as d:
            saved=gate.record_first(d,run,r);self.assertEqual(saved['mode'],'shadow')
            self.assertFalse(saved['paid_execution_authorized_by_this_receipt'])
            with self.assertRaises(FileExistsError):gate.record_first(d,run,r)
            self.assertEqual(len(list(Path(d).iterdir())),1)

    def test_runtime_contract_cannot_relax_recovery_or_change_work(self):
        run,o=fixture();run['post_risk_gate']['recovery_reserve_seconds']=60
        with self.assertRaises(ValueError):gate.check(run,o,NOW)
        run,o=fixture();run['goal_steps']=5000
        with self.assertRaises(ValueError):gate.check(run,o,NOW)


if __name__=='__main__':unittest.main()
