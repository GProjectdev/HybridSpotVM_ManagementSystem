import json,unittest
from subprocess import CompletedProcess
from price_watch import publish

class PublishTests(unittest.TestCase):
    def test_only_price_leaf_is_patched_with_uid_and_resource_version_tests(self):
        calls=[]
        def runner(args,**kw):
            calls.append((args,kw));obj={'metadata':{'uid':'uid','resourceVersion':'7','labels':{'training.dcnlab.com/evaluation-run':'run'}},'data':{'forecast.json':'frozen','prices.json':'old'}}
            return CompletedProcess(args,0,json.dumps(obj),'')
        publish({'observedAt':'now'},kubeconfig='/explicit',namespace='ns',name='feed',uid='uid',run_id='run',runner=runner)
        patch=json.loads(calls[-1][1]['input']);self.assertEqual([x['path'] for x in patch],['/metadata/uid','/metadata/resourceVersion','/data/prices.json'])
        self.assertNotIn('forecast.json',calls[-1][1]['input'])
    def test_foreign_uid_never_publishes(self):
        calls=[]
        def runner(args,**kw):calls.append(args);return CompletedProcess(args,0,json.dumps({'metadata':{'uid':'foreign','labels':{}}}),'')
        with self.assertRaises(ValueError):publish({},kubeconfig='/explicit',namespace='ns',name='feed',uid='uid',run_id='run',runner=runner)
        self.assertEqual(len(calls),1)

    def test_bounded_watch_does_not_refresh_again_at_its_stop_boundary(self):
        import price_watch,tempfile,sys
        from pathlib import Path
        from datetime import datetime,timedelta,timezone
        from unittest.mock import patch
        class Clock:
            value=0
            def now(self):return self.value
            def sleep(self,seconds):self.value+=seconds
        clock=Clock()
        with tempfile.TemporaryDirectory() as temp:
            directory=Path(temp);(directory/'run.json').write_text(json.dumps({'protocol':'ieee-access-final-20261117','expected_aws_account':'935317843233','run_id':'run','instance_type':'g5.xlarge','region':'ap-northeast-2','availability_zone':'ap-northeast-2c'}))
            argv=['watch','--directory',temp,'--profile','explicit','--account','935317843233','--ondemand-receipt',str(directory/'receipt'),'--deadline-utc',(datetime.now(timezone.utc)+timedelta(minutes=5)).isoformat(),'--seconds','1','--interval','30']
            with patch.object(sys,'argv',argv),patch.object(price_watch,'AWSReader'),patch.object(price_watch,'refresh',return_value={'observedAt':'2026-11-17T11:00:00Z'}) as refreshed,patch.object(price_watch.time,'monotonic',clock.now),patch.object(price_watch.time,'sleep',clock.sleep):
                price_watch.main()
            self.assertEqual(refreshed.call_count,1);self.assertEqual(len(list((directory/'price-history').glob('*.json'))),1)
