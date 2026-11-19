"""Verify immutable local experiment artifacts; performs no cloud operations."""
import argparse
import hashlib
import json
from pathlib import Path


def verify(manifest, run_file=None):
    manifest=Path(manifest).resolve();root=manifest.parent
    m=json.loads(manifest.read_text())
    if m.get('status')!='FROZEN' or not m.get('researcher_final_freeze_confirmed'):
        raise ValueError('researcher-approved FROZEN scientific contract required')
    for item in m['artifacts']:
        p=root/item['path']
        if p.is_symlink() or not p.resolve().is_relative_to(root):
            raise ValueError('artifact outside frozen root')
        if hashlib.sha256(p.read_bytes()).hexdigest()!=item['sha256']:
            raise ValueError('frozen artifact changed: '+item['path'])
    sha=hashlib.sha256(manifest.read_bytes()).hexdigest()
    if run_file:
        r=json.loads(Path(run_file).read_text())
        expected=m['rendered_contracts'][r['model']][r['arm']]
        if r.get('frozen_manifest_sha256')!=sha or r.get('protocol_lock_status')!='FROZEN':
            raise ValueError('Run not bound to this frozen manifest')
        for k in ('workload_lock_sha256','comparison_lock_sha256','training_code_sha256'):
            if r.get(k)!=expected[k]:raise ValueError('Run comparison/workload/code contract changed')
        cfg=json.loads((root/m['configs'][r['model']]['path']).read_text())
        keys=('model','goal_steps','world_size','batch_size','sequence_length','seed','learning_rate',
              'instance_type','region','availability_zone','training_image','asset_manifest_sha256',
              'existing_policy','existing_checkpoint','existing_replacement','existing_risk_spec',
              'checkpoint_buffer_reuse','group_replacement_batch_size','system_lock','risk_lock_sha256','fault_window')
        if r['model']=='resnet50':keys+=('momentum','weight_decay')
        if any(r.get(k)!=cfg.get(k) for k in keys):raise ValueError('Run scientific setting changed')
    return {'scientific_status':'FROZEN','manifest_sha256':sha,'verified_artifacts':len(m['artifacts']),
            'paid_execution_authorized_by_this_check':False,'cloud_mutations':False}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--manifest',type=Path,required=True);p.add_argument('--run',type=Path)
    a=p.parse_args();print(json.dumps(verify(a.manifest,a.run)))


if __name__=='__main__':main()
