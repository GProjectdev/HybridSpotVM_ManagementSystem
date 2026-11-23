import copy
import unittest
import post_risk_fault_gate as gate
import post_risk_evidence as evidence
from test_post_risk_fault_gate import fixture,NOW,L


def raw_fixture():
    run,o=fixture();run['app']='trainer'
    risk=dict(metadata=dict(name='risk',namespace=run['namespace'],uid='risk-uid',generation=1),status=dict(
        ready=True,observedGeneration=1,observedAt='2026-11-23T01:00:00Z',validUntil='2026-11-23T02:00:00Z',
        lambdaPerHour=run['post_risk_gate']['updated_lambda_per_hour'],source=dict(heldOutHour=10,selectedStartHour=9,
        wallSecondsPerTraceHour=3600,replayHour=1,replayStartUTC=run['source_created_at'],forecastSHA256='d'*64,traceSHA256='c'*64)))
    policy=dict(metadata=dict(uid=run['policy_uid'],creationTimestamp=run['source_created_at']),
        spec=dict(riskProfileRef=dict(name='risk')))
    decisions=[dict(decisionActor='checkpoint-coordinator',policyUID=run['policy_uid'],workloadUID=run['workload_uid'],
        riskName='risk',riskGeneration=1,riskObservedGeneration=1,riskObservedAt='2026-11-23T01:00:00Z',
        riskFresh=True,lambdaPerHour=risk['status']['lambdaPerHour'],checkpointIntervalSeconds=3358,
        decisionObservedAt='2026-11-23T01:00:10Z')]
    snaps={'manifest':dict(observed_at=NOW,errors=[]),'karmada-noderequests':dict(items=[policy]),
        'karmada-spotriskprofiles':dict(items=[risk]),'aws-fluidcrmigrations':dict(items=[o['schedule'],o['checkpoint']]),
        'aws-statefulsets':dict(items=[dict(metadata=dict(name='trainer',uid='member'))]),
        'aws-pods':dict(items=[]),'aws-nodeprovisions':dict(items=[]),'aws-instance-ledger':[],
        'aws-restorerequests':dict(items=[]),'karmada-spotreplacements':dict(items=[]),'karmada-spotrecoveries':dict(items=[])}
    for w in o['workers']:
        r=w['rank']; node='node-'+str(r)
        snaps['aws-pods']['items'].append(dict(metadata=dict(name='trainer-'+str(r),uid=w['pod_uid'],
            labels={L+'workload-uid':run['workload_uid']},ownerReferences=[dict(uid='member')]),
            spec=dict(nodeName=node),status=dict(phase='Running')))
        snaps['aws-nodeprovisions']['items'].append(dict(metadata=dict(uid=w['nodeprovision_uid'],
            labels={L+'policy-uid':run['policy_uid']}),status=dict(nodeName=node,phase='Ready',instanceId=w['instance_id'])))
        snaps['aws-instance-ledger'].append(dict(instance_id=w['instance_id'],policy_uid=run['policy_uid'],
            nodeprovision_uid=w['nodeprovision_uid'],market=w['market'],state='running',observed_at=NOW,
            provider_ownership_verified=True,instance_lifecycle='spot' if w['market']=='Spot' else 'on-demand'))
    return run,o,snaps,decisions


class EvidenceTests(unittest.TestCase):
    def test_actual_resource_shapes_build_complete_gate(self):
        r,o,s,d=raw_fixture();b=evidence.build(r,s,o['training_events'],d,o['safety'])
        self.assertTrue(gate.check(r,b,NOW)['ready'])
        self.assertEqual(b['risk_application']['applied_at'],d[0]['decisionObservedAt'])

    def test_forecast_provenance_or_native_clock_mismatch_not_applied(self):
        for k,v in [('heldOutHour',15),('selectedStartHour',14),('wallSecondsPerTraceHour',60),
                    ('forecastSHA256','f'*64),('traceSHA256','f'*64)]:
            r,o,s,d=raw_fixture();s['karmada-spotriskprofiles']['items'][0]['status']['source'][k]=v
            b=evidence.build(r,s,o['training_events'],d,o['safety'])
            self.assertIsNone(b['risk_application']);self.assertFalse(gate.check(r,b,NOW)['ready'])

    def test_calculation_without_matching_member_ack_not_applied(self):
        r,o,s,d=raw_fixture();s['aws-fluidcrmigrations']['items'][0]['status']['observedGeneration']=1
        self.assertIsNone(evidence.build(r,s,o['training_events'],d,o['safety'])['risk_application'])

    def test_first_application_pinned_across_repeated_observation(self):
        r,o,s,d=raw_fixture();b=evidence.build(r,s,o['training_events'],d,o['safety'])
        d[0]['decisionObservedAt']='2026-11-23T01:09:00Z'
        again=evidence.build(r,s,o['training_events'],d,o['safety'],application_receipt=b['risk_application'])
        self.assertEqual(again['risk_application']['applied_at'],b['risk_application']['applied_at'])
        s['aws-fluidcrmigrations']['items'][0]['metadata']['uid']='new-parent'
        with self.assertRaisesRegex(ValueError,'binding changed'):evidence.build(r,s,o['training_events'],d,o['safety'],application_receipt=b['risk_application'])

    def test_measured_cost_schedule_update_preserves_first_application_time(self):
        r,o,s,d=raw_fixture();b=evidence.build(r,s,o['training_events'],d,o['safety'])
        parent=s['aws-fluidcrmigrations']['items'][0]
        parent['metadata']['generation']=3;parent['status']['observedGeneration']=3
        parent['spec']['schedule']['intervalSeconds']=3400;d[0]['checkpointIntervalSeconds']=3400
        d[0]['decisionObservedAt']='2026-11-23T01:09:00Z'
        updated=evidence.build(r,s,o['training_events'],d,o['safety'],application_receipt=b['risk_application'])
        self.assertEqual(updated['risk_application']['applied_at'],b['risk_application']['applied_at'])
        self.assertTrue(gate.check(r,updated,NOW)['ready'])

    def test_first_incomplete_child_not_skipped_for_later_good_child(self):
        r,o,s,d=raw_fixture();early=copy.deepcopy(o['checkpoint'])
        early['metadata'].update(uid='a-early',creationTimestamp='2026-11-23T01:02:00Z')
        early['metadata']['creationTimestamp']='2026-11-23T01:04:00Z';early['status']['phase']='Pending';s['aws-fluidcrmigrations']['items'].append(early)
        b=evidence.build(r,s,o['training_events'],d,o['safety'])
        self.assertEqual(b['checkpoint']['metadata']['uid'],'a-early')
        self.assertFalse(gate.check(r,b,NOW)['ready'])

    def test_incomplete_capture_foreign_provider_and_wrong_market_rejected(self):
        for mode in ('capture','provider','market','stale'):
            r,o,s,d=raw_fixture()
            if mode=='capture':s['manifest']['errors']=[dict(cluster='aws',kind='fluidcrmigrations')]
            if mode=='provider':s['aws-instance-ledger'][1]['nodeprovision_uid']='other'
            if mode=='market':s['aws-instance-ledger'][1]['instance_lifecycle']='on-demand'
            if mode=='stale':s['aws-instance-ledger'][1]['observed_at']='2026-11-23T00:00:00Z'
            with self.assertRaises(ValueError):evidence.build(r,s,o['training_events'],d,o['safety'])


if __name__=='__main__':unittest.main()
