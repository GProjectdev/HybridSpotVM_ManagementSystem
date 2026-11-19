"""Refresh risk-feed prices using read-only AWS and a reviewed public OD receipt."""
import argparse
from datetime import datetime,timedelta,timezone
import hashlib,json,math,os
from pathlib import Path
try:
    from .ieee_v2_ledger import AWSReader
    from .collect import timestamp
except ImportError:
    from ieee_v2_ledger import AWSReader
    from collect import timestamp


def refresh(reader, ondemand_receipt, zone, now, instance_type="g5.xlarge"):
    reader.observe({},now.isoformat()) # validates account; no instance lookup
    receipt=json.loads(Path(ondemand_receipt).read_text());row=receipt['row']
    for k,v in {'TermType':'OnDemand','Unit':'Hrs','Currency':'USD','Instance Type':instance_type,'Tenancy':'Shared','Operating System':'Linux','Pre Installed S/W':'NA','Region Code':'ap-northeast-2','CapacityStatus':'Used','operation':'RunInstances','usageType':'APN2-BoxUsage:'+instance_type}.items():
        if row.get(k)!=v:raise ValueError('OD receipt product mismatch')
    if not 0 <= (now-timestamp(receipt['utc'])).total_seconds() <= 86400:
        raise ValueError('refresh/review public OD receipt within 24 hours')
    data=reader.command('ec2','describe-spot-price-history','--instance-types',instance_type,'--availability-zone',zone,'--product-descriptions','Linux/UNIX','--start-time',now.isoformat(),'--max-results','1')['SpotPriceHistory']
    if not data:raise ValueError('no current Spot price observation')
    spot=data[0]
    if spot['AvailabilityZone']!=zone or spot['InstanceType']!=instance_type or spot['ProductDescription']!='Linux/UNIX' or timestamp(spot['Timestamp'])>now:
        raise ValueError('Spot price product/time mismatch')
    od,sp=float(row['PricePerUnit']),float(spot['SpotPrice'])
    if not all(math.isfinite(v) and v>0 for v in (od,sp)):raise ValueError('invalid price')
    return {'observedAt':now.isoformat(),'currency':'USD','spotPricePerHour':sp,'onDemandPricePerHour':od,
            'spotPriceEffectiveAt':spot['Timestamp'],'availabilityZone':zone,'instanceType':instance_type,
            'ondemandReceiptSHA256':hashlib.sha256(Path(ondemand_receipt).read_bytes()).hexdigest()}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--profile',required=True);p.add_argument('--account',required=True);p.add_argument('--ondemand-receipt',type=Path,required=True);p.add_argument('--out',type=Path,required=True);p.add_argument('--instance-type',default='g5.xlarge');a=p.parse_args()
    reader=AWSReader(a.profile,a.account,'ap-northeast-2');data=refresh(reader,a.ondemand_receipt,'ap-northeast-2c',datetime.now(timezone.utc),a.instance_type)
    os.umask(0o077);tmp=a.out.with_suffix('.tmp');tmp.write_text(json.dumps(data,indent=2)+'\n');tmp.replace(a.out)


if __name__=='__main__':main()
