import copy,unittest
from fis_receipt import collect

class ReceiptTests(unittest.TestCase):
    def setUp(self):
        self.run={'protocol':'ieee-access-final-20261117','arm':'E2-B','run_id':'run','expected_aws_account':'935317843233','region':'ap-northeast-2','policy_uid':'policy'}
        self.exp={'id':'exp1','tags':{'evaluation-run':'run'},'targets':{'one':{'resourceType':'aws:ec2:spot-instance','resourceArns':['arn:aws:ec2:ap-northeast-2:935317843233:instance/i-0123456789abcdef0'],'selectionMode':'ALL'}},'actions':{'interrupt':{'actionId':'aws:ec2:send-spot-instance-interruptions'}}}
    def get(self,exp,run=None):
        calls=[]
        class Reader:
            account='935317843233'
            def observe(self,owned,at):assert not owned
            def command(self,*args):calls.append(args);return {'experiment':exp}
        out=collect(Reader(),run or self.run,'exp1','i-0123456789abcdef0','now',{'i-0123456789abcdef0':{'policy_uid':'policy','nodeprovision_uid':'np','market':'Spot'}})
        self.assertEqual(calls,[('fis','get-experiment','--id','exp1')]);return out
    def test_read_only_receipt(self):self.assertEqual(self.get(self.exp)['instance_id'],'i-0123456789abcdef0')
    def test_foreign_or_broad_target_and_wrong_arm_are_rejected(self):
        for patch in ('tag','target','action','filter'):
            e=copy.deepcopy(self.exp)
            if patch=='tag':e['tags']['evaluation-run']='other'
            if patch=='target':e['targets']['one']['resourceArns'].append('foreign')
            if patch=='action':e['actions']['interrupt']['actionId']='stop-instances'
            if patch=='filter':e['targets']['one']['filters']=[{'path':'State.Name','values':['running']}]
            with self.assertRaises(ValueError):self.get(e)
        with self.assertRaises(ValueError):self.get(self.exp,dict(self.run,arm='E1-B'))
