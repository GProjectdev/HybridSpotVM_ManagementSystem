import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import e2_schedule as schedule


class ScheduleTests(unittest.TestCase):
    def test_both_ranks_and_run_scope(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / 'events'
            events = [dict(run_id='r', event='step', rank=0, timestamp='2026-11-04T00:00:00Z'),
                      dict(run_id='other', event='step', rank=1, timestamp='2026-11-04T00:00:01Z')]
            path.write_text('\n'.join(map(json.dumps, events)))
            self.assertIsNone(schedule.anchor(path, 'r'))
            events.append(dict(run_id='r', event='step', rank=1, timestamp='2026-11-04T00:00:03Z'))
            path.write_text('\n'.join(map(json.dumps, events)) + '\n{')
            self.assertEqual(schedule.anchor(path, 'r').second, 3)

    def test_wrong_target_never_arms(self):
        with tempfile.TemporaryDirectory() as d:
            with patch('sys.argv', ['schedule', '--evidence', d, '--confirm', 'r',
                       '--instance-id', 'i-wrong', '--role-arn', 'role', '--alarm-arn', 'alarm']), \
                 patch.object(schedule.e2, 'info', return_value={'phase': 'running'}), \
                 patch.object(schedule.fis, 'target', return_value={'instance_id': 'i-right'}), \
                 patch.object(schedule.fis, 'start') as start:
                with self.assertRaises(ValueError):
                    schedule.main()
                start.assert_not_called()
                self.assertFalse((Path(d) / 'fis-schedule.json').exists())


if __name__ == '__main__':
    unittest.main()
