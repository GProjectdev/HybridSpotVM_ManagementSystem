import copy,json,tempfile,unittest
from pathlib import Path
from ieee_v2_ledger import historical_termination_receipts

class HistoricalTerminationTests(unittest.TestCase):
 def test_only_positive_current_uid_scope_receipts_count(self):
  run={'policy_uid':'policy','namespace':'eval','started_at':'2026-01-01T00:00:00Z'}
  row={'instance_id':'i-0123456789abcdef0','state':'terminated','provider_ownership_verified':True,'policy_uid':'policy','nodeprovision_uid':'np','nodeprovision_name':'worker','nodeprovision_namespace':'eval','launch_time':'2026-01-01T00:00:01Z','observed_at':'2026-01-01T00:01:00Z'}
  for field,value in [(None,None),('state','NOT_OBSERVED'),('state','running'),('provider_ownership_verified',False),('policy_uid','other'),('nodeprovision_uid','new'),('nodeprovision_name','other'),('nodeprovision_namespace','foreign'),('observed_at','2025-01-01T00:00:00Z'),('launch_time','invalid')]:
   with self.subTest(field=field,value=value),tempfile.TemporaryDirectory() as d:
    p=Path(d)/'snapshots'/'one';p.mkdir(parents=True);changed=copy.deepcopy(row)
    if field:changed[field]=value
    (p/'aws-instance-ledger.json').write_text(json.dumps([changed]));result=historical_termination_receipts(d,run,{row['instance_id']:row})
    self.assertEqual(bool(result),field is None)
    if result:self.assertTrue(result[row['instance_id']]['not_inferred_from_disappearance'])

 def test_current_live_state_never_overridden_by_old_termination(self):
  from integrated_control import Control
  from types import SimpleNamespace
  run={'policy_uid':'policy','namespace':'eval','started_at':'2026-01-01T00:00:00Z'}
  row={'instance_id':'i-0123456789abcdef0','state':'terminated','provider_ownership_verified':True,'policy_uid':'policy','nodeprovision_uid':'np','nodeprovision_name':'worker','nodeprovision_namespace':'eval','launch_time':'2026-01-01T00:00:01Z','observed_at':'2026-01-01T00:01:00Z'}
  for state,verified,passes in [('NOT_OBSERVED',False,True),('terminated',True,True),('running',True,False),('terminated',False,False)]:
   with self.subTest(state=state,verified=verified),tempfile.TemporaryDirectory() as d:
    c=Control.__new__(Control);c.d=Path(d);c.run=run;c.owned=lambda cluster:[]
    p=c.d/'snapshots'/'one';p.mkdir(parents=True);(p/'aws-instance-ledger.json').write_text(json.dumps([row]));(c.d/'cleanup-owned-instance-identities.json').write_text(json.dumps({row['instance_id']:row}))
    current=dict(row,state=state,provider_ownership_verified=verified);c.cloud=SimpleNamespace(observe=lambda *args:[current])
    if passes:c.verify_cleanup();self.assertTrue((c.d/'exact-ec2-cleanup-verification.json').exists())
    else:
     with self.assertRaises(ValueError):c.verify_cleanup()
