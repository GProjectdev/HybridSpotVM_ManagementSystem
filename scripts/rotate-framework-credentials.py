#!/usr/bin/env python3
"""Rotate bounded TokenRequest credentials without logging or persisting tokens.
Administrator-run; existing RBAC is reused, never expanded. Controller clients
use tokenFile so Secret rotation requires no subsequent controller rollout.
"""
import argparse,base64,datetime,json,subprocess

def main():
 p=argparse.ArgumentParser();p.add_argument('--namespace',required=True);p.add_argument('--mgmt-kubeconfig',required=True);p.add_argument('--karmada-kubeconfig',required=True);p.add_argument('--member-kubeconfig',action='append',default=[]);a=p.parse_args()
 cfg={'mgmt':a.mgmt_kubeconfig,'karmada':a.karmada_kubeconfig};cfg.update(dict(v.split('=',1) for v in a.member_kubeconfig));ns=a.namespace
 def call(cluster,args,obj=None):
  r=subprocess.run(['kubectl','--kubeconfig='+cfg[cluster],*args,'--request-timeout=30s'],input=None if obj is None else json.dumps(obj),text=True,capture_output=True)
  if r.returncode:raise RuntimeError(f'{cluster}: credential operation {args[0]} failed (exit {r.returncode}); token output suppressed')
  return r.stdout
 def credential(cluster,sa,tokenfile):
  call(cluster,['get','serviceaccount',sa,'-n',ns])
  token=call(cluster,['create','token',sa,'-n',ns,'--duration=8h']).strip()
  claims=json.loads(base64.urlsafe_b64decode(token.split('.')[1]+'=='))
  if claims['exp']<datetime.datetime.now(datetime.timezone.utc).timestamp()+5*3600:raise RuntimeError('API token lifetime shorter than rotation safety margin')
  source=json.loads(call(cluster,['config','view','--raw','--flatten','-o','json']));context=next(x['context'] for x in source['contexts'] if x['name']==source['current-context']);endpoint=next(x['cluster'] for x in source['clusters'] if x['name']==context['cluster'])
  conf={'apiVersion':'v1','kind':'Config','clusters':[{'name':'target','cluster':endpoint}],'users':[{'name':sa,'user':{'tokenFile':tokenfile}}],'contexts':[{'name':'target','context':{'cluster':'target','user':sa,'namespace':ns}}],'current-context':'target'}
  return json.dumps(conf),token,claims['exp']
 def secret(cluster,name,data):
  # Replace only data with UID/resourceVersion protection; retain labels/owners.
  existing=json.loads(call(cluster,['get','secret',name,'-n',ns,'-o','json']))
  existing['data']={k:base64.b64encode(v.encode()).decode() for k,v in data.items()};existing.pop('stringData',None)
  call(cluster,['replace','-f','-'],existing)
 memberData={};expirations={}
 for cluster in cfg:
  if cluster in ['mgmt','karmada']:continue
  conf,token,exp=credential(cluster,'framework-observer',f'/access/{cluster}.token');memberData[cluster+'.kubeconfig']=conf;memberData[cluster+'.token']=token;expirations[cluster]=exp
 for component in ['training-run-controller','training-capacity-controller','training-transition-controller','training-run-checkpoint-coordinator','framework-risk-collector']:
  conf,token,exp=credential('karmada',component,'/access/control.token')
  data={'control.kubeconfig':conf,'control.token':token}
  if component!='framework-risk-collector':data.update(memberData)
  secret('mgmt',component+'-access',data);expirations[component]=exp
 conf,token,exp=credential('karmada','training-execution-gate','/training-access/token');secret('karmada','training-execution-gate-access',{'kubeconfig':conf,'token':token});expirations['training-execution-gate']=exp
 print(json.dumps({'at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'namespace':ns,'rotated':list(expirations),'earliestExpiry':datetime.datetime.fromtimestamp(min(expirations.values()),datetime.timezone.utc).isoformat(),'tokenMaterialLogged':False}))
if __name__=='__main__':main()
