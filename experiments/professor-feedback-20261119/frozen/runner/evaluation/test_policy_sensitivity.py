import copy
import math
import unittest
from policy_sensitivity import requests

class PolicyInputProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.forecast={'forecasts':[{'evaluation_hour':i,'lambda_per_hour':.1,'future_observations_used':False} for i in range(168)]}
        self.prices={'at':'2026-11-19T00:00:00Z',**{t+'_price':{'spotPricePerHour':.5,'onDemandPricePerHour':1} for t in ('g5.2xlarge','g6.2xlarge')}}
        self.policy={'method':'paper-eq6-7','fixedOnDemand':1,'economics':{'enabled':True}}
    def test_controls_remain_unmodified_and_blocks_stay_separate(self):
        before=copy.deepcopy(self.policy)
        items=requests(self.forecast,self.prices,self.policy)
        self.assertEqual(len(items),2016)
        self.assertEqual(len({req['id'] for req,_ in items}),2016)
        self.assertEqual(self.policy,before)
        self.assertEqual({m['world_size'] for _,m in items},{2,4})
        self.assertEqual({m['alpha'] for _,m in items},{.7,.8,.9})
        for req,m in items:
            p=req['policy']['spec']['policy']
            self.assertNotIn('fixedOnDemand',p)
            self.assertEqual(p['forecastHorizonSeconds'],3600)
            self.assertEqual(p['economics']['lossCostPerEviction'],-1)
    def test_future_data_gaps_and_nonfinite_inputs_are_rejected(self):
        for field,value in [('future_observations_used',True),('evaluation_hour',5),('lambda_per_hour',float('nan')),('lambda_per_hour',-1)]:
            forecast=copy.deepcopy(self.forecast);forecast['forecasts'][0][field]=value
            with self.assertRaises(ValueError):requests(forecast,self.prices,self.policy)
        for value in (0,float('inf'),float('nan')):
            prices=copy.deepcopy(self.prices);prices['g5.2xlarge_price']['spotPricePerHour']=value
            with self.assertRaises(ValueError):requests(self.forecast,prices,self.policy)
