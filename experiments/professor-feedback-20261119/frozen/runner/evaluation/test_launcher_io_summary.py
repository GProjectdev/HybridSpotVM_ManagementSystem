import unittest
from launcher_io_summary import summarize

class LauncherIOAccountingTests(unittest.TestCase):
    def event(self,**values):
        return dict(event='launcher_checkpoint_io',timestamp='2026-11-19T00:00:00Z',run_id='run',pod_uid='uid',mode='reuse-identical',buffer_bytes=1024,written_bytes=0,elapsed_seconds=1.,**values)
    def test_duplicate_samples_do_not_double_count_and_absence_is_not_zero_latency(self):
        e=self.event();result=summarize([e,e,{'event':'step','global_step':100}])
        self.assertEqual(result['rows'][0]['observations'],1)
        self.assertEqual(result['rows'][0]['sum_local_elapsed_seconds_not_union'],1)
        self.assertEqual(summarize([])['status'],'NO_OBSERVATIONS')
    def test_claimed_reuse_with_nonzero_write_or_nonfinite_time_is_rejected(self):
        for change in ({'written_bytes':1},{'elapsed_seconds':float('inf')}):
            e=self.event();e.update(change)
            with self.assertRaises(ValueError):summarize([e])
