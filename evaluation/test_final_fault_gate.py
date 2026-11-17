import unittest
from final_fault_gate import check as real_check

def check(run,events,**kwargs):
    kwargs.setdefault("now","2026-11-17T09:00:03Z")
    kwargs.setdefault("current_pods",{0:"pod0",1:"pod1"})
    return real_check(run,events,**kwargs)

class GateTests(unittest.TestCase):
    def setUp(self):
        self.run={'protocol':'ieee-access-final-20261117','arm':'E2-B','run_id':'run','goal_steps':100,'fault_trigger':{'type':'committed_global_step','value':50},'policy_uid':'p','workload_uid':'w','started_at':'2026-11-17T09:00:00Z','hard_deadline_utc':'2026-11-17T13:00:00Z'}
    def event(self,r,step,at='01',event='step'):
        return dict(run_id='run',rank=r,world_size=2,global_step=step,timestamp='2026-11-17T09:00:'+at+'Z',event=event,pod_uid='pod'+str(r),attempt_id='attempt')
    def test_both_rank_step_and_single_fault_limit(self):
        events=[self.event(0,51),self.event(1,50)]
        self.assertTrue(check(self.run,events)['ready']);self.assertFalse(check(self.run,events[:1])['ready'])
        self.assertFalse(check(self.run,events,already_started=True)['ready'])
        self.assertFalse(check(self.run,[self.event(0,49),self.event(1,90)])['ready'])
    def test_rollback_latest_sample_and_completion_are_not_overridden_by_old_max(self):
        events=[self.event(0,70),self.event(1,70),self.event(1,40,'02')]
        self.assertFalse(check(self.run,events)['ready'])
        self.assertFalse(check(self.run,[self.event(0,100,event='completed'),self.event(1,70)])['ready'])
    def test_wrong_run_and_expired_deadline_are_rejected(self):
        with self.assertRaises(ValueError):check(self.run,[dict(self.event(0,50),run_id='other')])
        self.assertFalse(check(self.run,[self.event(0,50),self.event(1,50)],now='2026-11-17T13:00:00Z')['ready'])

    def test_old_pod_and_stale_progress_do_not_authorize_a_fault(self):
        events=[self.event(0,70),self.event(1,70)]
        self.assertFalse(check(self.run,events,now='2026-11-17T09:01:00Z')['ready'])
        self.assertFalse(check(self.run,events,current_pods={0:'replacement0',1:'pod1'})['ready'])
        with self.assertRaises(ValueError):check(self.run,events,current_pods={0:'same',1:'same'})

    def test_legacy_executor_explicitly_rejects_final_protocol_before_cloud_access(self):
        import fis
        from unittest.mock import patch
        from pathlib import Path
        with patch.object(fis.runner,'read',return_value=self.run),patch.object(fis,'aws') as cloud:
            with self.assertRaisesRegex(ValueError,'legacy fixed-namespace'):
                fis.plan(Path('/not-used'),'arn:role','arn:alarm')
            cloud.assert_not_called()
