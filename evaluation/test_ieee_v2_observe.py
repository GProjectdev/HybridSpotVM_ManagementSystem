import json
import unittest
from ieee_v2_observe import log_events, policy_events, validate_run, launcher_io_events


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

    def test_local_checkpoint_io_is_uid_bound_and_never_training_progress(self):
        self.info['app']='eval-test'
        event={'mode':'reuse-identical','buffer_bytes':1024,'written_bytes':0,'elapsed_seconds':.1,'secret':'not-exported'}
        prefix='2026-11-17T01:01:01Z [Launcher lifecycle] checkpoint_buffer_materialized '
        text=prefix+json.dumps(event)
        rows=list(launcher_io_events(text,self.info,self.pod))
        self.assertEqual(len(rows),1);self.assertEqual(rows[0]['pod_uid'],'current')
        self.assertNotIn('global_step',rows[0]);self.assertNotIn('secret',rows[0])
        self.assertEqual(list(log_events(text,self.info,self.pod)),[])
        self.assertEqual(list(launcher_io_events(text.replace('01:01:01Z','00:01:01Z'),self.info,self.pod)),[])
        for key,value in [('elapsed_seconds',float('nan')),('written_bytes',1024),('buffer_bytes',True),('mode','unsafe')]:
            bad=dict(event,**{key:value})
            self.assertEqual(list(launcher_io_events(prefix+json.dumps(bad),self.info,self.pod)),[])

    def test_legacy_or_mismatched_namespace_is_not_accepted(self):
        validate_run(self.info)
        for change in [{'namespace':'fluidcr-realign-121040'},{'arm':'A'},{'world_size':4}]:
            with self.assertRaises(ValueError):validate_run(dict(self.info,**change))
        self.pod['spec']['containers'][0]['env'][0]['value']='old'
        with self.assertRaises(ValueError):list(log_events('',self.info,self.pod))

class ExtendedDecisionAllowlistTests(unittest.TestCase):
    def test_measured_inputs_and_calculation_actor_survive_sanitization(self):
        raw={'msg':'EvaluationPolicyDecision','policyUID':'p','policyNamespace':'n','decisionActor':'checkpoint-coordinator',
             'alpha':.8,'spotPricePerHour':.5593,'checkpointSeconds':195,'copySeconds':70,'checkpointCostsObservedAt':'original-time',
             'paperAsyncProfileEnabled':False,'intervalSource':'checkpoint-cost-analytic','credentials':'omit'}
        rows=list(policy_events('UTC '+json.dumps(raw),'p','n'))
        self.assertEqual(rows[0]['checkpointSeconds'],195)
        self.assertEqual(rows[0]['decisionActor'],'checkpoint-coordinator')
        self.assertEqual(rows[0]['checkpointCostsObservedAt'],'original-time')
        self.assertNotIn('credentials',rows[0])
