import json,tempfile,unittest
from pathlib import Path
from unittest.mock import Mock
from datetime import datetime,timezone,timedelta
import integrated_control as c

class ControlBoundaryTests(unittest.TestCase):
    def controller(self):
        now=datetime.now(timezone.utc)
        x=c.Control.__new__(c.Control);x.run={'namespace':'eval-v3-case','run_id':'case','policy_uid':'policy','started_at':now.isoformat(),'hard_deadline_utc':(now+timedelta(hours=2)).isoformat(),'cleanup_start_utc':(now+timedelta(hours=1,minutes=40)).isoformat(),'max_seconds':7200,'paid_authorization':{'source':'explicit user window'},'purpose':'acceptance'};x.cloud=Mock();x.kub=Mock();return x
    def test_expired_or_unfrozen_run_cannot_start_paid_capacity(self):
        x=self.controller();x.mutate_gate();x.cloud.observe.assert_called_once()
        for change in ({'cleanup_start_utc':'2026-11-17T14:40:00Z'},{'paid_authorization':False},{'purpose':'formal','protocol_lock_status':'DRAFT'},{'max_seconds':60}):
            x=self.controller();x.run.update(change)
            with self.assertRaises(ValueError):x.mutate_gate()
            x.cloud.observe.assert_not_called()
    def test_cleanup_uses_server_uid_precondition_and_preserves_storage(self):
        x=self.controller();o={'apiVersion':'ml.dcn.ssu.ac.kr/v1alpha1','kind':'NodeProvision','metadata':{'name':'worker','namespace':x.run['namespace'],'uid':'exact-provider-uid'}}
        x.delete_owned('karmada','nodeprovisions',o)
        args,kw=x.kub.call_args
        self.assertEqual(args,('karmada','delete','--raw=/apis/ml.dcn.ssu.ac.kr/v1alpha1/namespaces/eval-v3-case/nodeprovisions/worker','--filename=-'))
        self.assertEqual(kw['obj']['preconditions'],{'uid':'exact-provider-uid'})
    def test_foreign_provider_and_recreated_parent_block_operations(self):
        x=self.controller();x.get=Mock(return_value={'items':[{'metadata':{'labels':{'training.dcnlab.com/policy-uid':'foreign'}}}]})
        with self.assertRaisesRegex(ValueError,'foreign'):x.owned('aws')
        x.get.return_value={'metadata':{'uid':'recreated'}};x.run['policy']='name'
        with self.assertRaisesRegex(ValueError,'recreated'):x.policy()
        x.kub.assert_not_called()
    def test_disappeared_cr_without_positive_terminated_ec2_is_not_cleanup(self):
        x=self.controller();x.owned=Mock(return_value=[])
        with tempfile.TemporaryDirectory() as d:
            x.d=Path(d);(x.d/'cleanup-owned-instance-identities.json').write_text(json.dumps({'i-0123456789abcdef0':{}}))
            for state in ['running','NOT_OBSERVED']:
                x.cloud.observe.return_value=[{'state':state,'provider_ownership_verified':True}]
                with self.assertRaisesRegex(ValueError,'termination'):x.verify_cleanup()
            self.assertFalse((x.d/'exact-ec2-cleanup-verification.json').exists())
if __name__=='__main__':unittest.main()
