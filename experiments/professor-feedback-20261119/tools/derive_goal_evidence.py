"""Read existing private pilot logs; export selected timing facts, never policy credentials.
No cluster/cloud mutation. Quantiles are observations, not confidence bounds.
"""
from pathlib import Path
import json,sys,datetime,statistics,csv,hashlib
D=Path(sys.argv[1]);O=Path(sys.argv[2]);O.mkdir(parents=True,exist_ok=True)
def read(p):return json.loads(p.read_text())
def at(s):return datetime.datetime.fromisoformat(s.replace('Z','+00:00'))
info=read(D/'run.json');cfg=read(D/'config.json');cutoff=read(D/'fixture-high-requested.json')['at'] if (D/'fixture-high-requested.json').exists() else None
unique={}
for line in (D/'training-events.jsonl').open():
 e=json.loads(line)
 if e.get('run_id')!=info['run_id'] or (cutoff and at(e['timestamp'])>=at(cutoff)):continue
 key=tuple(e.get(k) for k in ('pod_uid','attempt_id','rank','event','global_step','timestamp'))
 unique[key]=e
es=list(unique.values());snapshots=sorted((D/'snapshots').iterdir());policy=None;rounds={}
for sd in snapshots:
 p=sd/'karmada-noderequests.json'
 if policy is None and p.exists():
  for obj in read(p).get('items',[]):
   if obj['metadata']['uid']==info['policy_uid']:policy=obj
 p=sd/'karmada-fluidcrmigrations.json'
 if p.exists():
  for obj in read(p).get('items',[]):
   if obj['metadata'].get('labels',{}).get('training.dcnlab.com/policy-uid')!=info['policy_uid']:continue
   for st in obj.get('status',{}).get('clusters',[]):
    if st.get('phase')=='Completed' and len(st.get('pods',[]))==4 and st.get('startTime') and st.get('completionTime'):
     files=[f for p in st['pods'] for f in p.get('checkpointFiles',[]) if f.get('sha256') and f.get('exportedAt')]
     if len(files)>=4:rounds[obj['metadata']['uid']]=dict(uid=obj['metadata']['uid'],start_at=st['startTime'],completed_at=st['completionTime'],exported_at=max((f['exportedAt'] for f in files),key=at))
assert policy and rounds
origin=at(policy['metadata']['creationTimestamp']);rows=[]
for rank in range(4):
 steps=sorted((e for e in es if e.get('rank')==rank and e.get('event')=='step'),key=lambda e:at(e['timestamp']));assert steps
 pauses=sorted((e for e in es if e.get('rank')==rank and e.get('event')=='checkpoint_pause'),key=lambda e:at(e['timestamp']))
 resumes=sorted((e for e in es if e.get('rank')==rank and e.get('event')=='training_resumed'),key=lambda e:at(e['timestamp']))
 stall=None
 if pauses:
  following=[e for e in resumes if at(e['timestamp'])>at(pauses[0]['timestamp'])]
  if following:stall=(at(following[0]['timestamp'])-at(pauses[0]['timestamp'])).total_seconds()
 vals=sorted(e['step_seconds'] for e in steps if e['global_step']>=50 and e.get('step_seconds',0)>0)
 q=lambda f:vals[int(f*(len(vals)-1))]
 row=dict(model=cfg['model'],run_id=info['run_id'],rank=rank,policy_created_at=origin.isoformat(),first_step_at=steps[0]['timestamp'],startup_seconds=(at(steps[0]['timestamp'])-origin).total_seconds(),step_events=len(steps),last_step=steps[-1]['global_step'],q10_seconds=q(.1),median_seconds=statistics.median(vals),q90_seconds=q(.9),mean_seconds=statistics.mean(vals),first_checkpoint_pause_to_resume_seconds=stall,last_step_at=steps[-1]['timestamp'])
 if resumes:
  post=[e for e in steps if at(e['timestamp'])>at(resumes[0]['timestamp']) and e['global_step']>=50]
  if len(post)>1:row.update(post_resume_step_span=post[-1]['global_step']-post[0]['global_step'],post_resume_elapsed_seconds=(at(post[-1]['timestamp'])-at(post[0]['timestamp'])).total_seconds(),post_resume_seconds_per_step=(at(post[-1]['timestamp'])-at(post[0]['timestamp'])).total_seconds()/(post[-1]['global_step']-post[0]['global_step']))
 rows.append(row)
first=min(rounds.values(),key=lambda r:at(r['start_at']));first.update(checkpoint_seconds=(at(first['completed_at'])-at(first['start_at'])).total_seconds(),copy_seconds=(at(first['exported_at'])-at(first['completed_at'])).total_seconds(),durable_elapsed_seconds=(at(first['exported_at'])-origin).total_seconds())
summary=dict(model=cfg['model'],run_id=info['run_id'],policy_uid=info['policy_uid'],phase='initial 1OD+3Spot before diagnostic high; no phase compression used to estimate throughput',risk_clock_anchor=origin.isoformat(),first_checkpoint=first,rank_rows=rows,first_all_rank_step_elapsed_seconds=max(r['startup_seconds'] for r in rows),fast_iteration_observed_seconds=min(r['q10_seconds'] for r in rows),slow_iteration_observed_seconds=max(r['q90_seconds'] for r in rows),maximum_first_checkpoint_pause_seconds=max(r['first_checkpoint_pause_to_resume_seconds'] or 0 for r in rows),goal_frozen=False,formal_result=False,notes=['q10/q90 are empirical screening inputs, not probabilistic bounds or AWS supply guarantees','Iteration durations exclude portions of external VM wait, restore and checkpoint pause; do not label reciprocal median as E2E throughput','Startup measured from source NodeRequest creationTimestamp; run-specific provisioning delay must be checked at execution'])
(O/(cfg['model']+'-goal-evidence.json')).write_text(json.dumps(summary,indent=2)+'\n')
with (O/(cfg['model']+'-goal-evidence.csv')).open('w',newline='') as f:
 w=csv.DictWriter(f,fieldnames=list(rows[0]),lineterminator='\n');w.writeheader();w.writerows(rows)
print('GOAL_EVIDENCE_EXPORTED',cfg['model'],summary['first_all_rank_step_elapsed_seconds'],summary['fast_iteration_observed_seconds'],summary['slow_iteration_observed_seconds'],summary['maximum_first_checkpoint_pause_seconds'])
