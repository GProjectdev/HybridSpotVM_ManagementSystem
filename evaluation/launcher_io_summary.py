"""Summarize deduplicated launcher-local I/O; never interpret it as DDP stall."""
import argparse,csv,json,math
from pathlib import Path
try:
    from .ieee_v2 import canonical_hash
except ImportError:
    from ieee_v2 import canonical_hash


def summarize(events):
    seen=set();groups={}
    for e in events:
        if e.get('event')!='launcher_checkpoint_io':continue
        if not e.get('pod_uid') or not e.get('timestamp') or not e.get('run_id'):raise ValueError('I/O observation identity required')
        n,w,t=e.get('buffer_bytes'),e.get('written_bytes'),e.get('elapsed_seconds');mode=e.get('mode')
        if type(n)is not int or n<0 or type(w)is not int or type(t)not in (int,float) or not math.isfinite(t) or t<0 or mode not in ('reuse-identical','atomic-write') or w!=(0 if mode=='reuse-identical' else n):raise ValueError('invalid local I/O observation')
        key=canonical_hash(e)
        if key in seen:continue
        seen.add(key);group=(e['run_id'],e['pod_uid'])
        row=groups.setdefault(group,dict(run_id=group[0],pod_uid=group[1],observations=0,reused_files=0,atomic_writes=0,logical_buffer_bytes=0,written_bytes=0,sum_local_elapsed_seconds_not_union=0.0,scope='launcher-local I/O only; not DDP pause or full restore'))
        row['observations']+=1;row['reused_files']+=mode=='reuse-identical';row['atomic_writes']+=mode=='atomic-write';row['logical_buffer_bytes']+=n;row['written_bytes']+=w;row['sum_local_elapsed_seconds_not_union']+=t
    return {'status':'OBSERVATIONS_PRESENT' if seen else 'NO_OBSERVATIONS','rows':list(groups.values()),'semantics':'duplicate observer samples removed; per-rank elapsed sums are not wall-clock union; no end-to-end speedup inferred'}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--events',type=Path,required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args()
    if a.out.exists():p.error('preserve existing export; choose a new output')
    r=summarize(json.loads(x) for x in a.events.read_text().splitlines() if x.strip())
    fields=['run_id','pod_uid','observations','reused_files','atomic_writes','logical_buffer_bytes','written_bytes','sum_local_elapsed_seconds_not_union','scope']
    with a.out.open('x') as f:w=csv.DictWriter(f,fieldnames=fields);w.writeheader();w.writerows(r['rows'])
    print(json.dumps({'status':r['status'],'semantics':r['semantics']}))

if __name__=='__main__':main()
