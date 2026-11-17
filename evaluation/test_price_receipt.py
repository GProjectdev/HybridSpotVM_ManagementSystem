import unittest
from price_receipt import select_row

class PriceReceiptTests(unittest.TestCase):
    def test_filters_unused_reservations_and_wrong_hardware(self):
        row={'TermType':'OnDemand','Unit':'Hrs','Currency':'USD','Instance Type':'g5.xlarge','Tenancy':'Shared','Operating System':'Linux','Pre Installed S/W':'NA','Region Code':'ap-northeast-2','CapacityStatus':'Used','operation':'RunInstances','usageType':'APN2-BoxUsage:g5.xlarge','PricePerUnit':'1.237'}
        unused=dict(row,CapacityStatus='UnusedCapacityReservation',PricePerUnit='99')
        self.assertEqual(select_row([unused,row],'g5.xlarge'),row)
        with self.assertRaises(ValueError):select_row([unused],'g5.xlarge')
        with self.assertRaises(ValueError):select_row([row],'g6.xlarge')
        with self.assertRaises(ValueError):select_row([dict(row,PricePerUnit='0')],'g5.xlarge')
