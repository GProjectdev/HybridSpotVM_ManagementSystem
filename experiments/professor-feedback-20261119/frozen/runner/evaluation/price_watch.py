"""Bounded price refresh and immutable receipts for a single evaluation Run.

Publishes only prices.json to an explicitly UID-bound, run-labelled ConfigMap.
Does not create VM/FIS resources or alter forecast/model/clock inputs.
"""
import argparse,json,os,subprocess,time,uuid
from datetime import datetime,timezone
from pathlib import Path
try:
    from .ieee_v2_prices import refresh
    from .ieee_v2_ledger import AWSReader
    from .collect import timestamp
except ImportError:
    from ieee_v2_prices import refresh
    from ieee_v2_ledger import AWSReader
    from collect import timestamp


def publish(snapshot,*,kubeconfig,namespace,name,uid,run_id,runner=subprocess.run):
    base=['kubectl','--kubeconfig='+kubeconfig,'--request-timeout=15s','-n',namespace]
    p=runner(base+['get','configmap',name,'-o','json'],text=True,capture_output=True,timeout=25)
    if p.returncode:raise RuntimeError('price ConfigMap unavailable')
    obj=json.loads(p.stdout);m=obj['metadata']
    if m['uid']!=uid or m.get('labels',{}).get('training.dcnlab.com/evaluation-run')!=run_id:
        raise ValueError('price ConfigMap UID/run label mismatch')
    patch=[{'op':'test','path':'/metadata/uid','value':uid},{'op':'test','path':'/metadata/resourceVersion','value':m['resourceVersion']},
           {'op':'add','path':'/data/prices.json','value':json.dumps(snapshot,sort_keys=True)}]
    p=runner(base+['patch','configmap',name,'--type=json','--patch-file=/dev/stdin'],input=json.dumps(patch),text=True,capture_output=True,timeout=25)
    if p.returncode:raise RuntimeError('price publication failed; preserve previous timestamp, do not bypass UID/RV checks')


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--directory',type=Path,required=True);p.add_argument('--profile',required=True);p.add_argument('--account',required=True);p.add_argument('--ondemand-receipt',type=Path,required=True);p.add_argument('--deadline-utc',required=True);p.add_argument('--interval',type=int,default=240);p.add_argument('--seconds',type=int,default=0,help='0=single refresh; otherwise bounded <=5 hours')
    for x in ('publish-kubeconfig','publish-namespace','configmap','configmap-uid'):p.add_argument('--'+x)
    a=p.parse_args();info=json.loads((a.directory/'run.json').read_text())
    if info.get('protocol') not in {'ieee-access-final-20261117','HybridSpotDDP-E1E2-ABC-all4rank-v3-20261118'} or info.get('expected_aws_account')!=a.account:raise ValueError('explicit final Run/account required')
    if info.get('region')!='ap-northeast-2' or info.get('availability_zone')!='ap-northeast-2c':raise ValueError('reviewed execution location required')
    if not 30<=a.interval<=300 or not 0<=a.seconds<=18000:raise ValueError('refresh every <=5 minutes within bounded 5-hour Run')
    binding=[a.publish_kubeconfig,a.publish_namespace,a.configmap,a.configmap_uid]
    if any(binding) and not all(binding):raise ValueError('all explicit publication identity fields required')
    deadline=timestamp(a.deadline_utc)
    if (deadline-datetime.now(timezone.utc)).total_seconds()<120:raise ValueError('insufficient stop reserve; no price query started')
    if info.get('hard_deadline_utc') and deadline>timestamp(info['hard_deadline_utc']):raise ValueError('watch must not exceed Run deadline')
    finish=min(time.monotonic()+a.seconds,time.monotonic()+max(0,(deadline-datetime.now(timezone.utc)).total_seconds()))
    reader=AWSReader(a.profile,a.account,info['region']);history=a.directory/'price-history';os.umask(0o077);history.mkdir(exist_ok=True,mode=0o700)
    have_run=False
    while True:
        if have_run and time.monotonic()>=finish:break
        have_run=True
        now=datetime.now(timezone.utc)
        # Reserve enough time for bounded AWS and Kubernetes calls before stop.
        if (deadline-now).total_seconds()<120:break
        snapshot=refresh(reader,a.ondemand_receipt,info['availability_zone'],now,info['instance_type'])
        immutable=history/(now.strftime('%Y%m%dT%H%M%S.%fZ')+'-'+uuid.uuid4().hex[:8]+'.json')
        with immutable.open('x') as f:json.dump(snapshot,f,indent=2)
        if all(binding):publish(snapshot,kubeconfig=a.publish_kubeconfig,namespace=a.publish_namespace,name=a.configmap,uid=a.configmap_uid,run_id=info['run_id'])
        tmp=a.directory/('prices-'+uuid.uuid4().hex+'.tmp');tmp.write_text(json.dumps(snapshot,indent=2));tmp.replace(a.directory/'prices.current.json')
        print(json.dumps({'observedAt':snapshot['observedAt'],'receipt':str(immutable),'published':all(binding)}),flush=True)
        remaining=min(finish-time.monotonic(),(deadline-datetime.now(timezone.utc)).total_seconds()-120)
        if a.seconds==0 or remaining<=0:break
        time.sleep(min(a.interval,remaining))

if __name__=='__main__':main()
