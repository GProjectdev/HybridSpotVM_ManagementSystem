"""Bounded, two-rank CPU smoke: real ResNet-50/FluidCR save and fresh-process restore.

Uses staged, official CIFAR-10; does not certify CUDA/NCCL, CRIU, NFS recovery,
controller orchestration, or performance. No API calls or cloud resources.
"""
import argparse
from datetime import timedelta
import json
import os
from pathlib import Path
import tempfile


def worker(rank, root, data_root, resumed, port):
    os.environ.update(MASTER_ADDR='127.0.0.1',MASTER_PORT=str(port),RANK=str(rank),WORLD_SIZE='2',
        FLUIDCR_CHECKPOINT_PATH=str(Path(root)/str(rank)/'latest.pt'),
        FLUIDCR_CHECKPOINT_DIR=root,FLUIDCR_DISTRIBUTED='1',
        FLUIDCR_DISABLE_ENUMERATE_PATCH='1',FLUIDCR_DISABLE_ITER_PATCH='1',
        FLUIDCR_DISABLE_EPOCH_RANGE_PATCH='1')
    if resumed:
        os.environ['FLUIDCR_RESTORE_CHECKPOINT_PATH']=os.environ['FLUIDCR_CHECKPOINT_PATH']
    else:
        os.environ.pop('FLUIDCR_RESTORE_CHECKPOINT_PATH',None)
    import fluidcr
    import torch
    import torch.distributed as dist
    from torchvision.datasets import CIFAR10
    from torchvision.models import resnet50
    from torch.nn.parallel import DistributedDataParallel
    from train_resnet18 import TrainingData, StepBatchSampler
    from restore_audit import fingerprint
    from fluidcr.backends import pytorch as backend
    torch.set_num_threads(1)
    torch.manual_seed(42)
    torch.use_deterministic_algorithms(True)
    dist.init_process_group('gloo',rank=rank,world_size=2,timeout=timedelta(seconds=60))
    model=resnet50(weights=None,num_classes=10)
    model.conv1=torch.nn.Conv2d(3,64,3,stride=1,padding=1,bias=False)
    model.maxpool=torch.nn.Identity()
    optimizer=torch.optim.SGD(model.parameters(),lr=.1,momentum=.9,weight_decay=.0005)
    step=fluidcr.global_step(optimizer)
    assert step==(3 if resumed else 0), step
    if resumed:
        saved=torch.load(os.environ['FLUIDCR_CHECKPOINT_PATH'],weights_only=False)
        assert fingerprint(model.state_dict())==fingerprint(saved['models'][0]['state_dict'])
        assert fingerprint(optimizer.state_dict())==fingerprint(saved['optimizers'][0]['state_dict'])
        assert fingerprint(backend._collect_rng_states())==fingerprint(saved['rng_states'])
    ddp=DistributedDataParallel(model)
    data=TrainingData(CIFAR10(data_root,train=True,download=False),42)
    for keys in StepBatchSampler(len(data.dataset),2,2,rank,42,step,4):
        items=[data[k] for k in keys]
        images=torch.stack([x for x,y in items]);labels=torch.tensor([y for x,y in items])
        optimizer.zero_grad();loss=torch.nn.functional.cross_entropy(ddp(images),labels)
        assert torch.isfinite(loss)
        loss.backward();optimizer.step()
        step=fluidcr.global_step(optimizer)
        if step==3 and not resumed:
            backend._save_checkpoint(os.environ['FLUIDCR_CHECKPOINT_PATH'])
            dist.barrier()
    assert step==4
    result={'rank':rank,'step':step,'model_sha256':fingerprint(model.state_dict()),
            'parameters_sha256':fingerprint(dict(model.named_parameters())),
            'buffers_sha256':fingerprint(dict(model.named_buffers())),
            'optimizer_sha256':fingerprint(optimizer.state_dict()),
            'rng_sha256':fingerprint(backend._collect_rng_states())}
    Path(root,f'{"restored" if resumed else "reference"}-{rank}.json').write_text(json.dumps(result))
    dist.barrier();dist.destroy_process_group()


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--data-root',required=True);p.add_argument('--out',type=Path,required=True);a=p.parse_args()
    if a.out.exists():raise ValueError('use fresh output')
    import socket
    import torch.multiprocessing as mp
    def port():
        with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
    with tempfile.TemporaryDirectory(prefix='resnet50-cpu-') as root:
        for restored in (False,True):mp.spawn(worker,args=(root,a.data_root,restored,port()),nprocs=2,join=True)
        rows=[]
        for rank in (0,1):
            ref=json.loads(Path(root,f'reference-{rank}.json').read_text());got=json.loads(Path(root,f'restored-{rank}.json').read_text())
            assert ref==got,(ref,got)
            rows.append(got)
        assert rows[0]['parameters_sha256']==rows[1]['parameters_sha256']
        assert rows[0]['optimizer_sha256']==rows[1]['optimizer_sha256']
        a.out.write_text(json.dumps({'passed':True,'scope':'CPU Gloo real CIFAR-10/ResNet-50; fresh-process FluidCR restore and next update exact per rank; not GPU/controller acceptance','rows':rows},indent=2)+'\n')

if __name__=='__main__':main()
