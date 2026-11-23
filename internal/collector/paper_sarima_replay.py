"""Serve frozen historical forecasts on an explicit, uncompressed live clock.

No estimator changes, interpolation, future-hour publication, or VM actions.
Historical provenance remains distinct from the current publication clock.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
from pathlib import Path
import re
import ssl


def instant(value):
    t = datetime.fromisoformat(value.replace('Z', '+00:00'))
    if t.tzinfo is None:
        raise ValueError('timezone required')
    return t.astimezone(timezone.utc)


def load_locked(forecast_path, manifest, selection_evidence_path=None):
    body = Path(forecast_path).read_bytes()
    if manifest.get('status') != 'LOCKED' or manifest.get('forecast_sha256') != hashlib.sha256(body).hexdigest():
        raise ValueError('locked manifest and exact forecast hash required')
    data = json.loads(body)
    if data.get('training_hours') != 672 or data.get('held_out_hours') != 168 or data.get('model') != 'SARIMA(1,1,1)(1,0,1)[24]':
        raise ValueError('unexpected estimator/data split')
    if manifest.get('trace_sha256') != data['normalization']['trace_sha256']:
        raise ValueError('trace hash mismatch')
    post_risk=manifest.get('selection_rule')=='researcher-approved-post-risk-durable-window'
    selected=manifest.get('selection_rule')=='first-native-eligible-adjacent-transition' or post_risk
    if manifest.get('wall_seconds_per_trace_hour') != 3600:
        raise ValueError('uncompressed clock required')
    if selected:
        start=manifest.get('start_hour')
        if type(start) is not int or not 0<=start<167 or not selection_evidence_path:
            raise ValueError('reviewed offset and selection evidence required')
        evidence_body=Path(selection_evidence_path).read_bytes()
        if hashlib.sha256(evidence_body).hexdigest()!=manifest.get('selection_evidence_sha256'):
            raise ValueError('selection evidence hash mismatch')
        evidence=json.loads(evidence_body)
        if (evidence.get('forecast_sha256')!=manifest['forecast_sha256']
            or evidence.get('candidate_window',{}).get('start_hour')!=start
            or evidence.get('candidate_window',{}).get('change_hour')!=start+1):
            raise ValueError('selection evidence does not bind this forecast/adjacent window')
        if post_risk:
            approval=manifest.get('researcher_approval_sha256','')
            if (start!=9 or evidence.get('purpose')!='E1-L/E2-L'
                or evidence.get('status')!='RESEARCHER_APPROVED'
                or not isinstance(approval,str) or not re.fullmatch(r'[a-f0-9]{64}',approval)
                or evidence.get('researcher_approval_sha256')!=approval):
                raise ValueError('post-risk hour9/10 input requires explicit researcher-approved selection evidence')
    elif manifest.get('selection_rule') != 'first-held-out-hour' or manifest.get('start_hour') != 0:
        raise ValueError('explicit reviewed selection required')
    if manifest.get('provenance') != 'external-US-V100-availability-proxy' or not manifest.get('population_assumption'):
        raise ValueError('explicit external proxy provenance required')
    n = data['normalization']['risk_population']
    if type(n) is not int or n <= 0:
        raise ValueError('positive population required')
    rows = data['forecasts']
    if len(rows) != 168:
        raise ValueError('complete held-out forecasts required')
    for i, row in enumerate(rows):
        a, h = row['aggregate_forecast_per_hour'], row['lambda_per_hour']
        if row['evaluation_hour'] != i or row.get('future_observations_used') is not False or not all(type(x) in (float, int) and math.isfinite(x) and x >= 0 for x in (a, h)):
            raise ValueError('invalid or noncausal forecast')
        target = instant(row['relative_forecast_hour'])
        if instant(row['last_input_hour']) >= target or (i and target != instant(rows[i-1]['relative_forecast_hour']) + timedelta(hours=1)):
            raise ValueError('forecast timeline is not contiguous/causal')
        if abs(h - min(a/n, 100.0)) > 1e-9:
            raise ValueError('unexpected per-instance normalization/cap')
    return data


def payload(data, manifest, start, now):
    elapsed = (now - start).total_seconds()
    index = math.floor(elapsed/3600)
    base=manifest.get('start_hour',0)
    if type(base) is not int or base<0 or index < 0 or base+index >= len(data['forecasts']):
        raise ValueError('replay not started or exhausted')
    row = data['forecasts'][base+index]
    at = start + timedelta(hours=index)
    return {'lambdaPerHour': row['lambda_per_hour'], 'observedAt': at.isoformat(),
            'validUntil': (at+timedelta(hours=1)).isoformat(),
            'source': {'type':'paper-sarima', 'provenance':'paper-availability-count-sarima',
                       'rollingMeanHours':3, 'retrainWindowWeeks':4, 'seasonalPeriodHours':24,
                       'riskPopulation':data['normalization']['risk_population'], 'maxLambdaPerHour':100,
                       'aggregateForecastPreemptionsPerHour':row['aggregate_forecast_per_hour'],
                       'perInstanceLambdaApproximation':manifest['population_assumption'],
                       'inputKind':'historical-external-proxy-replay', 'forecastSHA256':manifest['forecast_sha256'],
                       'traceSHA256':manifest['trace_sha256'], 'historicalForecastHour':row['relative_forecast_hour'],
                       'historicalLastInputHour':row['last_input_hour'], 'replayStartUTC':start.isoformat(),
                       'replayHour':index, 'heldOutHour':base+index, 'selectedStartHour':base,
                       'selectionEvidenceSHA256':manifest.get('selection_evidence_sha256'),
                       'wallSecondsPerTraceHour':3600,
                       'notMeasuredSeoulRisk':True}}


def attach_prices(result, prices, now, *, instance_type="g5.xlarge", availability_zone="ap-northeast-2c"):
    if (not isinstance(instance_type,str) or not re.fullmatch(r'[a-z][a-z0-9]*\.[a-z0-9]+',instance_type)
        or not isinstance(availability_zone,str) or not re.fullmatch(r'[a-z]{2}(?:-[a-z0-9]+)+-\d[a-z]',availability_zone)):
        raise ValueError('explicit execution product required')
    at = instant(prices['observedAt'])
    if prices.get('currency') != 'USD' or not 0 <= (now-at).total_seconds() <= 600:
        raise ValueError('price observation stale/future or currency mismatch')
    if prices.get('availabilityZone') != availability_zone or prices.get('instanceType') != instance_type:
        raise ValueError('price execution product mismatch')
    for field in ('spotPricePerHour','onDemandPricePerHour'):
        v=prices[field]
        if type(v) not in (int,float) or not math.isfinite(v) or v<=0:raise ValueError('invalid price')
        result[field]=v
    result['source']['priceObservedAt']=prices['observedAt']
    result['source']['priceInstanceType']=instance_type
    result['source']['priceAvailabilityZone']=availability_zone
    result['source']['ondemandReceiptSHA256']=prices['ondemandReceiptSHA256']
    return result


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--forecasts',type=Path,required=True);p.add_argument('--manifest',type=Path,required=True)
    p.add_argument('--start-utc',required=True);p.add_argument('--bind',default='127.0.0.1');p.add_argument('--port',type=int,default=8443)
    p.add_argument('--selection-evidence',type=Path,help='exact hashed native-policy selection receipt for nonzero reviewed offset')
    p.add_argument('--prices',type=Path,help='optional current-price file; refresh every <=5min; absent means throughput-only bootstrap')
    p.add_argument('--execution-instance-type',default='g5.xlarge',help='must match the execution profile and price receipt; does not change historical trace provenance')
    p.add_argument('--execution-availability-zone',default='ap-northeast-2c',help='must match the execution profile and price receipt')
    p.add_argument('--cert',required=True);p.add_argument('--key',required=True)
    a=p.parse_args();manifest=json.loads(a.manifest.read_text());data=load_locked(a.forecasts,manifest,a.selection_evidence);start=instant(a.start_utc)
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path != '/risk':self.send_error(404);return
            try:
                now=datetime.now(timezone.utc)
                result=payload(data,manifest,start,now)
                if a.prices:result=attach_prices(result,json.loads(a.prices.read_text()),now,instance_type=a.execution_instance_type,availability_zone=a.execution_availability_zone)
                body=json.dumps(result,allow_nan=False).encode()
            except (ValueError, KeyError, OSError):self.send_error(503,'risk or price evidence unavailable');return
            self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Cache-Control','no-store');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
        def log_message(self,*args):pass
    server=ThreadingHTTPServer((a.bind,a.port),Handler);context=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);context.minimum_version=ssl.TLSVersion.TLSv1_2;context.load_cert_chain(a.cert,a.key);server.socket=context.wrap_socket(server.socket,server_side=True);server.serve_forever()


if __name__=='__main__':main()
