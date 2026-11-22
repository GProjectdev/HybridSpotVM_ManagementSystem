"""Guards needed before observed replay can be used for forward estimates."""
from decimal import Decimal as D
import unittest
from .stateful_replay import World, Round, integrate_price, interval_union, trace_value


class LedgerTests(unittest.TestCase):
    def test_piecewise_market_prices(self):
        self.assertEqual(integrate_price(1800, 9000, [(0, 1), (3600, 2), (7200, 3)]), D('4'))

    def test_endpoint_has_zero_new_price_duration(self):
        self.assertEqual(integrate_price(0, 3600, [(0, 1), (3600, 99)]), 1)

    def test_short_lifetime_has_no_hidden_invoice_minimum(self):
        self.assertEqual(integrate_price(0, 1, [(0, 3600)]), 1)

    def test_price_inputs_fail_closed(self):
        for a,b,p in [(0,1,[(1,1)]),(1,0,[(0,1)]),(0,1,[(0,-1)]),(0,1,[(0,1),(0,2)]),(0,1,[(0,'NaN')])]:
            with self.subTest((a,b,p)),self.assertRaises(ValueError): integrate_price(a,b,p)

    def test_overlapping_operations_are_unioned(self):
        self.assertEqual(interval_union([(0,3),(2,5),(5,7),(10,11)]), 8)
        self.assertEqual(interval_union([]),0)


class WorldTests(unittest.TestCase):
    def world(self):return World('workload', ('p0','p1'), [100,101])
    def round(self, uid='cp1', started=5, durable=10, steps=(90,90), pods=('p0','p1')):
        return Round(uid, uid, 'workload', 1, steps, pods, D(started), D(durable), 'fixture-receipt')
    def active(self, planned=False):
        w=self.world();w.tick(10);w.save_round(self.round())
        w.begin(11,'op',('new0','new1'),{'np1':'OnDemand'},planned)
        w.targets_ready(12,'op');return w

    def test_emergency_accepts_last_complete_old_round(self):
        w=self.active();w.pin_round(13,'op','cp1')
        self.assertEqual(w.operation['round'],'cp1')

    def test_planned_rejects_old_round(self):
        w=self.active(planned=True)
        with self.assertRaises(ValueError):w.pin_round(13,'op','cp1')

    def test_partial_mixed_rank_round_rejected(self):
        for steps in [(90,), (90,91)]:
            w=self.world();w.tick(10)
            with self.assertRaises(ValueError):w.save_round(self.round(steps=steps))

    def test_future_durable_receipt_rejected(self):
        w=self.world();w.tick(8)
        with self.assertRaises(ValueError):w.save_round(self.round())

    def test_historical_archive_source_differs_from_current_fenced_world(self):
        w=self.world();w.tick(10);w.save_round(self.round(pods=('old0','old1')))
        w.begin(11,'op',('new0','new1'),{'np1':'OnDemand'},False);w.targets_ready(12,'op')
        w.pin_round(13,'op','cp1');w.fence(14,'op',('p0','p1'));w.prepare(15,'op')

    def test_round_cannot_change_after_selection(self):
        w=self.active();w.tick(13);w.save_round(self.round(uid='cp2'))
        w.pin_round(14,'op','cp1')
        with self.assertRaises(ValueError):w.pin_round(15,'op','cp2')

    def test_latest_round_uses_durable_time(self):
        w=self.world();w.tick(20);w.save_round(self.round());w.save_round(self.round(uid='z',durable=19))
        self.assertEqual(w.latest_round().uid,'z')

    def test_no_durable_round_cannot_restore(self):
        with self.assertRaises(ValueError):self.world().latest_round()

    def test_source_fence_must_name_current_pods(self):
        w=self.active();w.pin_round(13,'op','cp1')
        with self.assertRaises(ValueError):w.fence(14,'op',('old0','old1'))

    def test_prepare_requires_fence(self):
        w=self.active();w.pin_round(13,'op','cp1')
        with self.assertRaises(ValueError):w.prepare(14,'op')

    def test_source_cannot_continue_after_fence(self):
        w=self.active();w.pin_round(13,'op','cp1');w.fence(14,'op',w.pods)
        with self.assertRaises(ValueError):w.commit(15,0,'p0',102)

    def test_full_world_resume_rollback_and_verified_hold(self):
        w=self.active();w.pin_round(13,'op','cp1');w.fence(14,'op',w.pods);w.prepare(15,'op');w.resume(16,'op')
        self.assertEqual(w.total_rollback,[10,11]);self.assertEqual(w.generation,1)
        for rank in range(2):w.commit(17,rank,'new'+str(rank),91)
        with self.assertRaises(ValueError):w.verify(18,'op')
        for rank in range(2):w.commit(19,rank,'new'+str(rank),92)
        w.verify(20,'op')
        self.assertEqual(w.held_on_demand(3619,{'np1'}),{'np1':D(3620)})
        self.assertEqual(w.held_on_demand(3620,{'np1'}),{})

    def test_wrong_operation_rejected(self):
        w=self.active()
        with self.assertRaises(ValueError):w.targets_ready(13,'foreign')

    def test_two_competing_operations_rejected(self):
        w=self.active()
        with self.assertRaises(ValueError):w.begin(13,'other',('b0','b1'),{'np2':'Spot'},False)

    def test_no_lookahead_clock(self):
        w=self.world();w.tick(10)
        with self.assertRaises(ValueError):w.commit(9,0,'p0',101)

    def test_unrecorded_step_rollback_rejected(self):
        w=self.world()
        with self.assertRaises(ValueError):w.commit(1,0,'p0',90)

    def test_one_jump_is_not_two_fresh_progress_samples(self):
        w=self.active();w.pin_round(13,'op','cp1');w.fence(14,'op',w.pods);w.prepare(15,'op');w.resume(16,'op')
        for rank in range(2):w.commit(17,rank,'new'+str(rank),92)
        with self.assertRaises(ValueError):w.verify(18,'op')

    def test_four_rank_one_batch_restore_keeps_all_rank_rollback(self):
        w=World('workload',('p0','p1','p2','p3'),[3232,3232,3232,3233]);w.tick(20)
        cp=Round('cp','round','workload',1,(3216,)*4,w.pods,D(12),D(19),'measured-shape-fixture')
        w.save_round(cp);w.begin(21,'op',('n0','n1','n2','n3'),{'np1':'OnDemand','np2':'OnDemand','np3':'OnDemand'},False)
        w.targets_ready(22,'op');w.pin_round(23,'op','cp');w.fence(24,'op',w.pods);w.prepare(25,'op');w.resume(26,'op')
        self.assertEqual(w.total_rollback,[16,16,16,17]);self.assertEqual(w.generation,1)
        for at,step in [(27,3217),(28,3218)]:
            for rank in range(4):w.commit(at,rank,'n'+str(rank),step)
        w.verify(29,'op');self.assertEqual(len(w.held_on_demand(30,{'np1','np2','np3'})),3)


class TraceTests(unittest.TestCase):
    def test_native_boundary_and_no_wrap(self):
        self.assertEqual(trace_value([.1,.2],0,3599),D('.1'))
        self.assertEqual(trace_value([.1,.2],0,3600),D('.2'))
        with self.assertRaises(EOFError):trace_value([.1,.2],1,3600)

    def test_unknown_or_negative_rate_not_zero_filled(self):
        with self.assertRaises(ValueError):trace_value([-.1],0,0)
        with self.assertRaises(ValueError):trace_value(['NaN'],0,0)


if __name__=='__main__':unittest.main()
