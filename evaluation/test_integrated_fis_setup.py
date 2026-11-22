import copy
import unittest
from .integrated_fis_setup import build_setup
from .integrated_protocol import PROTOCOL


class SetupTests(unittest.TestCase):
    def setUp(self):
        self.run={'protocol':PROTOCOL,'arm':'C','world_size':4,'expected_aws_account':'935317843233',
                  'run_id':'gpt2-c-01','namespace':'eval-v3-gpt2-c-01','policy_uid':'nr-uid',
                  'region':'ap-northeast-2','availability_zone':'ap-northeast-2c','instance_type':'g5.2xlarge'}
        self.role='arn:aws:iam::935317843233:role/HybridSpotFIS-20261117'
        self.nodes=[{'metadata':{'name':'worker-'+str(i),'uid':'np-'+str(i),'namespace':self.run['namespace'],
                                'labels':{'training.dcnlab.com/policy-uid':'nr-uid'}},
                     'spec':{'marketType':'OnDemand' if i==0 else 'Spot'},
                     'status':{'phase':'Ready','instanceId':'i-'+str(i+1)*17,'nodeName':'node-'+str(i)}} for i in range(4)]

    def test_exact_spot_role_scope_and_owned_OD_alarm(self):
        p=build_setup(self.run,self.nodes,self.role)
        action=p['policy_document']['Statement'][1]
        self.assertEqual(action['Resource'],'arn:aws:ec2:ap-northeast-2:935317843233:instance/i-'+'2'*17)
        self.assertEqual(action['Condition']['StringEquals']['ec2:ResourceTag/node-provision-uid'],'np-1')
        self.assertEqual(p['alarm']['Dimensions'],[{'Name':'InstanceId','Value':'i-'+'1'*17}])
        self.assertEqual(p['alarm']['TreatMissingData'],'breaching')
        self.assertFalse(p['mutations_performed'])

    def test_selection_is_stable_before_outcomes(self):
        a=build_setup(self.run,self.nodes,self.role)
        b=build_setup(self.run,list(reversed(self.nodes)),self.role)
        self.assertEqual(a,b)

    def test_inputs_not_changed(self):
        before=copy.deepcopy((self.run,self.nodes))
        build_setup(self.run,self.nodes,self.role)
        self.assertEqual((self.run,self.nodes),before)

    def test_wrong_comparison_never_becomes_C(self):
        for key,value in [('arm','B'),('world_size',2),('instance_type','g5.xlarge'),('availability_zone','ap-northeast-2a')]:
            with self.subTest(key):
                r=dict(self.run,**{key:value})
                with self.assertRaises(ValueError):build_setup(r,self.nodes,self.role)

    def test_foreign_identity_or_unready_fails(self):
        for which in ['foreign','deleting','unready','duplicate','different_namespace']:
            n=copy.deepcopy(self.nodes)
            if which=='foreign':n[1]['metadata']['labels']['training.dcnlab.com/policy-uid']='other'
            if which=='deleting':n[1]['metadata']['deletionTimestamp']='now'
            if which=='unready':n[1]['status']['phase']='Joining'
            if which=='duplicate':n[1]['status']['instanceId']=n[0]['status']['instanceId']
            if which=='different_namespace':n[1]['metadata']['namespace']='other'
            with self.subTest(which),self.assertRaises(ValueError):build_setup(self.run,n,self.role)

    def test_market_transition_cannot_silently_retarget(self):
        n=copy.deepcopy(self.nodes);n[1]['spec']['marketType']='OnDemand'
        with self.assertRaises(ValueError):build_setup(self.run,n,self.role)

    def test_foreign_or_new_role_not_implicitly_allowed(self):
        with self.assertRaises(ValueError):build_setup(self.run,self.nodes,self.role+'-new')


if __name__=='__main__':unittest.main()
