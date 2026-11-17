import copy
import json
from pathlib import Path
import unittest

import ieee_v2 as v2
import run


class ProtocolTests(unittest.TestCase):
    def setUp(self):
        self.c = json.loads((v2.HERE / 'config.gpt2.example.json').read_text())
        self.c.pop('namespace')
        self.c.update(cluster='aws1', instance_type='g5.xlarge', goal_steps=4747,
                      training_image='docker.io/example/train@sha256:' + 'a'*64,
                      asset_manifest_sha256='b'*64, learning_rate=0.00005,
                      nfs_server='10.50.1.98')
        self.source = {'spec': {'capacity': {'aws': {'ami':'ami-a','subnetId':'subnet-a',
             'vpcId':'vpc-a','securityGroupIds':['sg-a'],
             'credentialsRef':{'name':'aws-node-credentials','namespace':'old-run'}}},
             'policy':{'fixedOnDemand':2,'economics':{'enabled':True}},
             'checkpoint':{'measuredCosts':{'checkpointSeconds':999},'paperProfile':{'enabled':True}}}}
        self.risk = {'status':'LOCKED','wall_seconds_per_trace_hour':3600,'trace_sha256':'c'*64,
                     'forecast_sha256':'d'*64,'profile_spec':{'provider':'aws','region':'ap-northeast-2',
                     'availabilityZone':'ap-northeast-2c','instanceType':'g5.xlarge',
                     'paperEstimator':{'feedEndpoint':'https://feed.example/feed'}}}

    def test_new_runs_are_fully_namespace_scoped_without_changing_legacy_globals(self):
        for rid in ('n0-first','n0-repeat'):
            info, storage, objects, policy = v2.render(self.c,self.source,rid,'N0')
            ns = 'eval-v2-' + rid
            for obj in storage + objects + [policy]:
                if obj['kind'] == 'PersistentVolume':
                    self.assertEqual(obj['spec']['claimRef']['namespace'],ns)
                else:
                    self.assertEqual(obj['metadata']['namespace'],ns)
            self.assertEqual(policy['kind'],'NodeRequest')
            self.assertEqual(policy['spec']['capacity']['aws']['credentialsRef']['namespace'],ns)
            self.assertEqual(policy['metadata']['annotations']['training.dcnlab.com/suspend'],'true')
            self.assertEqual(policy['metadata']['annotations']['training.dcnlab.com/periodic-checkpoint'],'disabled')
            sts=next(x for x in objects if x['kind']=='StatefulSet')
            self.assertEqual(sts['spec']['replicas'],0)
            env={e['name']:e.get('value') for e in sts['spec']['template']['spec']['containers'][0]['env']}
            self.assertIn('.'+ns+'.svc.',env['MASTER_ADDR'])
            self.assertEqual(sts['spec']['template']['spec']['nodeSelector']['ml.dcn.ssu.ac.kr/node-provision-namespace'],ns)
            self.assertFalse(info['ready_for_paid_execution'])
        self.assertEqual(run.NS,'fluidcr-realign-121040')
        self.assertEqual(self.source['spec']['capacity']['aws']['credentialsRef']['namespace'],'old-run')

    def test_e2_changes_only_composition_and_event_rule_not_checkpoint_or_recovery(self):
        rendered = {arm: v2.render(self.c,self.source,'test'+arm.lower(),arm,risk_source=self.risk) for arm in ('S0','S1','P0','P1')}
        self.assertEqual(len({x[0]['workload_lock_sha256'] for x in rendered.values()}),1)
        self.assertEqual(len({x[0]['checkpoint_lock_sha256'] for x in rendered.values()}),1)
        self.assertEqual(len({x[0]['risk_source_sha256'] for x in rendered.values()}),1)
        for arm, (info,_,objs,p) in rendered.items():
            self.assertEqual(p['metadata']['annotations']['training.dcnlab.com/planned-partial'],'disabled')
            self.assertNotIn('paperProfile',p['spec']['checkpoint'])
            self.assertTrue(p['spec']['policy']['economics']['enabled'])
            self.assertEqual(p['spec']['policy']['method'], 'paper-eq6-7')
            self.assertNotIn('forecastSeconds',p['spec']['policy'])
            self.assertEqual(p['spec']['policy']['forecastHorizonSeconds'],3600)
            self.assertEqual(p['spec']['policy'].get('fixedOnDemand'),1 if arm.startswith('S') else None)
            self.assertEqual(info['event_after_first_committed_step_seconds'],1800 if arm.endswith('1') else None)
            self.assertNotIn('staticLambdaPerHour',next(o for o in objs if o['kind']=='SpotRiskProfile')['spec'])

    def test_invalid_or_unlocked_protocol_is_rejected(self):
        for fault in ('tag','goal','namespace','compressed-risk','missing-lock','foreign-profile'):
            c,r=copy.deepcopy(self.c),copy.deepcopy(self.risk)
            if fault=='tag':c['training_image']='example:latest'
            if fault=='goal':c['goal_steps']=4836
            if fault=='namespace':c['namespace']='old-run'
            if fault=='compressed-risk':r['wall_seconds_per_trace_hour']=60
            if fault=='missing-lock':r['status']='PREVIEW'
            if fault=='foreign-profile':r['profile_spec']['region']='us-east-1'
            with self.subTest(fault=fault),self.assertRaises(ValueError):
                v2.render(c,self.source,'test','P1',risk_source=r)
        with self.assertRaises(ValueError):v2.render(self.c,self.source,'test','S0')


class MetricTests(unittest.TestCase):
    def setUp(self):
        self.info={'protocol':'ieee-access-v2','arm':'N0','run_id':'test','goal_steps':10,'tokens_per_committed_step':4096}
        self.events=[self.event(r,'step',1,'00:00:10') for r in (0,1)]
        self.events += [self.event(r,'completed',10,'00:01:10') for r in (0,1)]

    def event(self,rank,event,step,at,attempt='attempt'):
        return {'run_id':'test','rank':rank,'event':event,'global_step':step,
                'timestamp':'2026-11-17T'+at+'Z','attempt_id':attempt,
                'pod_uid':'pod-'+str(rank),'world_size':2}

    def test_rank_replay_and_duplicate_collection_do_not_inflate_useful_tokens(self):
        events=self.events+self.events+[self.event(0,'step',1,'00:00:20','replay')]
        m=v2.metrics(self.info,events,request_at='2026-11-17T00:00:00Z')
        self.assertEqual(m['useful_tokens'],40960)
        self.assertEqual(m['T_startup_seconds'],10)
        self.assertEqual(m['T_E2E_seconds'],70)
        self.assertEqual(m['T_training_seconds'],60)
        self.assertIsNone(m['checkpoint_stall_seconds'])
        self.assertIsNone(m['estimated_gpu_compute_cost_usd'])

    def test_old_completed_logs_and_one_rank_only_do_not_certify_completion(self):
        m=v2.metrics(self.info,self.events,request_at='2026-11-17T00:02:00Z')
        self.assertFalse(m['all_rank_completion_observed'])
        m=v2.metrics(self.info,[e for e in self.events if e['rank']==0])
        self.assertIsNone(m['useful_tokens'])
        self.assertEqual(m['quality'],'INCOMPLETE')
        with self.assertRaises(ValueError):v2.metrics(self.info,[dict(self.events[0],world_size=4)])

    def test_stall_union_avoids_double_counting(self):
        self.assertEqual(v2.interval_union([('2026-11-17T00:00:00Z','2026-11-17T00:01:00Z'),
                           ('2026-11-17T00:00:30Z','2026-11-17T00:02:00Z')]),120)

    def test_billing_requires_contiguous_lifetimes_and_no_double_counting(self):
        owned={'i-123':{'state':'terminated','nodeprovision_uid':'np','policy_uid':'policy',
                        'billing_start':'2026-11-17T00:00:00Z','billing_end':'2026-11-17T01:00:00Z'}}
        segment={'instance_id':'i-123','start':owned['i-123']['billing_start'],
                 'end':owned['i-123']['billing_end'],'currency':'USD','price_per_hour':'1.237'}
        self.assertAlmostEqual(v2.compute_cost([segment],owned),1.237)
        for segments,instances in [([segment,segment],owned),([],owned),
              ([dict(segment,start='2026-11-17T00:01:00Z')],owned),
              ([segment],{'i-123':dict(owned['i-123'],state='running')}),
              ([dict(segment,currency='KRW')],owned),
              ([dict(segment,price_per_hour='NaN')],owned)]:
            with self.assertRaises(ValueError):v2.compute_cost(segments,instances)


if __name__=='__main__':unittest.main()
