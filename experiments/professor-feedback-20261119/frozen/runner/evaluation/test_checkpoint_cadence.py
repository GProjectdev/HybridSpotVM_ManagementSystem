import unittest
from checkpoint_cadence import summarize

class CadenceTests(unittest.TestCase):
    def events(self,cid,offset):
        out=[]
        from datetime import datetime,timedelta,timezone
        base=datetime(2026,11,17,tzinfo=timezone.utc)
        for r in (0,1):
            for k,s in [('checkpoint_pause',0),('checkpoint_saved',5),('training_resumed',20)]:
                out.append({'checkpoint_id':cid,'rank':r,'event':k,'timestamp':(base+timedelta(seconds=offset+s+r)).isoformat()})
        return out
    def test_spacing_is_not_the_configured_resume_to_next_pause_wait(self):
        e=self.events('a',0)+self.events('b',141);r=summarize(e+e)
        self.assertEqual(len(r['complete_rounds']),2);self.assertEqual(r['cadence'][0]['pause_start_to_pause_start_seconds'],141)
        self.assertEqual(r['cadence'][0]['previous_application_resume_to_next_pause_seconds'],120)
    def test_missing_rank_or_resume_is_not_fabricated(self):
        e=self.events('a',0);r=summarize(e[:-1]);self.assertEqual(r['complete_rounds'],[]);self.assertEqual(r['incomplete_checkpoint_ids'],['a'])
