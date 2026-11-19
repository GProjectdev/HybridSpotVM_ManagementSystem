import unittest
from final_fis import risk_window_deadline


class NativeFISWindowTests(unittest.TestCase):
    def setUp(self):
        self.run={'purpose':'formal','world_size':4,'fault_window':{
            'anchor':'source NodeRequest creationTimestamp',
            'transition_after_seconds':3600,'notice_seconds':120,'margin_seconds':120}}

    def test_live_source_creation_bounds_dispatch_before_notice_and_margin(self):
        self.assertEqual(risk_window_deadline(self.run,'2026-11-19T00:00:00Z',
            '2026-11-19T00:55:59Z'),'2026-11-19T00:56:00+00:00')
        for now in ('2026-11-19T00:56:00Z','2026-11-19T01:00:00Z'):
            with self.subTest(now=now),self.assertRaisesRegex(ValueError,'overlaps'):
                risk_window_deadline(self.run,'2026-11-19T00:00:00Z',now)

    def test_missing_contract_and_future_policy_fail_closed(self):
        with self.assertRaisesRegex(ValueError,'contract'):
            risk_window_deadline(dict(self.run,fault_window={}),None,'2026-11-19T00:00:00Z')
        with self.assertRaisesRegex(ValueError,'future'):
            risk_window_deadline(self.run,'2026-11-19T00:01:00Z','2026-11-19T00:00:00Z')

    def test_historical_acceptance_and_two_rank_contract_unchanged(self):
        self.assertIsNone(risk_window_deadline(dict(self.run,purpose='acceptance'),None,None))
        self.assertIsNone(risk_window_deadline(dict(self.run,world_size=2),None,None))
