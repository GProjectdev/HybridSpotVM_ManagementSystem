"""Direct app pause/resume evidence; never infer pause from an iteration gap."""
try:
    from .collect import timestamp
    from .ieee_v2 import interval_union
except ImportError:
    from collect import timestamp
    from ieee_v2 import interval_union


def analyze(events):
    pending, closed, seen = {}, [], set()
    orphan_resumes = 0
    for e in sorted(events,key=lambda e:timestamp(e['timestamp'])):
        if e['event'] not in {'checkpoint_pause','training_resumed'}:
            continue
        identity=(e['checkpoint_id'],e['rank'])
        key=(identity,e['event'],e['timestamp'],e['pod_uid'],e['attempt_id'])
        if key in seen:continue
        seen.add(key)
        if e['event']=='checkpoint_pause':
            # Repeated observations cannot move the original pause boundary.
            pending.setdefault(identity,e)
        elif identity in pending:
            pause=pending.pop(identity)
            if timestamp(e['timestamp'])<timestamp(pause['timestamp']):
                raise ValueError('resume before pause')
            closed.append({'checkpoint_id':identity[0],'rank':identity[1],
                           'start':pause['timestamp'],'end':e['timestamp'],
                           'source_pod_uid':pause['pod_uid'],'target_pod_uid':e['pod_uid']})
        else:orphan_resumes+=1
    complete=not pending and orphan_resumes==0 and bool(closed)
    return {'semantics':'union of observed per-rank application pause-to-resume boundaries; includes recovery wait, not pure checkpoint I/O',
            'complete':complete,'intervals':closed,'unmatched_pauses':len(pending),'unmatched_resumes':orphan_resumes,
            'observed_unavailability_seconds':interval_union([(e['start'],e['end']) for e in closed]) if closed else None}
