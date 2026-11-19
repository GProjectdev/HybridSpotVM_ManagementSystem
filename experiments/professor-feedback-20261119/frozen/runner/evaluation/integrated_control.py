"""UID-scoped control stages for already prepared four-rank A/B/C resources.

Bootstrap (storage, runtime/net config, credentials and admission) is explicit.
This module never creates credentials, changes IAM, removes finalizers or deletes
storage/namespaces. It reuses the existing Kubernetes/provider controllers.
"""
import argparse,json,subprocess,time
from datetime import datetime,timezone
from pathlib import Path
try:
    from .integrated_protocol import PROTOCOL
    from .ieee_v2 import canonical_hash
    from .ieee_v2_ledger import AWSReader,ownership,historical_termination_receipts
    from .collect import timestamp
    from .run import save
except ImportError:
    from integrated_protocol import PROTOCOL
    from ieee_v2 import canonical_hash
    from ieee_v2_ledger import AWSReader,ownership,historical_termination_receipts
    from collect import timestamp
    from run import save

class Control:
    def __init__(self,directory,kubeconfigs,profile,runner=subprocess.run):
        self.d=Path(directory);self.run=json.loads((self.d/'run.json').read_text());self.k=kubeconfigs;self.runner=runner
        r=self.run
        if r.get('protocol')!=PROTOCOL or r.get('world_size')!=4 or r.get('arm') not in {'A','B','C'} or r.get('namespace')!='eval-v3-'+r['run_id']:
            raise ValueError('explicit four-rank v3 run required')
        if not r.get('policy_uid'):raise ValueError('source policy UID must be recorded by bootstrap')
        self.cloud=AWSReader(profile,r['expected_aws_account'],r['region'],runner)
    def kub(self,cluster,*args,obj=None):
        p=self.runner(['kubectl','--kubeconfig='+self.k[cluster],'--request-timeout=15s',*args],input=json.dumps(obj) if obj is not None else None,text=True,capture_output=True,timeout=35)
        if p.returncode:raise RuntimeError('scoped Kubernetes operation failed: '+cluster+'/'+args[0])
        return p.stdout
    def get(self,cluster,kind,name=None):
        s=self.kub(cluster,'-n',self.run['namespace'],'get',kind,*([name,'--ignore-not-found'] if name else []),'-o','json')
        return json.loads(s) if s.strip() else None
    def policy(self):
        r=self.run;p=self.get('karmada','noderequest',r['policy'])
        if p is None or p['metadata']['uid']!=r['policy_uid']:raise ValueError('policy absent or recreated')
        expected=r.get('group_replacement_batch_size',1)
        actual=p['metadata'].get('annotations',{}).get('training.dcnlab.com/group-replacement-batch-size','1')
        if type(expected) is not int or not 1<=expected<=4 or actual!=str(expected):
            raise ValueError('live policy batch size differs from run comparison lock')
        return p
    def delete_owned(self,cluster,plural,obj):
        # Server-side UID precondition closes the read/delete recreation race.
        try:
            from .run import delete_url
        except ImportError:
            from run import delete_url
        options={'apiVersion':'v1','kind':'DeleteOptions','preconditions':{'uid':obj['metadata']['uid']}}
        return self.kub(cluster,'delete','--raw='+delete_url(obj,plural),'--filename=-',obj=options)
    def mutate_gate(self):
        r=self.run;now=datetime.now(timezone.utc)
        if not r.get('paid_authorization') or not r.get('cleanup_start_utc') or now>=timestamp(r['cleanup_start_utc']):raise ValueError('explicit paid window/cleanup reserve required')
        start=timestamp(r['started_at']);cleanup=timestamp(r['cleanup_start_utc']);hard=timestamp(r['hard_deadline_utc'])
        if now<start or not 0<(hard-start).total_seconds()<=min(r['max_seconds'],18000) or (hard-cleanup).total_seconds()<1200:
            raise ValueError('bounded active window with20minute cleanup reserve required')
        if r.get('purpose')=='formal' and r.get('protocol_lock_status')!='FROZEN':raise ValueError('formal settings not frozen')
        if r.get('purpose')=='formal':self.verify_system_lock()
        self.cloud.observe({},now.isoformat())
    def verify_system_lock(self):
        """Read actual deployed specs, not the current local Git branch name."""
        refs=self.run.get('system_lock',{}).get('image_references',{})
        if not refs:raise ValueError('explicit deployed system image lock required')
        clusters={'MGMT':'mgmt','AWS':'aws','ONPRE1':'onpre1'};receipts=[]
        for key,expected in refs.items():
            parts=key.split('/');cluster=clusters.get(parts[0])
            if cluster not in self.k or not self.k[cluster]:raise ValueError('all locked cluster kubeconfigs required')
            if len(parts)==5 and parts[1] in {'deployments','daemonsets'}:
                _,kind,namespace,name,container=parts
                o=json.loads(self.kub(cluster,'-n',namespace,'get',kind,name,'-o','json'))
                cs=[c for c in o['spec']['template']['spec']['containers'] if c['name']==container]
                if len(cs)!=1 or cs[0]['image']!=expected or o['metadata'].get('deletionTimestamp'):
                    raise ValueError('deployed controller image differs from lock: '+key)
                status=o.get('status',{});desired=o['spec'].get('replicas',1) if kind=='deployments' else status.get('desiredNumberScheduled',0)
                updated=status.get('updatedReplicas',0) if kind=='deployments' else status.get('updatedNumberScheduled',0)
                ready=status.get('availableReplicas',0) if kind=='deployments' else status.get('numberReady',0)
                if desired<0 or (kind=='deployments' and desired<1) or updated!=desired or ready<desired or status.get('observedGeneration')!=o['metadata']['generation']:
                    raise ValueError('locked controller rollout not Ready: '+key)
            elif len(parts)==2 and parts[1]=='fluidcr-payload':
                o=json.loads(self.kub(cluster,'-n','fluidcr-system','get','deployment','fluidcr-webhook','-o','json'))
                args=[a for c in o['spec']['template']['spec']['containers'] for a in c.get('args',[])];values=[]
                for i,a in enumerate(args):
                    if a=='--payload-image' and i+1<len(args):values.append(args[i+1])
                    elif a.startswith('--payload-image='):values.append(a.split('=',1)[1])
                if values!=[expected]:raise ValueError('deployed payload differs from lock: '+key)
            else:raise ValueError('unrecognized system image lock entry: '+key)
            receipt={'component':key,'uid':o['metadata']['uid'],'generation':o['metadata']['generation'],'image':expected}
            if len(parts)==5 and parts[1]=='daemonsets':receipt.update(eligible_nodes=desired,ready_nodes=ready)
            receipts.append(receipt)
        save(self.d/'live-system-lock-verification.json',{'at':datetime.now(timezone.utc).isoformat(),'comparison_lock_sha256':self.run['comparison_lock_sha256'],'components':receipts})
    def owned(self,cluster):
        xs=self.get(cluster,'nodeprovisions')['items'];uid=self.run['policy_uid']
        if any(x['metadata'].get('labels',{}).get('training.dcnlab.com/policy-uid')!=uid for x in xs):raise ValueError('foreign provider in dedicated namespace')
        return xs
    def provision(self):
        self.mutate_gate();p=self.policy();r=self.run
        if p['metadata'].get('deletionTimestamp'):raise ValueError('deleting policy')
        patch=[{'op':'test','path':'/metadata/uid','value':r['policy_uid']},{'op':'add','path':'/metadata/annotations/training.dcnlab.com~1suspend','value':'false'}]
        self.kub('karmada','-n',r['namespace'],'patch','noderequest',r['policy'],'--type=json','--patch-file=/dev/stdin',obj=patch)
        r.update(phase='provisioning',capacity_requested_at=datetime.now(timezone.utc).isoformat());save(self.d/'run.json',r)
    def start(self):
        self.mutate_gate();r=self.run;p=self.policy();xs=self.owned('aws')
        if len(xs)!=4 or any(x.get('status',{}).get('phase')!='Ready' for x in xs):raise ValueError('four Ready owned providers required')
        if any(x.get('spec',{}).get('instanceType')!=r['instance_type'] for x in xs):raise ValueError('mixed/unexpected GPU instance type')
        known=ownership(xs,r['policy_uid']);rows=self.cloud.observe(known,datetime.now(timezone.utc).isoformat())
        if len(rows)!=4 or any(x['state']!='running' or not x['provider_ownership_verified'] for x in rows):raise ValueError('four running UID-verified providers required')
        actual=self.cloud.command('ec2','describe-instances','--instance-ids',*sorted(known))
        actual=[i for reservation in actual['Reservations'] for i in reservation['Instances']]
        if {i['InstanceId'] for i in actual}!=set(known) or len(actual)!=4 or any(i['InstanceType']!=r['instance_type'] or i['Placement']['AvailabilityZone']!=r['availability_zone'] or i.get('InstanceLifecycle','on-demand')!=('spot' if known[i['InstanceId']]['market']=='Spot' else 'on-demand') for i in actual):
            raise ValueError('actual EC2 hardware/AZ/market differs from owned configuration')
        save(self.d/'instance-ownership.json',known)
        nodes={x['status']['nodeName'] for x in xs}
        if len(nodes)!=4:raise ValueError('one VM per rank required')
        ods=sorted([x for x in xs if x['spec']['marketType']=='OnDemand'],key=lambda x:x['metadata']['name'])
        if not ods:raise ValueError('rank0 OD anchor required')
        if r['arm']=='A' and len(ods)!=4:raise ValueError('A must be allOD')
        if r['arm']!='A' and len(ods)==4:raise ValueError('B/C initial Spot exposure missing')
        old=self.get('karmada','statefulset',r['app'])
        if old is not None:raise ValueError('start is not replayable; existing workload requires reconciliation')
        manifests=json.loads((self.d/'karmada-resources.json').read_text());sts=next(o for o in manifests if o['kind']=='StatefulSet')
        env={e['name']:e.get('value') for e in sts['spec']['template']['spec']['containers'][0]['env']}
        if env.get('WORLD_SIZE')!='4' or sts['spec'].get('replicas')!=0:raise ValueError('suspended four-rank workload contract required')
        anchor=ods[0]['status']['nodeName'];cordoned=[]
        try:
            for node in nodes-{anchor}:self.kub('aws','cordon',node);cordoned.append(node)
            created=json.loads(self.kub('karmada','create','-f','-','-o','json',obj=sts));wid=created['metadata']['uid'];r['workload_uid']=wid
            self.kub('karmada','-n',r['namespace'],'patch','statefulset',r['app'],'--type=merge','--patch-file=/dev/stdin',obj={'metadata':{'labels':{'training.dcnlab.com/workload-uid':wid}},'spec':{'template':{'metadata':{'labels':{'training.dcnlab.com/workload-uid':wid}}}}})
            self.kub('karmada','-n',r['namespace'],'patch','noderequest',r['policy'],'--type=json','--patch-file=/dev/stdin',obj=[{'op':'test','path':'/metadata/uid','value':r['policy_uid']},{'op':'add','path':'/spec/workloadRef/uid','value':wid}])
            save(self.d/'run.json',r);self.kub('karmada','-n',r['namespace'],'scale','statefulset',r['app'],'--replicas=1')
            end=min(time.monotonic()+240,time.monotonic()+max(0,(timestamp(r['cleanup_start_utc'])-datetime.now(timezone.utc)).total_seconds()))
            while time.monotonic()<end:
                pod=self.get('aws','pod',r['app']+'-0')
                if pod and pod['spec'].get('nodeName'):
                    if pod['spec']['nodeName']!=anchor:raise ValueError('rank0 not on pinned OD')
                    break
                time.sleep(3)
            else:raise RuntimeError('rank0 placement timeout')
            for node in list(cordoned):self.kub('aws','uncordon',node);cordoned.remove(node)
            self.kub('karmada','-n',r['namespace'],'scale','statefulset',r['app'],'--replicas=4')
            policy=self.policy();workload=self.get('karmada','statefulset',r['app'])
            r.update(phase='running',ready_for_paid_execution=True,initial_instance_ids=sorted(known),training_requested_at=datetime.now(timezone.utc).isoformat(),live_spec_sha256=canonical_hash({'policy_spec':policy['spec'],'workload_spec':workload['spec']}));save(self.d/'run.json',r)
        finally:
            for node in cordoned:self.kub('aws','uncordon',node)
    def quiesce(self):
        # Cleanup remains allowed after the paid deadline. No finalizer bypass.
        r=self.run;p=self.get('karmada','noderequest',r['policy']);known={}
        for cluster in ('karmada','aws'):
            xs=self.owned(cluster)
            if cluster=='aws':known=ownership(xs,r['policy_uid'],json.loads((self.d/'instance-ownership.json').read_text()) if (self.d/'instance-ownership.json').exists() else {})
        save(self.d/'cleanup-owned-instance-identities.json',known)
        if p:
            if p['metadata']['uid']!=r['policy_uid']:raise ValueError('recreated policy; refuse cleanup')
            self.kub('karmada','-n',r['namespace'],'patch','noderequest',r['policy'],'--type=json','--patch-file=/dev/stdin',obj=[{'op':'test','path':'/metadata/uid','value':r['policy_uid']},{'op':'add','path':'/metadata/annotations/training.dcnlab.com~1suspend','value':'true'}])
        sts=self.get('karmada','statefulset',r['app'])
        if sts:
            if sts['metadata']['uid']!=r.get('workload_uid'):raise ValueError('recreated workload; refuse cleanup')
            self.delete_owned('karmada','statefulsets',sts)
        if p:self.delete_owned('karmada','noderequests',p)
        for x in self.owned('karmada'):self.delete_owned('karmada','nodeprovisions',x)
        save(self.d/'cleanup-requested.json',{'at':datetime.now(timezone.utc).isoformat(),'state':'REQUESTED_NOT_VERIFIED','preserved':['PVC','PV','NFS','Namespace'],'owned_instance_ids':list(known)})
    def verify_cleanup(self):
        r=self.run
        if self.owned('karmada') or self.owned('aws'):raise ValueError('normal provider finalizers still pending')
        known=json.loads((self.d/'cleanup-owned-instance-identities.json').read_text());rows=self.cloud.observe(known,datetime.now(timezone.utc).isoformat())
        if len(rows)!=len(known) or {x.get('instance_id') for x in rows}!=set(known):raise ValueError('positive exact EC2 termination not yet verified')
        absent={x['instance_id']:known[x['instance_id']] for x in rows if x['state']=='NOT_OBSERVED'}
        prior=historical_termination_receipts(self.d,r,absent)
        if len(rows)!=len(known) or any(not ((x['state']=='terminated' and x.get('provider_ownership_verified')) or (x['state']=='NOT_OBSERVED' and x['instance_id'] in prior)) for x in rows):raise ValueError('positive exact EC2 termination not yet verified')
        save(self.d/'exact-ec2-cleanup-verification.json',{'at':datetime.now(timezone.utc).isoformat(),'instances':rows,'historical_positive_termination_receipts':prior,'EBS_keypair_IAM_webhook_cleanup':'must separately verify recorded temporary resources; not implied by EC2 state'})

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('command',choices=['provision','start','quiesce','verify-cleanup']);p.add_argument('--directory',type=Path,required=True);p.add_argument('--profile',required=True);p.add_argument('--karmada-kubeconfig',required=True);p.add_argument('--member-kubeconfig',required=True)
    p.add_argument('--mgmt-kubeconfig');p.add_argument('--onprem-kubeconfig')
    a=p.parse_args();c=Control(a.directory,{'karmada':a.karmada_kubeconfig,'aws':a.member_kubeconfig,'mgmt':a.mgmt_kubeconfig,'onpre1':a.onprem_kubeconfig},a.profile);getattr(c,a.command.replace('-','_'))();print(a.command+' completed; inspect receipts')
if __name__=='__main__':main()
