from datetime import datetime, timezone
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import paper_sarima_feed as feed
import paper_sarima_preview as preview


class PreviewTests(unittest.TestCase):
    def test_each_forecast_receives_only_earlier_hours_and_one_parameter_fit(self):
        trace=feed.AvailabilityTrace([1]*841,3600,1,datetime(1970,1,1,tzinfo=timezone.utc))
        model=object();inputs=[]
        def forecast(past,config,previous):
            inputs.append((list(past.index),previous))
            return 0.1,model
        with patch.object(feed,'forecast_aggregate_preemptions_per_hour',side_effect=forecast):
            result=preview.preview(trace)
        self.assertEqual(len(result),168)
        self.assertEqual(len(inputs[0][0]),672)
        self.assertIsNone(inputs[0][1])
        self.assertTrue(all(previous is model for _,previous in inputs[1:]))
        for i,row in enumerate(result):
            self.assertLess(inputs[i][0][-1].to_pydatetime(),feed.parse_timestamp(row['relative_forecast_hour']))
            self.assertEqual(len(inputs[i][0]),672+i)

    def test_raw_source_requires_declared_clock_population_and_exact_hash(self):
        with tempfile.TemporaryDirectory() as d:
            p=Path(d)/'trace.json';p.write_text(json.dumps({'metadata':{'gap_seconds':195},'data':[1]*20158}))
            m={'trace_sha256':hashlib.sha256(p.read_bytes()).hexdigest(),
               'clock_kind':'explicit-relative-trace-clock','relative_origin':'1970-01-01T00:00:00Z',
               'risk_population':1,'population_assumption':'one queried availability slot; not measured running-instance exposure'}
            self.assertEqual(preview.load_source(p,m).gap_seconds,195)
            for patch in [{'trace_sha256':'0'*64},{'clock_kind':'actual-Seoul-time'}, {'risk_population':0},{'population_assumption':''}]:
                with self.assertRaises(ValueError):preview.load_source(p,dict(m,**patch))
