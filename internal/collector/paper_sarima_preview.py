"""Offline, causal 28-day/7-day preview using the existing SARIMA estimator.

Raw traces without timestamps/population are not silently normalized. A separate
manifest must explicitly name the relative clock and availability-unit population
assumption. Output is historical preview, never a live Seoul hazard feed.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path

import paper_sarima_feed as feed


def load_source(path, manifest):
    data = path.read_bytes()
    sha = hashlib.sha256(data).hexdigest()
    if manifest.get('trace_sha256') != sha:
        raise ValueError('source trace hash mismatch')
    raw = json.loads(data)
    if manifest.get('clock_kind') != 'explicit-relative-trace-clock':
        raise ValueError('explicit relative clock declaration required; do not invent actual observation timestamps')
    origin = feed.parse_timestamp(manifest['relative_origin'])
    population = manifest.get('risk_population')
    if type(population) is not int or population <= 0 or not manifest.get('population_assumption'):
        raise ValueError('explicit positive population and assumption required')
    gap = raw.get('metadata', {}).get('gap_seconds')
    gap = feed.finite_nonnegative_int(gap, 'gap_seconds')
    values = [feed.finite_nonnegative_int(x, 'availability') for x in raw['data']]
    return feed.validate_trace(feed.AvailabilityTrace(values, gap, population, origin))


def preview(trace, config=None):
    config = config or feed.EstimatorConfig()
    feed.validate_config(config)
    if config.retrain_window_weeks != 4 or config.min_history_hours != 672:
        raise ValueError('protocol locks four weeks / 672 complete history hours')
    rate = feed.hourly_preemption_rates(trace)
    if len(rate) < 672 + 168:
        raise ValueError('28-day history and following seven complete days required')
    results = []
    model = None
    # No fitting on evaluation outcomes; the existing weekly refit interval
    # means one parameter fit, then hourly state updates during seven days.
    for index in range(672, 672 + 168):
        past = rate.iloc[:index]
        aggregate, fitted = feed.forecast_aggregate_preemptions_per_hour(past, config, model)
        if model is None:
            model = fitted
        target = past.index[-1].to_pydatetime() + timedelta(hours=1)
        results.append({'relative_forecast_hour':target.isoformat(), 'evaluation_hour':index-672,
                        'last_input_hour':past.index[-1].isoformat(), 'history_hours_used':672,
                        'aggregate_forecast_per_hour':aggregate,
                        'lambda_per_hour':min(aggregate / trace.risk_population, config.max_lambda_per_hour),
                        'parameter_fit_count':1, 'future_observations_used':False})
    return results


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--trace', type=Path, required=True)
    p.add_argument('--normalization-manifest', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    a = p.parse_args()
    if a.out.exists():
        p.error('preview output exists; preserve original evidence')
    manifest = json.loads(a.normalization_manifest.read_text())
    trace = load_source(a.trace, manifest)
    results = preview(trace)
    payload = {'status':'PREVIEW_NOT_LOCKED', 'kind':'external-US-V100-availability-proxy',
               'meaning':'not measured Seoul A10G eviction risk or prediction accuracy',
               'normalization':manifest, 'gap_seconds':trace.gap_seconds,
               'training_hours':672, 'held_out_hours':168, 'wall_seconds_per_trace_hour':3600,
               'model':'SARIMA(1,1,1)(1,0,1)[24]', 'causal_rolling_mean_hours':3,
               'optimizer_maxiter':500,
               'vm_policy_divergence':'NOT_CHECKED; forecast preview alone does not establish runtime switching',
               'forecasts':results}
    a.out.parent.mkdir(parents=True,exist_ok=True)
    a.out.write_text(json.dumps(payload,indent=2)+'\n')
    print(json.dumps({'forecasts':len(results),'output':str(a.out),'status':payload['status']}))


if __name__=='__main__':main()
