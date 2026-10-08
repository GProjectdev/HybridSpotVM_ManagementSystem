"""Step-bounded CIFAR-10 DDP workload using the injected FluidCR backend."""

import argparse
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta, timezone
import hashlib
import json
import math
import os
import random
import re
import signal
import sys
import time
import uuid


@dataclass(frozen=True)
class Config:
    run_id: str
    goal_steps: int
    batch_size: int
    seed: int
    data_root: str
    rank: int
    world_size: int
    local_rank: int
    device: str
    learning_rate: float
    momentum: float
    weight_decay: float


def parse_config(argv=None, environ=None):
    env = os.environ if environ is None else environ
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run-id", default=env.get("EVAL_RUN_ID", ""))
    parser.add_argument("--goal-steps", type=int, default=env.get("EVAL_GOAL_STEPS", "10000"))
    parser.add_argument("--batch-size", type=int, default=env.get("EVAL_BATCH_SIZE", "128"))
    parser.add_argument("--seed", type=int, default=env.get("EVAL_SEED", "42"))
    parser.add_argument("--data-root", default=env.get("EVAL_DATA_ROOT", "/datasets/cifar10"))
    parser.add_argument("--device", choices=("cuda", "cpu"), default="cuda")
    parser.add_argument("--learning-rate", type=float, default=0.1)
    parser.add_argument("--momentum", type=float, default=0.9)
    parser.add_argument("--weight-decay", type=float, default=5e-4)
    args = parser.parse_args(argv)
    try:
        rank, world = int(env["RANK"]), int(env["WORLD_SIZE"])
        local = int(env.get("LOCAL_RANK", "0"))
    except (KeyError, ValueError):
        parser.error("RANK and WORLD_SIZE must be explicit integers; LOCAL_RANK defaults to 0")
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}", args.run_id):
        parser.error("EVAL_RUN_ID/--run-id must match the collector's 1-128 character run identifier")
    if args.goal_steps < 1 or args.batch_size < 1:
        parser.error("goal steps and per-rank batch size must be positive")
    if not 0 <= args.seed < 2**32:
        parser.error("seed must be in [0, 2**32)")
    if world < 1 or not 0 <= rank < world or local < 0:
        parser.error("invalid distributed rank/world size")
    if (not math.isfinite(args.learning_rate) or args.learning_rate <= 0
            or not math.isfinite(args.momentum) or not 0 <= args.momentum < 1
            or not math.isfinite(args.weight_decay) or args.weight_decay < 0):
        parser.error("invalid SGD hyperparameters")
    return Config(**vars(args), rank=rank, world_size=world, local_rank=local)


class Events:
    def __init__(self, config, stream=None):
        self.config = config
        self.stream = sys.stdout if stream is None else stream
        self.attempt_id = uuid.uuid4().hex

    def emit(self, event, global_step, **fields):
        if event not in {"worker_started", "restored", "step", "completed"}:
            raise ValueError("unknown evaluation event")
        if type(global_step) is not int or global_step < 0:
            raise ValueError("global_step must be a nonnegative integer")
        record = dict(fields)
        record.update(
            timestamp=datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"),
            global_step=global_step, rank=self.config.rank,
            run_id=self.config.run_id, event=event,
            attempt_id=self.attempt_id, world_size=self.config.world_size,
            goal_steps=self.config.goal_steps, batch_size=self.config.batch_size,
        )
        print("EVAL_EVENT " + json.dumps(record, sort_keys=True, allow_nan=False),
              file=self.stream, flush=True)


class StepBatchSampler:
    """Reconstruct a rank's batches directly from completed optimizer steps."""

    def __init__(self, size, batch_size, world_size, rank, seed, start_step, goal_steps):
        if size < 1 or batch_size < 1 or world_size < 1 or not 0 <= rank < world_size:
            raise ValueError("invalid sampler dimensions")
        if not 0 <= start_step <= goal_steps:
            raise ValueError("invalid sampler step range")
        self.steps_per_epoch = size // (batch_size * world_size)
        if not self.steps_per_epoch:
            raise ValueError("dataset must contain at least one full global batch")
        self.size, self.batch_size, self.world_size = size, batch_size, world_size
        self.rank, self.seed = rank, seed
        self.start_step, self.goal_steps = start_step, goal_steps

    def __len__(self):
        return self.goal_steps - self.start_step

    def __iter__(self):
        cached_epoch, indices = None, None
        for step in range(self.start_step, self.goal_steps):
            epoch, batch = divmod(step, self.steps_per_epoch)
            if epoch != cached_epoch:
                indices = list(range(0, self.size))
                random.Random(f"order:{self.seed}:{epoch}").shuffle(indices)
                cached_epoch = epoch
            offset = (batch * self.world_size + self.rank) * self.batch_size
            yield [(epoch, index) for index in indices[offset:offset + self.batch_size]]


def augmentation_parameters(seed, epoch, index):
    rng = random.Random(f"augment:{seed}:{epoch}:{index}")
    return rng.randrange(9), rng.randrange(9), rng.random() < 0.5


class TrainingData:
    def __init__(self, dataset, seed):
        self.dataset, self.seed = dataset, seed

    def __len__(self):
        return len(self.dataset)

    def __getitem__(self, key):
        from torchvision.transforms import functional as functional

        epoch, index = key
        image, label = self.dataset[index]
        top, left, flip = augmentation_parameters(self.seed, epoch, index)
        image = functional.crop(functional.pad(image, 4), top, left, 32, 32)
        if flip:
            image = functional.hflip(image)
        image = functional.normalize(functional.to_tensor(image),
                                     (0.4914, 0.4822, 0.4465), (0.2470, 0.2435, 0.2616))
        return image, label


def contract_digest(config, dataset_size):
    contract = asdict(config)
    for key in ("rank", "local_rank", "data_root"):
        contract.pop(key)
    contract.update(dataset="cifar10-train", dataset_size=dataset_size,
                    model="resnet18-cifar-stem", data_recipe="step-shuffle-crop-flip-v1")
    return hashlib.sha256(json.dumps(contract, sort_keys=True).encode("utf-8")).digest()


def resumed_step(fluidcr, optimizer, goal_steps):
    # The actual backend installs this attribute at optimizer construction.
    if not hasattr(optimizer, "_fluidcr_step"):
        raise RuntimeError("FluidCR optimizer hook is not active")
    step = fluidcr.global_step(optimizer)
    if type(step) is not int or not 0 <= step <= goal_steps:
        raise RuntimeError("restored optimizer step is outside this run's goal")
    return step


def train_updates(config, fluidcr, torch, model, optimizer, loader, device, events):
    step = resumed_step(fluidcr, optimizer, config.goal_steps)
    if step == config.goal_steps:
        return step
    model.train()
    started = time.monotonic()
    # Implicit iteration bypasses FluidCR's builtins.iter/enumerate skip patches.
    # The batch sampler already starts at the restored step; never skip twice.
    for images, labels in loader:
        images, labels = images.to(device), labels.to(device)
        optimizer.zero_grad(set_to_none=True)
        loss = torch.nn.functional.cross_entropy(model(images), labels)
        loss_value = float(loss.detach().item())
        if not math.isfinite(loss_value):
            raise RuntimeError("non-finite training loss")
        loss.backward()
        optimizer.step()
        completed = resumed_step(fluidcr, optimizer, config.goal_steps)
        if completed != step + 1:
            raise RuntimeError("FluidCR must advance exactly one step per optimizer update")
        step = completed
        events.emit("step", step, loss=loss_value,
                    step_seconds=time.monotonic() - started,
                    samples_seen=step * config.batch_size * config.world_size)
        if step == config.goal_steps:
            return step
        started = time.monotonic()
    raise RuntimeError("data stream ended before the goal")


def hold_completed(fluidcr, events, step):
    # Completion is a collector contract, not launcher exit(0): StatefulSets
    # restart exited containers. There are no optimizer rendezvous while idle.
    if hasattr(signal, "SIGUSR1"):
        signal.signal(signal.SIGUSR1, signal.SIG_IGN)
    fluidcr.cancel_checkpoint_watchdog()
    events.emit("completed", step)
    while True:
        time.sleep(30)


def main(argv=None):
    config = parse_config(argv)
    events = Events(config)
    events.emit("worker_started", 0, pid=os.getpid(), seed=config.seed)
    os.environ.setdefault("CUBLAS_WORKSPACE_CONFIG", ":4096:8")
    # Also set these in the image, before the launcher's sitecustomize executes.
    for name in ("ENUMERATE", "ITER", "EPOCH_RANGE"):
        os.environ[f"FLUIDCR_DISABLE_{name}_PATCH"] = "1"
    os.environ["FLUIDCR_DISTRIBUTED"] = "1"

    import fluidcr
    import numpy as np
    import torch
    import torch.distributed as dist
    from torch.nn.parallel import DistributedDataParallel
    from torch.utils.data import DataLoader
    from torchvision.datasets import CIFAR10
    from torchvision.models import resnet18

    random.seed(config.seed)
    np.random.seed(config.seed)
    torch.manual_seed(config.seed)
    torch.use_deterministic_algorithms(True)
    torch.backends.cudnn.benchmark = False
    torch.backends.cudnn.deterministic = True
    torch.backends.cuda.matmul.allow_tf32 = False
    torch.backends.cudnn.allow_tf32 = False
    if config.device == "cuda":
        if not torch.cuda.is_available():
            raise RuntimeError("CUDA is required unless --device cpu is explicit")
        torch.cuda.set_device(config.local_rank)
        device = torch.device("cuda", config.local_rank)
    else:
        device = torch.device("cpu")
    dist.init_process_group(backend="nccl" if config.device == "cuda" else "gloo",
                            rank=config.rank, world_size=config.world_size,
                            timeout=timedelta(minutes=10))
    # Data must be staged before the timed run, avoiding network/download races.
    dataset = TrainingData(CIFAR10(config.data_root, train=True, download=False), config.seed)
    expected_contract = torch.tensor(list(contract_digest(config, len(dataset))), dtype=torch.uint8)
    model = resnet18(weights=None, num_classes=10)
    model.conv1 = torch.nn.Conv2d(3, 64, kernel_size=3, stride=1, padding=1, bias=False)
    model.maxpool = torch.nn.Identity()
    model.register_buffer("_evaluation_contract", expected_contract.clone())
    model = model.to(device)
    # FluidCR loads model, optimizer momentum, RNG, and step count here.
    optimizer = torch.optim.SGD(model.parameters(), lr=config.learning_rate,
                               momentum=config.momentum, weight_decay=config.weight_decay)
    step = resumed_step(fluidcr, optimizer, config.goal_steps)
    if not torch.equal(model._evaluation_contract.cpu(), expected_contract):
        raise RuntimeError("checkpoint run/configuration does not match EVAL_* settings")
    # Restore before DDP's rank-0 broadcast, including when rank 0 is the
    # replacement. Survivors rebuild DDP inside the FluidCR optimizer hook;
    # they do not repeat main(), so no startup-only collectives may follow.
    model = DistributedDataParallel(model,
                                    device_ids=[config.local_rank] if config.device == "cuda" else None)
    if step > 0:
        events.emit("restored", step)
    sampler = StepBatchSampler(len(dataset), config.batch_size, config.world_size,
                               config.rank, config.seed, step, config.goal_steps)
    fluidcr.set_steps_per_epoch(sampler.steps_per_epoch)
    # A private loader generator prevents iterator construction consuming the
    # model's restored torch RNG stream. Augmentation has its own per-item RNG.
    generator = torch.Generator().manual_seed(config.seed + config.rank)
    loader = DataLoader(dataset, batch_sampler=sampler, num_workers=0, generator=generator)
    step = train_updates(config, fluidcr, torch, model, optimizer, loader, device, events)
    dist.barrier()
    hold_completed(fluidcr, events, step)


if __name__ == "__main__":
    main()
