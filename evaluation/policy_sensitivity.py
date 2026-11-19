"""Replay frozen inputs through production DecideAt; performs no cloud mutations."""
import argparse
import copy
import csv
import hashlib
import json
import math
from pathlib import Path
import subprocess

ALPHAS = (0.7, 0.8, 0.9)


def requests(forecast, prices, base):
    rows = forecast['forecasts']
    if len(rows) != 168 or any(row['evaluation_hour'] != i or row.get('future_observations_used') is not False for i, row in enumerate(rows)):
        raise ValueError('complete existing causal held-out forecast required')
    result = []
    for instance in ('g5.2xlarge', 'g6.2xlarge'):
        price = prices[instance + '_price']
        if any(not math.isfinite(price[k]) or price[k] <= 0 for k in ('spotPricePerHour', 'onDemandPricePerHour')):
            raise ValueError('positive actual execution prices required')
        for world in (4, 2):
            for alpha in ALPHAS:
                for row in rows:
                    lam = row['lambda_per_hour']
                    if not math.isfinite(lam) or lam < 0:
                        raise ValueError('nonnegative existing hazard required')
                    policy = copy.deepcopy(base)
                    policy.pop('fixedOnDemand', None)
                    policy.update(method='paper-eq6-7', alpha=alpha, minOnDemand=1, forecastHorizonSeconds=3600)
                    # No fabricated or timestamp-relabelled recovery loss measurement.
                    policy['economics'] = dict(policy.get('economics', {}), enabled=True, lossCostPerEviction=-1)
                    identifier = '%s-w%d-a%.1f-h%03d' % (instance, world, alpha, row['evaluation_hour'])
                    result.append((dict(id=identifier, now=prices['at'], policy={
                        'apiVersion':'training.dcnlab.com/v1alpha1','kind':'NodeRequest',
                        'spec':{'targetWorkers':world,'policy':policy,
                                'checkpoint':{'minIntervalSeconds':60,'maxIntervalSeconds':600,'resume':True}}},
                        runtime={'WorldSize':world,'ReadyRanks':world},
                        risk={'Ready':True,'LambdaPerHour':lam,'SpotPricePerHour':price['spotPricePerHour'],
                              'OnDemandPricePerHour':price['onDemandPricePerHour']}),
                        dict(instance_type=instance, world_size=world, alpha=alpha, held_out_hour=row['evaluation_hour'],
                             lambda_per_hour=lam, spot_usd_per_hour=price['spotPricePerHour'],
                             od_usd_per_hour=price['onDemandPricePerHour'], price_observed_at=prices['at'],
                             input_kind='existing_US_V100_availability_proxy_forecast',
                             throughput_boundary_lambda_per_hour=-math.log(alpha),
                             forecast_horizon_seconds=3600)))
    return result


def analyze(forecast_path, prices_path, policy_path, out, runner=subprocess.run):
    out = Path(out)
    out.mkdir(parents=True, exist_ok=False)
    forecast, prices, config = (json.loads(Path(p).read_text()) for p in (forecast_path, prices_path, policy_path))
    items = requests(forecast, prices, config['existing_policy'])
    repository = Path(__file__).resolve().parents[1]
    process = runner(['go','run','./evaluation/variable_probe'],cwd=repository,
                     input=''.join(json.dumps(req)+'\n' for req,_ in items),text=True,capture_output=True,timeout=180)
    if process.returncode:
        raise RuntimeError('production policy adapter failed: '+process.stderr[-1000:])
    answers = [json.loads(line) for line in process.stdout.splitlines()]
    if len(answers) != len(items):raise ValueError('policy response count mismatch')
    rows=[]
    for (req, metadata), answer in zip(items,answers):
        if answer['id'] != req['id']:raise ValueError('policy response identity mismatch')
        decision=answer['decision']
        rows.append(dict(metadata,desired_od=decision['OnDemandWorkers'],desired_spot=decision['SpotWorkers'],
                         vm_policy_source=decision['VMPolicySource'],vm_policy_stage=decision['VMPolicyStage'],
                         price_evaluated=decision['PriceEvaluated'],economics_evaluated=decision['CostEvaluated'],
                         reason=decision['Reason'],checkpoint_interval_seconds=decision['CheckpointIntervalSeconds'],
                         interval_source=decision['IntervalSource'],interval_cost_evaluated=decision['IntervalCostEvaluated'],
                         observation_scope='offline policy result; not GPU time/cost or observed execution interval'))
    def save_csv(name, values, fields):
        with (out/name).open('w') as f:
            writer=csv.DictWriter(f,fieldnames=fields);writer.writeheader();writer.writerows(values)
    save_csv('policy_sensitivity.csv',rows,list(rows[0]))
    byhour={}
    for row in rows:
        key=(row['instance_type'],row['world_size'],row['held_out_hour'])
        byhour.setdefault(key,{})[row['alpha']]=row
    changes=[]
    for key, values in byhour.items():
        if len({r['desired_od'] for r in values.values()})>1:
            changes.append(dict(instance_type=key[0],world_size=key[1],held_out_hour=key[2],lambda_per_hour=values[0.8]['lambda_per_hour'],
                                od_alpha_07=values[0.7]['desired_od'],od_alpha_08=values[0.8]['desired_od'],od_alpha_09=values[0.9]['desired_od']))
    save_csv('alpha_decision_changes.csv',changes,['instance_type','world_size','held_out_hour','lambda_per_hour','od_alpha_07','od_alpha_08','od_alpha_09'])
    # Do not inject old costs from a different storage/payload baseline merely to
    # make an analytic branch run. A future eligible measurement can fill this gap.
    blocked=[dict(instance_type=r['instance_type'],world_size=r['world_size'],held_out_hour=r['held_out_hour'],alpha=r['alpha'],lambda_per_hour=r['lambda_per_hour'],checkpoint_seconds='',copy_seconds='',analytic_interval_seconds='',status='BLOCKED_NO_COMPARABLE_MEASURED_D',reason='new payload and completed gp3 baseline require same-configuration measured checkpoint round') for r in rows]
    save_csv('checkpoint_D_sensitivity_pending.csv',blocked,list(blocked[0]))
    summary={'kind':'OFFLINE_UNCHANGED_PRODUCTION_POLICY_SENSITIVITY','rows':len(rows),'alpha_changed_rows':len(changes),
             'alphas':ALPHAS,'world_sizes_are_separate_blocks':[4,2],'instance_types_are_separate_blocks':['g5.2xlarge','g6.2xlarge'],
             'all_vm_stages':sorted({r['vm_policy_stage'] for r in rows}),
             'all_interval_sources':sorted({r['interval_source'] for r in rows}),
             'economics_evaluated_rows':sum(r['economics_evaluated'] for r in rows),
             'actual_GPU_measurement':False,'checkpoint_D_analysis_complete':False,
             'inputs':{str(p):hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in (forecast_path,prices_path,policy_path)}}
    (out/'policy_sensitivity_summary.json').write_text(json.dumps(summary,indent=2)+'\n')
    return summary


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ('forecasts','prices','policy-config','out'):parser.add_argument('--'+name,type=Path,required=True)
    args=parser.parse_args()
    print(json.dumps(analyze(args.forecasts,args.prices,args.policy_config,args.out)))

if __name__=='__main__':main()
