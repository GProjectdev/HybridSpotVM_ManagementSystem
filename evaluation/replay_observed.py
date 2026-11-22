"""Reconstruct existing A/B02 time, price integrals and per-rank rollback.

Uses archived derived CSV, never alters original decisions or certifies live
fencing. This is an observed replay calibration check, not forward prediction.
"""
import argparse
import csv
from datetime import datetime
from decimal import Decimal as D
import hashlib
import json
from pathlib import Path

from .stateful_replay import integrate_price


def rows(path):
    with path.open(newline='') as f:
        return list(csv.DictReader(f))


def seconds(value):
    # Integer timedelta arithmetic preserves original microsecond timestamps.
    t=datetime.fromisoformat(value.replace('Z','+00:00'))
    if t.tzinfo is None:
        raise ValueError('timezone required')
    epoch=datetime.fromisoformat('1970-01-01T00:00:00+00:00')
    diff=t-epoch
    return D(diff.days*86400+diff.seconds)+D(diff.microseconds)/1000000


def reconstruct_ledger(path, spot_rate=None, od_rate=None):
    result=[]
    for row in rows(path):
        start=seconds(row['launch_utc'])
        end=seconds(row['first_positive_terminated_receipt_utc'])
        rate=row.get('price_USD_per_hour') or (spot_rate if row['market']=='Spot' else od_rate)
        if rate is None:
            raise ValueError('explicit historical market price needed')
        cost=integrate_price(start,end,[(start,rate)])
        reported=D(row['GPU_compute_upper_USD'])
        lower=row.get('last_positive_nonterminal_call_start_utc',row.get('last_nonterminal_observation_utc'))
        low=integrate_price(start,seconds(lower),[(start,rate)])
        result.append({'instance_id':row['instance_id'],'market':row['market'],
                       'lifetime_upper_seconds':str(end-start),'historical_USD_per_hour':str(rate),
                       'reconstructed_upper_USD':str(cost),'reported_upper_USD':str(reported),
                       'upper_residual_USD':str(cost-reported),
                       'reconstructed_lower_USD':str(low),
                       'lower_residual_USD':str(low-D(row['GPU_compute_lower_USD'])),
                       'invoice_verified':False})
    if len({r['instance_id'] for r in result}) != len(result):
        raise ValueError('duplicate instance')
    return result


def reconstruct_progress(path, step_field, time_field, goal, nr_created):
    rr=rows(path)
    result=[]
    for rank in range(4):
        rank_rows=[r for r in rr if int(r['rank'])==rank]
        first=min(seconds(r[time_field]) for r in rank_rows if int(r[step_field])==1)
        goals=[r for r in rank_rows if int(r[step_field])>=goal]
        if not goals:
            raise ValueError('not all ranks reached goal')
        target=min(seconds(r[time_field]) for r in goals)
        result.append({'rank':rank,'first_committed_step_epoch_seconds':str(first),
                       'goal_epoch_seconds':str(target),'goal_steps':goal})
    return {'rank_rows':result,
            'NR_to_goal_seconds':str(max(D(r['goal_epoch_seconds']) for r in result)-seconds(nr_created)),
            'NR_to_first_common_step_seconds':str(max(D(r['first_committed_step_epoch_seconds']) for r in result)-seconds(nr_created))}


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--evaluation',type=Path,required=True)
    ap.add_argument('--rules',type=Path,required=True,help='predeclared tolerances and measured NR anchors')
    ap.add_argument('--out',type=Path,required=True)
    args=ap.parse_args()
    if args.out.exists():
        raise ValueError('preserve prior output')
    rules=json.loads(args.rules.read_text())
    e=args.evaluation;b=e/'GPT2_A_B02_Results_20261120';a=e/'runs/gpt2-a-01/analysis'
    sources=[args.rules,a/'gpu_cost_ledger.csv',a/'rank_progress.csv',b/'gpu_cost_ledger.csv',b/'rank_progress.csv',b/'A_B_comparison.csv',b/'planned_reconfiguration_rank_progress.csv']
    before={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}
    comp=rows(b/'A_B_comparison.csv')[0]
    records={}
    for arm,base in [('A',a),('B02',b)]:
        bill=reconstruct_ledger(base/'gpu_cost_ledger.csv',**rules['historical_prices'])
        progress=reconstruct_progress(base/'rank_progress.csv','global_step','timestamp',4800,rules['NR_anchors_utc'][arm])
        cost=sum((D(r['reconstructed_upper_USD']) for r in bill),D(0))
        ref_arm='A' if arm=='A' else 'B'
        cost_residual=cost-D(comp[ref_arm+'_GPU_USD'])
        time_residual=D(progress['NR_to_goal_seconds'])-D(comp[ref_arm+'_T_E2E_seconds'])
        tests={'all_instance_integrals':all(abs(D(r['upper_residual_USD']))<=D(rules['max_cost_residual_USD']) and abs(D(r['lower_residual_USD']))<=D(rules['max_cost_residual_USD']) for r in bill),
               'total_cost':abs(cost_residual)<=D(rules['max_cost_residual_USD']),
               'all_rank_goal_time':abs(time_residual)<=D(rules['max_time_residual_seconds'])}
        records[arm]={'price_integrals':bill,'progress':progress,'GPU_upper_USD':str(cost),
                      'reported_cost_residual_USD':str(cost_residual),'reported_time_residual_seconds':str(time_residual),'tests':tests}
    rollbacks=[]
    for r in rows(b/'planned_reconfiguration_rank_progress.csv'):
        saved=int(r['checkpoint_saved_step']);old=int(r['last_source_observed_step']);resumed=int(r['first_target_observed_step'])
        rollbacks.append({'rank':int(r['rank']),'saved_step':saved,'last_source_step':old,'first_target_step':resumed,
                          'lost_commits_to_saved_round':old-saved,
                          'source_minus_first_target_step':old-resumed,
                          'next_commit_is_saved_plus_one':resumed==saved+1,
                          'matches_receipt_rollback':old-saved==int(r['observed_rollback_commits'])})
    if len(rollbacks)!=4 or {r['rank'] for r in rollbacks}!={0,1,2,3}:
        raise ValueError('complete per-rank replay mapping required')
    after={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}
    success=(before==after and all(all(r['tests'].values()) for r in records.values())
             and all(r['next_commit_is_saved_plus_one'] and r['matches_receipt_rollback'] for r in rollbacks))
    result={'kind':'OBSERVED_EVENT_ACCOUNTING_RECONSTRUCTION_NOT_FORWARD_PREDICTION',
            'results':records,'rollback':rollbacks,'inputs_sha256_before':before,'inputs_unchanged':before==after,
            'predeclared_rules':rules,'pass':success,'original_formal_valid_modified':False,
            'B02_researcher_adoption_pending':True,
            'scope':'Archived derived CSV consistency; not a fresh independent raw audit, cloud call, live fence verification or out-of-sample accuracy proof.',
            'stateful_forward_replay_validated':False}
    args.out.parent.mkdir(parents=True,exist_ok=True)
    with args.out.open('x') as f:json.dump(result,f,indent=2)
    print(json.dumps({'pass':success,'A_GPU_upper_USD':records['A']['GPU_upper_USD'],'B02_GPU_upper_USD':records['B02']['GPU_upper_USD'],'out':str(args.out)}))
    if not success:raise SystemExit(1)


if __name__=='__main__':main()
