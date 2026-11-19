import copy
import unittest
from management_evidence import policy_row


class ManagementEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.run = dict(policy_uid='p', workload_uid='w', run_id='r', namespace='eval-final-r')
        self.obj = {'metadata': {'uid': 'p', 'namespace': 'eval-final-r', 'generation': 2,
                    'labels': {'training.dcnlab.com/evaluation-run': 'r'}},
                    'spec': {'workloadRef': {'uid': 'w'}}, 'status': {
                        'policy': {'intervalSource': 'risk-band-bootstrap', 'checkpointIntervalSeconds': 300},
                        'checkpoint': {'intervalSource': 'checkpoint-cost-analytic', 'checkpointIntervalSeconds': 600}}}

    def test_coordinator_is_not_inferred_from_policy_branch(self):
        row = policy_row(self.obj, self.run, '2026-11-18T00:00:00Z')
        self.assertEqual(row['policy_interval_source'], 'risk-band-bootstrap')
        self.assertEqual(row['checkpoint_interval_source'], 'checkpoint-cost-analytic')
        self.assertEqual(row['checkpoint_interval_seconds'], 600)
        self.assertIsNone(row['policy_observed_at'])
        self.assertIsNone(row['lambda_per_hour'])

    def test_foreign_recreated_or_unbound_identity_is_excluded(self):
        for section, key, value in [('metadata', 'uid', 'new'), ('metadata', 'namespace', 'other')]:
            obj = copy.deepcopy(self.obj); obj[section][key] = value
            self.assertIsNone(policy_row(obj, self.run, 'observation'))
        obj = copy.deepcopy(self.obj); obj['spec']['workloadRef']['uid'] = 'stale'
        self.assertIsNone(policy_row(obj, self.run, 'observation'))

    def test_missing_checkpoint_branch_stays_missing(self):
        self.obj['status'].pop('checkpoint')
        self.assertIsNone(policy_row(self.obj, self.run, 'observation')['checkpoint_interval_source'])
