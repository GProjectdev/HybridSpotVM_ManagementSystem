"""Download a qualified public Seoul Linux/Shared On-Demand price receipt.

No AWS credentials or mutating cloud API. Refresh immediately before each run.
"""
import argparse,csv,io,json,math,os,re,time,urllib.request
from datetime import datetime,timezone
from pathlib import Path
URL='https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/ap-northeast-2/index.csv'


def select_row(rows,instance_type):
    expected={'TermType':'OnDemand','Unit':'Hrs','Currency':'USD','Instance Type':instance_type,
              'Tenancy':'Shared','Operating System':'Linux','Pre Installed S/W':'NA',
              'Region Code':'ap-northeast-2','CapacityStatus':'Used','operation':'RunInstances',
              'usageType':'APN2-BoxUsage:'+instance_type}
    for row in rows:
        if all(row.get(k)==v for k,v in expected.items()):
            price=float(row['PricePerUnit'])
            if not math.isfinite(price) or price<=0:raise ValueError('invalid public OD price')
            return row
    raise ValueError('no qualified public OD row; do not substitute capacity-reservation pricing')


def fetch(instance_type):
    if not re.fullmatch(r'[a-z][a-z0-9]*\.[a-z0-9]+',instance_type):raise ValueError('invalid instance type')
    deadline=time.monotonic()+180
    with urllib.request.urlopen(URL,timeout=30) as response:
        reader=csv.reader(io.TextIOWrapper(response,encoding='utf-8'));metadata=[]
        for line in reader:
            if line and line[0]=='SKU':header=line;break
            metadata.append(line)
            if len(metadata)>20:raise ValueError('unexpected price CSV preamble')
        else:raise ValueError('missing public CSV header')
        def rows():
            for line in reader:
                if time.monotonic()>deadline:raise TimeoutError('public receipt deadline')
                yield dict(zip(header,line))
        row=select_row(rows(),instance_type)
    return {'utc':datetime.now(timezone.utc).isoformat(),'source':URL,'source_metadata':metadata,'row':row}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--instance-type',required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args()
    if a.out.exists():raise ValueError('preserve previous receipt')
    receipt=fetch(a.instance_type);os.umask(0o077)
    with a.out.open('x') as f:json.dump(receipt,f,indent=2)
    print('Qualified public On-Demand price receipt saved.')

if __name__=='__main__':main()
