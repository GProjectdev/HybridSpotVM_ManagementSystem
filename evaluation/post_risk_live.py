"""Post-risk evidence hook for the existing exact-target FIS dispatcher.

All existing account, provider UID, market, type/AZ, 4-rank one-GPU placement,
health alarm, exclusive intent and pre-dispatch rechecks are retained.
"""
import json
import subprocess
from pathlib import Path
try:
    from .final_fis import Live
    from . import post_risk_observe as observe, post_risk_fault_gate as gate
except ImportError:
    from final_fis import Live
    import post_risk_observe as observe, post_risk_fault_gate as gate


class PostRiskLive(Live):
    def __init__(self,run,profile,kubeconfigs,directory,runner=subprocess.run):
        super().__init__(run,profile,kubeconfigs,runner)
        self.directory=Path(directory)

    def final_gate(self,policy,current,events,observed):
        run=self.run
        snapshot,safety=observe.latest_inputs(self.directory)
        result,bundle=observe.evaluate(self.directory,run,snapshot,safety,observed)
        if not result.get('ready'):return result
        if {int(k):v for k,v in bundle['current_pods'].items()}!=current:
            raise ValueError('live current rank UIDs differ from observer evidence')
        risk=json.loads(self.kub('karmada','get','spotriskprofile',policy['spec']['riskProfileRef']['name'],'-o','json'))
        app=bundle['risk_application'];rm=risk['metadata'];rs=risk['status'];src=rs.get('source',{})
        g=run['post_risk_gate']
        if (rm.get('uid')!=app['risk_uid'] or rs.get('ready') is not True
                or rm.get('generation')!=rs.get('observedGeneration')
                or rs.get('lambdaPerHour')!=g['updated_lambda_per_hour']
                or src.get('heldOutHour')!=g['update_hour'] or src.get('selectedStartHour')!=g['start_hour']
                or src.get('forecastSHA256')!=g['forecast_sha256'] or src.get('traceSHA256')!=g['trace_sha256']
                or not gate.stamp(rs['observedAt'])<=gate.stamp(observed)<gate.stamp(rs['validUntil'])):
            raise ValueError('live risk input changed after first admission')
        migrations=json.loads(self.kub('aws','get','fluidcrmigrations','-o','json'))['items']
        for key in ('schedule','checkpoint'):
            matches=[x for x in migrations if x['metadata']['uid']==bundle[key]['metadata']['uid']]
            if len(matches)!=1:raise ValueError('live schedule/archive UID missing')
            bundle[key]=matches[0]
        active=[]
        terminal=('Verified','Completed','Failed','Cancelled')
        for x in migrations:
            if x['metadata']['uid'] in (bundle['schedule']['metadata']['uid'],bundle['checkpoint']['metadata']['uid']):continue
            if x.get('spec',{}).get('workloadRef',{}).get('uid')==run['workload_uid'] and x.get('status',{}).get('phase') not in terminal:
                active.append(x['metadata']['uid'])
        for cluster,kind in (('aws','restorerequests'),('karmada','spotreplacements'),('karmada','spotrecoveries')):
            for x in json.loads(self.kub(cluster,'get',kind,'-o','json'))['items']:
                if x['metadata'].get('labels',{}).get(gate.LABEL+'policy-uid')==run['policy_uid'] and x.get('status',{}).get('phase') not in terminal:
                    active.append(x['metadata']['uid'])
        bundle['active_operation_uids']=active
        bundle['training_events']=bundle['training_events']+events
        # Recheck at actual end of reads; no cached timestamp can grant freshness.
        from datetime import datetime,timezone
        now=datetime.now(timezone.utc).isoformat()
        result=gate.check(run,bundle,now)
        return observe.enforce_first(run,observe.read(self.directory/'post-risk-first-gate.json'),result,now)
