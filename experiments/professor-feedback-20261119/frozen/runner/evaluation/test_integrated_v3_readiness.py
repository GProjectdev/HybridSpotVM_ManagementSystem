import unittest
from evaluation.integrated_v3_readiness import compare_bc_contracts, timing_envelope


class BCReadiness(unittest.TestCase):
    def test_equal_drafts_do_not_freeze_missing_goal(self):
        draft={'world_size':4,'goal_steps':None,'forecast_sha256':'source'}
        result=compare_bc_contracts(draft,dict(draft))
        self.assertTrue(result['identical_draft_settings'])
        self.assertFalse(result['ready_to_freeze'])

    def test_changed_risk_or_model_blocks_pair(self):
        b={'world_size':4,'goal_steps':100,'risk_window':14,'model':'gpt2'}
        for field,value in [('risk_window',5),('world_size',2),('model','resnet50')]:
            c=dict(b);c[field]=value
            self.assertIn(field,compare_bc_contracts(b,c)['different_fields'])

    def test_no_pilot_means_unknown(self):
        self.assertEqual(timing_envelope(forecast_transition_seconds=3600)['status'],'UNRESOLVED')

    def test_notice_overlapping_transition_blocks(self):
        r=timing_envelope(forecast_transition_seconds=3600,fault_elapsed_seconds=3490,
                         normal_goal_elapsed_seconds=4000,durable_checkpoint_elapsed_seconds=700,
                         margin_seconds=0)
        self.assertEqual(r['status'],'CONFLICT')

    def test_synthetic_compatible_window_never_authorizes_cloud(self):
        r=timing_envelope(forecast_transition_seconds=3600,fault_elapsed_seconds=2200,
                         normal_goal_elapsed_seconds=4400,durable_checkpoint_elapsed_seconds=700,
                         margin_seconds=60)
        self.assertEqual(r['status'],'OFFLINE_WINDOW_COMPATIBLE')
        self.assertFalse(r['live_execution_authorized'])

    def test_goal_or_checkpoint_phase_conflicts_are_preserved(self):
        r=timing_envelope(forecast_transition_seconds=3600,fault_elapsed_seconds=1800,
                         normal_goal_elapsed_seconds=3500,durable_checkpoint_elapsed_seconds=1900,
                         margin_seconds=60)
        self.assertIn('normal_run_ends_before_risk_transition',r['reasons'])
        self.assertIn('durable_checkpoint_not_ready_at_declared_fault',r['reasons'])


if __name__=='__main__':unittest.main()
