"""Observed application checkpoint cadence; not archive I/O or scheduler latency."""
try:
    from .collect import timestamp
except ImportError:
    from collect import timestamp


def summarize(events,world_size=2):
    groups={}
    for e in events:
        if e.get('event') not in {'checkpoint_pause','checkpoint_saved','training_resumed'}:continue
        cid=e.get('checkpoint_id');rank=e.get('rank')
        if not cid or type(rank)is not int or not 0<=rank<world_size:raise ValueError('invalid checkpoint identity/rank')
        groups.setdefault(cid,{}).setdefault(rank,{}).setdefault(e['event'],[]).append(timestamp(e['timestamp']))
    rounds=[];incomplete=[]
    for cid,ranks in groups.items():
        if set(ranks)!=set(range(world_size)) or any(not all(ranks[r].get(k) for k in ('checkpoint_pause','checkpoint_saved','training_resumed')) for r in ranks):
            incomplete.append(cid);continue
        boundaries=[]
        for r in ranks:
            pause=min(ranks[r]['checkpoint_pause']);saved=min(ranks[r]['checkpoint_saved']);resume=min(t for t in ranks[r]['training_resumed'])
            if not pause<=saved<=resume:raise ValueError('invalid per-rank checkpoint ordering')
            boundaries.append((pause,saved,resume))
        start=min(x[0] for x in boundaries);finish=max(x[2] for x in boundaries)
        rounds.append({'checkpoint_id':cid,'first_pause_utc':start.isoformat(),'last_resume_utc':finish.isoformat(),'application_pause_span_seconds':(finish-start).total_seconds()})
    rounds.sort(key=lambda x:timestamp(x['first_pause_utc']))
    cadence=[]
    for prev,current in zip(rounds,rounds[1:]):
        gap=(timestamp(current['first_pause_utc'])-timestamp(prev['last_resume_utc'])).total_seconds()
        if gap<0:raise ValueError('overlapping full-group checkpoint rounds')
        cadence.append({'previous_checkpoint_id':prev['checkpoint_id'],'next_checkpoint_id':current['checkpoint_id'],'pause_start_to_pause_start_seconds':(timestamp(current['first_pause_utc'])-timestamp(prev['first_pause_utc'])).total_seconds(),'previous_application_resume_to_next_pause_seconds':gap})
    return {'semantics':'application pause/save/resume observations; interval setting is not equated with pause-start spacing; use CR completion/export timestamps for scheduler/archive timing','complete_rounds':rounds,'incomplete_checkpoint_ids':sorted(incomplete),'cadence':cadence}
