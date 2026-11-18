import json
import unittest
from ieee_v2_observe import log_events, policy_events, validate_run


class ObserverTests(unittest.TestCase):
    def test_policy_stage_fields_retained_only_for_exact_policy_and_namespace(self):
        event = {'msg': 'EvaluationPolicyDecision', 'policyUID': 'p', 'policyNamespace': 'n',
                 'intervalSource': 'checkpoint-cost-analytic', 'decisionStage': 'CheckpointMeasured',
                 'intervalCostEvaluated': True, 'private_key': 'do-not-export'}
        rows = list(policy_events('UTC '+json.dumps(event), 'p', 'n'))
        self.assertEqual(rows[0]['intervalSource'], 'checkpoint-cost-analytic')
        self.assertEqual(rows[0]['decisionStage'], 'CheckpointMeasured')
        self.assertNotIn('private_key', rows[0])
        self.assertEqual(list(policy_events('UTC '+json.dumps(event), 'other', 'n')), [])

    def setUp(self):
        self.info={'protocol':'ieee-access-v2','arm':'N1','namespace':'eval-v2-test','run_id':'test',
                   'world_size':2,'workload_lock_sha256':'a'*64,'started_at':'2026-11-17T01:00:00Z'}
        self.pod={'metadata':{'name':'eval-test-0','uid':'current','creationTimestamp':'2026-11-17T01:01:00Z'},
                  'spec':{'nodeName':'worker','containers':[{'name':'trainer','env':[{'name':'EVAL_RUN_ID','value':'test'}]}]}}

    def event(self, at, **extra):
        return {'event':'step','run_id':'test','rank':0,'global_step':12,'world_size':2,
                'attempt_id':'attempt','timestamp':at,**extra}

    def test_current_pod_window_and_run_are_required_and_arbitrary_fields_removed(self):
        records=[self.event('2026-11-17T00:59:59Z'),self.event('2026-11-17T01:00:59Z'),
                 self.event('2026-11-17T01:01:01.123456789Z',private_key='do-not-export'),
                 self.event('2026-11-17T01:01:02Z',run_id='foreign')]
        text='\n'.join('EVAL_EVENT '+json.dumps(e) for e in records)
        events=list(log_events(text,self.info,self.pod))
        self.assertEqual(len(events),1)
        self.assertEqual(events[0]['pod_uid'],'current')
        self.assertNotIn('private_key',events[0])
        self.assertEqual(events[0]['timestamp'],'2026-11-17T01:01:01.123456789Z')

    def test_legacy_or_mismatched_namespace_is_not_accepted(self):
        validate_run(self.info)
        for change in [{'namespace':'fluidcr-realign-121040'},{'arm':'A'},{'world_size':4}]:
            with self.assertRaises(ValueError):validate_run(dict(self.info,**change))
        self.pod['spec']['containers'][0]['env'][0]['value']='old'
        with self.assertRaises(ValueError):list(log_events('',self.info,self.pod))
