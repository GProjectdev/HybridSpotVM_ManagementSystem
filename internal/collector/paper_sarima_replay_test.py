from datetime import datetime,timedelta,timezone
import hashlib,json,tempfile,unittest
from pathlib import Path
import paper_sarima_replay as replay

class ReplayTests(unittest.TestCase):
    def setUp(self):
        self.origin=datetime(1970,2,1,tzinfo=timezone.utc)
        self.data={'training_hours':672,'held_out_hours':168,'model':'SARIMA(1,1,1)(1,0,1)[24]',
                   'normalization':{'trace_sha256':'a'*64,'risk_population':1},
                   'forecasts':[{'evaluation_hour':i,'future_observations_used':False,
                     'relative_forecast_hour':(self.origin+timedelta(hours=i)).isoformat(),
                     'last_input_hour':(self.origin+timedelta(hours=i-1)).isoformat(),
                     'aggregate_forecast_per_hour':i/100,'lambda_per_hour':i/100} for i in range(168)]}
        self.body=json.dumps(self.data).encode()
        self.manifest={'status':'LOCKED','forecast_sha256':hashlib.sha256(self.body).hexdigest(),
                       'trace_sha256':'a'*64,'selection_rule':'first-held-out-hour','start_hour':0,
                       'wall_seconds_per_trace_hour':3600,'provenance':'external-US-V100-availability-proxy',
                       'population_assumption':'one availability slot; not running instance exposure'}
    def test_exact_hour_boundaries_and_exhaustion(self):
        start=datetime(2026,11,18,tzinfo=timezone.utc)
        for secs,hour in [(0,0),(3599,0),(3600,1),(604799,167)]:
            p=replay.payload(self.data,self.manifest,start,start+timedelta(seconds=secs))
            self.assertEqual(p['source']['replayHour'],hour)
            self.assertEqual(p['lambdaPerHour'],hour/100)
            self.assertTrue(p['source']['notMeasuredSeoulRisk'])
            self.assertEqual(p['source']['historicalForecastHour'],self.data['forecasts'][hour]['relative_forecast_hour'])
        for secs in [-1,604800]:
            with self.assertRaises(ValueError):replay.payload(self.data,self.manifest,start,start+timedelta(seconds=secs))
    def test_lock_hash_selection_and_causality(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'forecasts.json';p.write_bytes(self.body)
            replay.load_locked(p,self.manifest)
            for fault in [{'status':'PREVIEW'},{'forecast_sha256':'b'*64},{'wall_seconds_per_trace_hour':60},{'start_hour':65},{'trace_sha256':'b'*64}]:
                with self.assertRaises(ValueError):replay.load_locked(p,dict(self.manifest,**fault))
            self.data['forecasts'][1]['last_input_hour']=self.data['forecasts'][1]['relative_forecast_hour']
            p.write_text(json.dumps(self.data));m=dict(self.manifest,forecast_sha256=hashlib.sha256(p.read_bytes()).hexdigest())
            with self.assertRaises(ValueError):replay.load_locked(p,m)

    def test_prices_must_be_fresh_real_execution_product(self):
        now=datetime(2026,11,17,tzinfo=timezone.utc)
        prices={'observedAt':now.isoformat(),'currency':'USD','availabilityZone':'ap-northeast-2c','instanceType':'g5.xlarge','spotPricePerHour':.6,'onDemandPricePerHour':1.237,'ondemandReceiptSHA256':'a'*64}
        self.assertEqual(replay.attach_prices({'source':{}},prices,now)['onDemandPricePerHour'],1.237)
        for change in [{'observedAt':(now-timedelta(seconds=601)).isoformat()},{'spotPricePerHour':float('nan')},{'availabilityZone':'us-east-2a'}]:
            with self.assertRaises(ValueError):replay.attach_prices({'source':{}},dict(prices,**change),now)

    def test_g6_price_requires_explicit_execution_product_and_preserves_proxy(self):
        now=datetime(2026,11,18,tzinfo=timezone.utc)
        prices={'observedAt':now.isoformat(),'currency':'USD','availabilityZone':'ap-northeast-2c','instanceType':'g6.xlarge','spotPricePerHour':.47,'onDemandPricePerHour':.9896,'ondemandReceiptSHA256':'a'*64}
        with self.assertRaises(ValueError):replay.attach_prices({'source':{}},prices,now)
        source={'notMeasuredSeoulRisk':True,'traceSHA256':'b'*64,'inputKind':'historical-external-proxy-replay'}
        result=replay.attach_prices({'source':dict(source)},prices,now,instance_type='g6.xlarge')
        self.assertEqual(result['source']['priceInstanceType'],'g6.xlarge')
        self.assertEqual(result['source']['priceAvailabilityZone'],'ap-northeast-2c')
        for key,value in source.items():self.assertEqual(result['source'][key],value)
        with self.assertRaises(ValueError):replay.attach_prices({'source':{}},prices,now,instance_type='g6.xlarge',availability_zone='us-east-2a')
        with self.assertRaises(ValueError):replay.attach_prices({'source':{}},prices,now,instance_type=None)

    def test_reviewed_nonzero_offset_preserves_full_hash_and_wall_clock(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'forecast.json';p.write_bytes(self.body)
            e=Path(d)/'selection.json';e.write_text(json.dumps({'forecast_sha256':self.manifest['forecast_sha256'],'candidate_window':{'start_hour':14,'change_hour':15}}))
            m=dict(self.manifest,selection_rule='first-native-eligible-adjacent-transition',start_hour=14,selection_evidence_sha256=hashlib.sha256(e.read_bytes()).hexdigest())
            data=replay.load_locked(p,m,e);start=datetime(2026,11,18,tzinfo=timezone.utc)
            self.assertEqual(len(data['forecasts']),168)
            for seconds,hour in [(0,14),(3599,14),(3600,15)]:
                result=replay.payload(data,m,start,start+timedelta(seconds=seconds))
                self.assertEqual(result['source']['heldOutHour'],hour)
                self.assertEqual(result['lambdaPerHour'],hour/100)
            with self.assertRaises(ValueError):replay.load_locked(p,m)
            e.write_text('{}')
            with self.assertRaises(ValueError):replay.load_locked(p,m,e)
