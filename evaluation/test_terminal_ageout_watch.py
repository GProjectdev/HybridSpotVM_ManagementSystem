"""CPU regression: positive terminated receipts survive EC2 visibility expiry."""
import copy
from datetime import datetime, timedelta
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from operational_watch import ledger_sample, observe, observe_cleanup, stamp, validate_current_ledger


class TerminalAgeOutTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.d = Path(self.tmp.name)
        self.now = stamp('2026-11-20T02:00:00Z')
        self.run = dict(run_id='fixture', policy_uid='policy', namespace='eval-fixture',
                        started_at='2026-11-20T00:00:00Z', max_expected_worker_instances=7,
                        aws_profile='unused', expected_aws_account='unused', region='unused')
        self.old, self.live = 'i-00000000000000001', 'i-00000000000000002'
        self.owned = {i: dict(policy_uid='policy', nodeprovision_namespace='eval-fixture',
                            nodeprovision_uid='uid-'+i, nodeprovision_name='np-'+i)
                      for i in [self.old, self.live]}
        self.prior = [dict(self.owned[i], instance_id=i, state='terminated' if i == self.old else 'running',
                          provider_ownership_verified=True, launch_time=self.run['started_at'],
                          observed_at='2026-11-20T00:30:00Z') for i in self.owned]
        self.rows = [dict(self.owned[self.old], instance_id=self.old, state='NOT_OBSERVED',
                          observed_at=self.now.isoformat()),
                     dict(self.prior[1], observed_at=self.now.isoformat())]
        self.write('instance-ownership.json', self.owned)
        self.write('snapshots/prior/manifest.json', {})
        self.write('snapshots/prior/aws-instance-ledger.json', self.prior)
        self.write('price-history/fresh.json', dict(observedAt=self.now.isoformat(),
                                                  spotPricePerHour=.6, onDemandPricePerHour=1.49))

    def write(self, name, value):
        p = self.d/name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(json.dumps(value))

    def validate(self, rows=None):
        return validate_current_ledger(self.run, self.owned, self.rows if rows is None else rows,
                                       self.now, directory=self.d)

    def test_expired_terminal_retains_cost_and_counts_live_replacement(self):
        self.assertEqual(set(self.validate()), {self.old})
        result = ledger_sample(self.d, self.run, self.now, {}, self.rows)
        self.assertEqual(result['nonterminal_owned_instances'], 1)
        self.assertAlmostEqual(result['estimated_GPU_and_root_cost_usd'], 2.5*(1.49+100*.0912/720))
        self.assertEqual(self.rows[0]['state'], 'NOT_OBSERVED')

    def test_disappearance_without_positive_receipt_is_rejected(self):
        self.prior[0]['state'] = 'shutting-down'
        self.write('snapshots/prior/aws-instance-ledger.json', self.prior)
        with self.assertRaises(ValueError): self.validate()

    def test_foreign_or_unverified_history_rejected(self):
        for field, value in [('policy_uid','other'), ('nodeprovision_uid','other'),
                             ('nodeprovision_namespace','other'), ('nodeprovision_name','other'),
                             ('provider_ownership_verified',False)]:
            with self.subTest(field=field):
                rows=copy.deepcopy(self.prior); rows[0][field]=value
                self.write('snapshots/prior/aws-instance-ledger.json',rows)
                with self.assertRaises(ValueError): self.validate()

    def test_future_receipt_cannot_be_used_in_historical_replay(self):
        self.prior[0]['observed_at'] = '2026-11-20T02:01:00Z'
        self.write('snapshots/prior/aws-instance-ledger.json', self.prior)
        with self.assertRaises(ValueError): self.validate()

    def test_current_absence_query_must_still_be_fresh(self):
        rows=copy.deepcopy(self.rows)
        rows[0]['observed_at']=(self.now-timedelta(seconds=91)).isoformat()
        with self.assertRaises(ValueError): self.validate(rows)

    def test_live_unknown_stale_foreign_or_missing_remains_rejected(self):
        for change in [dict(state='NOT_OBSERVED'), dict(provider_ownership_verified=False),
                       dict(nodeprovision_uid='other'),
                       dict(observed_at=(self.now-timedelta(seconds=91)).isoformat())]:
            with self.subTest(change=change):
                rows=copy.deepcopy(self.rows); rows[1].update(change)
                with self.assertRaises(ValueError): self.validate(rows)
        with self.assertRaises(ValueError): self.validate(self.rows[:1])

    def test_positive_history_conflicting_with_current_live_state_rejected(self):
        rows=[dict(self.prior[0], state='running', observed_at=self.now.isoformat()), self.rows[1]]
        with self.assertRaisesRegex(ValueError, 'conflicts'):
            ledger_sample(self.d,self.run,self.now,{},rows)

    def test_cleanup_still_queries_live_replacement_and_reports_retired_proof(self):
        self.run.update(policy='policy-name', kubeconfigs={})
        with patch('operational_watch.cleanup_phase', return_value={'verified': True}), \
             patch('integrated_control.Control') as control, \
             patch('operational_watch.storage_sample', return_value={}), \
             patch('operational_watch.datetime', wraps=datetime) as clock:
            clock.now.return_value=self.now
            control.return_value.get.return_value=None
            control.return_value.cloud.observe.return_value=self.rows
            result=observe_cleanup(self.d,self.run,'unused',self.now)
        self.assertEqual(result['nonterminal_owned_instances'],1)
        self.assertFalse(result['positive_EC2_termination_observed'])
        self.assertEqual(result['absent_retired_instances_with_positive_receipts'],[self.old])
        control.return_value.cloud.observe.assert_called_once()

    def test_training_sensor_uses_history_without_rewriting_current_rows(self):
        self.write('snapshots/prior/manifest.json',dict(observed_at='2026-11-20T00:30:00Z'))
        self.write('snapshots/current/manifest.json',dict(observed_at=self.now.isoformat(),errors=[]))
        self.write('snapshots/current/aws-pods.json',dict(items=[]))
        self.write('snapshots/current/aws-instance-ledger.json',self.rows)
        with patch('operational_watch.storage_sample',return_value={}), \
             patch('ieee_v2_ledger.AWSReader') as reader, \
             patch('operational_watch.datetime',wraps=datetime) as clock:
            clock.now.return_value=self.now
            reader.return_value.observe.return_value=self.rows
            result=observe(self.d,self.run,'unused',self.now)
        self.assertEqual(result['nonterminal_owned_instances'],1)
        self.assertEqual(result['absent_retired_instances_with_positive_receipts'],[self.old])
        saved=json.loads((self.d/'training-watch-ledger.jsonl').read_text())
        self.assertEqual(saved['rows'][0]['state'],'NOT_OBSERVED')
        self.assertEqual(saved['prior_termination_receipts'][self.old]['ledger_row']['observed_at'],
                         '2026-11-20T00:30:00Z')


if __name__ == '__main__':
    unittest.main()
