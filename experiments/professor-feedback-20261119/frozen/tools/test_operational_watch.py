import copy
from datetime import datetime,timezone
import unittest
from operational_watch import authorization,decision,GIB

class WatchTests(unittest.TestCase):
    def setUp(self):
        self.now=datetime(2026,11,20,1,tzinfo=timezone.utc)
        self.run=dict(run_id='gpt2-a-01',paid_authorization=True,policy_uid='uid',frozen_manifest_sha256='a'*64,
                      started_at='2026-11-20T00:00:00Z',cleanup_start_utc='2026-11-20T04:40:00Z',hard_deadline_utc='2026-11-20T05:00:00Z',
                      approved_budget_usd=50,nfs_stop_free_GiB=80,active_paths_stop_GiB=6,max_expected_worker_instances=4,other_cost_reserve_usd=10)
        self.receipt=dict(authorized=True,run_id='gpt2-a-01',frozen_manifest_sha256='a'*64,hard_deadline_utc=self.run['hard_deadline_utc'],budget_usd=50,researcher_message_reference='fixture-not-real-approval')
        self.sample=dict(nfs_free_bytes=415*GIB,run_allocated_bytes=GIB,local_free_bytes=180*GIB,nonterminal_owned_instances=4,estimated_GPU_and_root_cost_usd=5,cleanup_compute_reserve_usd=2)
    def test_freeze_approval_is_not_paid_execution_approval(self):
        run=dict(self.run,paid_authorization=False)
        with self.assertRaises(ValueError):authorization(run,self.receipt,self.now)
        with self.assertRaises(ValueError):authorization(self.run,dict(self.receipt,authorized=False),self.now)
    def test_wrong_run_or_deadline_receipt_rejected(self):
        for change in [{'run_id':'different'},{'frozen_manifest_sha256':'b'*64},{'hard_deadline_utc':'2026-11-20T06:00:00Z'},{'budget_usd':75}]:
            with self.assertRaises(ValueError):authorization(self.run,dict(self.receipt,**change),self.now)
    def test_healthy_sample_has_no_stop(self):
        authorization(self.run,self.receipt,self.now)
        self.assertFalse(decision(self.run,self.sample,self.now,0)['stop_requested'])
    def test_second_sensor_failure_stops_but_is_not_zero_cost(self):
        first=decision(self.run,{'error':'missing_cost'},self.now,0)
        second=decision(self.run,{'error':'missing_cost'},self.now,first['consecutive_sensor_failures'])
        self.assertFalse(first['stop_requested']);self.assertTrue(second['stop_requested'])
    def test_space_and_capacity_and_budget_boundaries(self):
        for sample,reason in [(dict(self.sample,nfs_free_bytes=80*GIB),'NFS_free_headroom'),
                              (dict(self.sample,run_allocated_bytes=6*GIB),'owned_run_storage_envelope'),
                              (dict(self.sample,nonterminal_owned_instances=5),'unexpected_concurrent_owned_capacity'),
                              (dict(self.sample,estimated_GPU_and_root_cost_usd=38),'cost_estimate_plus_cleanup_reserve')]:
            self.assertIn(reason,decision(self.run,sample,self.now,0)['reasons'])
    def test_deadline_stops_even_with_failed_sensor(self):
        now=datetime(2026,11,20,4,40,tzinfo=timezone.utc)
        self.assertIn('absolute_cleanup_deadline',decision(self.run,{'error':'unavailable'},now,0)['reasons'])

if __name__=='__main__':unittest.main()
