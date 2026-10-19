import unittest
import json
import io
from types import SimpleNamespace
import collect
from audit_restore import summarize
from resnet18.train_resnet18 import Events


class AuditSummaryTests(unittest.TestCase):
    def test_real_producer_to_collector(self):
        stream = io.StringIO()
        events = Events(SimpleNamespace(rank=0, run_id='audit01', world_size=2,
                                        goal_steps=100, batch_size=32), stream=stream)
        for name in ('restore_audit_fresh', 'restore_audit', 'update_audit'):
            events.emit(name, 10, passed=True)
        parsed = [collect.event_from_message(line) for line in stream.getvalue().splitlines()]
        self.assertEqual([e['event'] for e in parsed],
                         ['restore_audit_fresh', 'restore_audit', 'update_audit'])

    def test_collector_preserves_audit_but_not_paths(self):
        event = dict(event='restore_audit', rank=0, global_step=10, run_id='audit01',
                     timestamp='2026-10-19T00:00:00Z', attempt_id='a1', passed=True,
                     checks=dict(model=True, optimizer=True, rng=True, step=True), checkpoint_path='/private')
        result = collect.event_from_message('EVAL_EVENT ' + json.dumps(event))
        self.assertTrue(result['passed'])
        self.assertEqual(result['checks'], event['checks'])
        self.assertNotIn('checkpoint_path', result)
        event['passed'] = 'true'
        with self.assertRaises(ValueError):
            collect.event_from_message('EVAL_EVENT ' + json.dumps(event))

    def rows(self):
        rows = []
        for rank in (0, 1):
            rows.append(dict(event='restore_audit', rank=rank, attempt_id=str(rank), global_step=10, passed=True,
                             checks=dict(model=True, optimizer=True, rng=True, step=True)))
            rows.extend(dict(event='update_audit', rank=rank, attempt_id=str(rank), global_step=s, passed=True,
                             gradients_finite=True, gradients_nonzero=True, parameters_changed=True)
                        for s in (11, 12, 13))
        return rows

    def test_requires_both_ranks_and_three_updates(self):
        self.assertTrue(summarize(self.rows())['rank_local_audit_passed'])
        self.assertFalse(summarize(self.rows()[:-1])['rank_local_audit_passed'])
        self.assertFalse(summarize([])['rank_local_audit_passed'])

    def test_duplicates_cannot_fill_missing_step(self):
        rows = self.rows()[:-1]
        rows.append(rows[-1])
        self.assertFalse(summarize(rows)['rank_local_audit_passed'])

    def test_any_failure_is_preserved(self):
        rows = self.rows()
        rows.append(dict(event='update_audit', passed=False))
        self.assertFalse(summarize(rows)['rank_local_audit_passed'])

    def test_pass_flag_without_checks_is_not_evidence(self):
        rows = self.rows()
        del rows[0]['checks']
        self.assertFalse(summarize(rows)['rank_local_audit_passed'])

    def test_attempts_cannot_supply_each_others_updates(self):
        rows = self.rows()
        rows[-1]['attempt_id'] = 'different'
        self.assertFalse(summarize(rows)['rank_local_audit_passed'])
