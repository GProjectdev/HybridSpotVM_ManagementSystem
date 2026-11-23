"""Build post-risk gate evidence from existing read-only observer snapshots.

No Secret/ConfigMap read, no cloud mutation. Historical snapshots remain intact.
The first matching decision plus acknowledged Member spec is an application
lower bound, not an invented exact Kubernetes patch timestamp.
"""
import json
from pathlib import Path
try:
    from . import post_risk_fault_gate as gate
except ImportError:
    import post_risk_fault_gate as gate

L = gate.LABEL


def one(items, predicate, label):
    found = [x for x in items if predicate(x)]
    if len(found) != 1:
        raise ValueError('exactly one '+label+' required')
    return found[0]


def application(run, policy, risk, parent, decisions, observed_at):
    """Receipt from actual forecast provenance, policy calculation and Member ack."""
    g=gate.validate_contract(run); now=gate.stamp(observed_at)
    rm,rs=risk['metadata'],risk.get('status',{}); src=rs.get('source',{})
    pm,ps=parent['metadata'],parent['spec']
    if (policy['metadata']['uid']!=run['policy_uid']
            or policy['metadata']['creationTimestamp']!=run['source_created_at']
            or rm['namespace']!=run['namespace'] or not rm.get('uid')
            or rs.get('ready') is not True or rs.get('observedGeneration')!=rm['generation']
            or src.get('heldOutHour')!=g['update_hour'] or src.get('selectedStartHour')!=g['start_hour']
            or src.get('wallSecondsPerTraceHour')!=3600 or src.get('replayHour')!=1
            or src.get('forecastSHA256')!=g['forecast_sha256'] or src.get('traceSHA256')!=g['trace_sha256']
            or gate.stamp(src['replayStartUTC'])!=gate.stamp(run['source_created_at'])
            or rs.get('lambdaPerHour')!=g['updated_lambda_per_hour']
            or not gate.stamp(rs['observedAt'])<=now<gate.stamp(rs['validUntil'])):
        return None
    if (pm.get('labels',{}).get(L+'policy-uid')!=run['policy_uid']
            or ps.get('workloadRef',{}).get('uid')!=run['workload_uid']
            or parent.get('status',{}).get('observedGeneration')!=pm['generation']
            or ps.get('schedule',{}).get('enabled') is not True):
        return None
    matches=[]
    for d in decisions:
        if (d.get('decisionActor')=='checkpoint-coordinator' and d.get('policyUID')==run['policy_uid']
                and d.get('workloadUID')==run['workload_uid'] and d.get('riskName')==rm['name']
                and d.get('riskGeneration')==rm['generation'] and d.get('riskObservedGeneration')==rm['generation']
                and d.get('riskObservedAt')==rs['observedAt'] and d.get('riskFresh') is True
                and d.get('lambdaPerHour')==rs['lambdaPerHour']
                and d.get('checkpointIntervalSeconds')==ps['schedule']['intervalSeconds']
                and gate.stamp(rs['observedAt'])<=gate.stamp(d['decisionObservedAt'])<=now):
            matches.append(d)
    if not matches:return None
    decision=min(matches,key=lambda d:gate.stamp(d['decisionObservedAt']))
    return dict(policy_uid=run['policy_uid'],workload_uid=run['workload_uid'],risk_uid=rm['uid'],
        risk_lock_sha256=g['risk_lock_sha256'],source_hour=src['heldOutHour'],
        evidence_sha256=gate.digest(dict(risk=risk,decision=decision,parent=parent)),
        applied_at=decision['decisionObservedAt'],lambda_per_hour=rs['lambdaPerHour'],
        schedule_uid=pm['uid'],schedule_generation=pm['generation'],initial_schedule_generation=pm['generation'],
        interval_seconds=ps['schedule']['intervalSeconds'],ack_observed_at=observed_at,
        application_time_semantics='decision lower bound + Member generation/spec acknowledgement; not exact patch timestamp')


def build(run, snapshots, events, decisions, safety, *, application_receipt=None):
    """snapshots has current raw observer resource lists and manifest.

    Caller must keep the first application receipt and first eligible decision;
    a later cheaper/better archive cannot replace that observation.
    """
    g=gate.validate_contract(run); manifest=snapshots['manifest']
    errors=[e for e in manifest.get('errors',[]) if not (
        e.get('cluster')=='aws' and e.get('kind') in ('resourcebindings','propagationpolicies'))]
    if errors:raise ValueError('essential observer capture incomplete')
    observed=manifest['observed_at']
    policy=one(snapshots['karmada-noderequests']['items'],lambda o:o['metadata']['uid']==run['policy_uid'],'source policy')
    if policy['metadata'].get('deletionTimestamp'):raise ValueError('source policy deleting')
    risk_name=policy.get('spec',{}).get('riskProfileRef',{}).get('name')
    if not risk_name:raise ValueError('source policy risk reference required')
    risk=one(snapshots['karmada-spotriskprofiles']['items'],lambda o:o['metadata']['name']==risk_name,'risk profile')
    migrations=snapshots['aws-fluidcrmigrations']['items']
    parents=[o for o in migrations if o.get('spec',{}).get('schedule') is not None
        and o['metadata'].get('labels',{}).get(L+'policy-uid')==run['policy_uid']]
    if len(parents)>1:raise ValueError('ambiguous Member schedule')
    parent=parents[0] if parents else {}
    receipt=application(run,policy,risk,parent,decisions,observed) if parent else None
    if application_receipt:
        if receipt is None:raise ValueError('risk application no longer matches current state')
        fields=('policy_uid','workload_uid','risk_uid','risk_lock_sha256','source_hour',
                'schedule_uid','lambda_per_hour')
        if any(receipt[k]!=application_receipt[k] for k in fields):
            raise ValueError('risk/schedule binding changed after first application')
        if receipt['schedule_generation'] < application_receipt['schedule_generation']:
            raise ValueError('Member schedule generation regressed')
        # A new measured D/copy may normally update the interval after this
        # very checkpoint. Preserve the first risk application lower bound,
        # while requiring current Coordinator decision + Member ack above.
        receipt=dict(receipt,applied_at=application_receipt['applied_at'],
            initial_schedule_generation=application_receipt.get('initial_schedule_generation',application_receipt['schedule_generation']),
            initial_application_evidence_sha256=application_receipt.get(
                'initial_application_evidence_sha256',application_receipt['evidence_sha256']))
    member=one(snapshots['aws-statefulsets']['items'],lambda o:o['metadata']['name']==run['app'],'member StatefulSet')
    if member['metadata'].get('deletionTimestamp'):raise ValueError('member workload deleting')
    pods={}; nodes={}
    for p in snapshots['aws-pods']['items']:
        m=p['metadata'];name=m['name']
        if name not in [run['app']+'-'+str(r) for r in range(4)]:continue
        if (m.get('deletionTimestamp') or p.get('status',{}).get('phase')!='Running'
                or m.get('labels',{}).get(L+'workload-uid')!=run['workload_uid']
                or not any(o.get('uid')==member['metadata']['uid'] for o in m.get('ownerReferences',[]))):
            raise ValueError('current rank ownership/running state required')
        rank=int(name.rsplit('-',1)[1]);pods[rank]=m['uid'];nodes[rank]=p['spec']['nodeName']
    if len(pods)!=4 or len(set(nodes.values()))!=4:raise ValueError('four distinct current worker nodes required')
    providers=snapshots['aws-nodeprovisions']['items'];ledger=snapshots['aws-instance-ledger']
    workers=[]
    for rank,node in nodes.items():
        np=one(providers,lambda n:n.get('status',{}).get('nodeName')==node,'node provider')
        nm=np['metadata']; ns=np.get('status',{})
        if (nm.get('labels',{}).get(L+'policy-uid')!=run['policy_uid'] or nm.get('deletionTimestamp')
                or ns.get('phase')!='Ready'):raise ValueError('owned Ready provider required')
        row=one(ledger,lambda r:r.get('instance_id')==ns['instanceId'],'provider ledger row')
        if (row.get('policy_uid')!=run['policy_uid'] or row.get('nodeprovision_uid')!=nm['uid']
                or row.get('provider_ownership_verified') is not True
                or row.get('instance_lifecycle')!=('spot' if row.get('market')=='Spot' else 'on-demand')
                or not 0<=(gate.stamp(observed)-gate.stamp(row['observed_at'])).total_seconds()<=g['max_observation_age_seconds']):
            raise ValueError('current provider UID/market proof required')
        workers.append(dict(rank=rank,pod_uid=pods[rank],instance_id=ns['instanceId'],nodeprovision_uid=nm['uid'],
            policy_uid=run['policy_uid'],market=row['market'],state=row['state'],ownership_verified=True))
    candidates=[]
    if receipt:
        for cp in migrations:
            cm=cp['metadata']
            if (cm.get('labels',{}).get(L+'scheduled-parent')==parent['metadata']['uid']
                    and gate.stamp(cm['creationTimestamp'])>=gate.stamp(receipt['applied_at'])):
                candidates.append(cp)
    # First eligible round; never skip an incomplete first child for a later one.
    checkpoint=min(candidates,key=lambda c:(gate.stamp(c['metadata']['creationTimestamp']),c['metadata']['uid'])) if candidates else None
    active=[]
    for kind in ('aws-restorerequests','karmada-spotreplacements','karmada-spotrecoveries'):
        for o in snapshots[kind]['items']:
            if (o['metadata'].get('labels',{}).get(L+'policy-uid')==run['policy_uid']
                    and o.get('status',{}).get('phase') not in ('Verified','Completed','Failed','Cancelled')):
                active.append(o['metadata']['uid'])
    for o in migrations:
        if o is checkpoint or o is parent:continue
        if (o.get('spec',{}).get('workloadRef',{}).get('uid')==run['workload_uid']
                and o.get('status',{}).get('phase') not in ('Completed','Failed','Cancelled')):
            active.append(o['metadata']['uid'])
    return dict(run_id=run['run_id'],policy_uid=run['policy_uid'],workload_uid=run['workload_uid'],
        captured_at=observed,safety=safety,current_pods=pods,training_events=events,risk_application=receipt,
        schedule=parent,checkpoint=checkpoint,active_operation_uids=active,workers=workers)


def load_snapshot(path):
    path=Path(path)
    names=('manifest','karmada-noderequests','karmada-spotriskprofiles','aws-fluidcrmigrations',
           'aws-statefulsets','aws-pods','aws-nodeprovisions','aws-instance-ledger',
           'aws-restorerequests','karmada-spotreplacements','karmada-spotrecoveries')
    return {n:json.loads((path/(n+'.json')).read_text()) for n in names}
