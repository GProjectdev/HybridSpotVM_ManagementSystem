# ResNet18 / CIFAR-10 evaluation worker

Build with this directory as the context:

```bash
docker build -t resnet18-eval:local evaluation/resnet18
```

The image uses PyTorch 2.4.1 / torchvision 0.19.1. FluidCR comes from the
existing webhook payload, not from PyPI. In the Pod, explicitly set
`command: [python, -u, /workspace/train_resnet18.py]` so the webhook can
wrap it with `/opt/fluidcr/bin/fluidcr-launcher`. Do not wrap it twice.
The runner mounts the script ConfigMap at `/workspace`, replacing the image's
copy with this same entrypoint. Its event keys match `evaluation/collect.py`.

| Environment | Meaning / default |
| --- | --- |
| `EVAL_RUN_ID` | Required, stable across all ranks and restores of one run |
| `EVAL_GOAL_STEPS` | Total completed optimizer updates, default 10000 |
| `EVAL_BATCH_SIZE` | Per-rank batch size, default 128 |
| `EVAL_SEED` | Integer in [0, 2^32), default 42 |
| `EVAL_DATA_ROOT` | Pre-staged CIFAR-10 directory, default `/datasets/cifar10` |
| `RANK`, `WORLD_SIZE` | Required explicit DDP topology; intended world size 2 |
| `LOCAL_RANK` | Local CUDA device, default 0 (one GPU per Pod) |
| `MASTER_ADDR`, `MASTER_PORT` | PyTorch environment rendezvous endpoint |
| `FLUIDCR_CHECKPOINT_PATH` | Injected per-Pod path, `/checkpoint/$(POD_NAME)/latest.pt` |

Use a fresh dynamic checkpoint PVC per run. Mount the same CIFAR-10 dataset on
every rank, already extracted as `cifar-10-batches-py` under the data root.
Training never downloads data. For preparation, use torchvision's
`CIFAR10(root, train=True, download=True)` once outside the measured run.
`--help` describes equivalent CLI arguments and SGD settings. `--device cpu`
selects Gloo for local smoke tests; CUDA/NCCL is the default.

## Events and completion

Each application event is a flushed stdout line beginning with `EVAL_EVENT `,
followed by JSON. Required keys: `timestamp` (UTC RFC3339 ending in `Z`),
`global_step`, `rank`, `run_id`, `event`. Event names are `worker_started`,
`restored`, `step`, and `completed`. Additional common keys: `attempt_id`,
`world_size`, `goal_steps`, `batch_size`. Step records include local loss,
`step_seconds` (wall time including data access and any survivor pause), and
`samples_seen` (logical global progress, not cumulative replay work).

`worker_started` is emitted before imports/restore and reports step 0; it is
not proof of fresh model state. `restored` reports the loaded positive step
after optimizer restoration and DDP construction. FluidCR may exit
inside `optimizer.step()` after saving, so a saved step need not have a step
event. Deduplicate logical progress by `(run_id, rank, global_step)` and use
`attempt_id` to distinguish replay and process restarts.

Both ranks rendezvous at the goal, emit `completed`, and remain alive without
additional optimizer steps. The collector must observe completion for both
ranks, then suspend the training policy. Completion ignores subsequent
SIGUSR1 and cancels a pending FluidCR checkpoint watchdog: idle workers cannot
participate in optimizer-boundary checkpoint rendezvous. The launcher API can
still report Running; the evaluation completion authority is the JSON event.
The trainer does not edit Kubernetes policy/status, write a completion
checkpoint, or terminate the launcher. Normal Pod termination stops the idle
process. The inspected launcher would mark exit 0 Completed, but a StatefulSet
would restart that container, hence the explicit keepalive here.

## Resume contract

Import FluidCR before constructing the model/optimizer. The neighboring
`My_FluidCR-work/fluidcr/backends/pytorch.py` patches SGD's base optimizer
constructor to load model state, SGD momentum, RNG, and `_fluidcr_step`.
Restore the optimizer before constructing DDP so a replacement rank 0 cannot
broadcast freshly initialized parameters into live survivors. Do not add
startup-only collectives after DDP: a partial-migration survivor resumes inside
its optimizer hook and never repeats application startup. Matching checkpoint
steps across ranks rely on FluidCR's coordinated checkpoint/restore contract.
Read progress through the real public `fluidcr.global_step(optimizer)` API;
`fluidcr.set_steps_per_epoch()` supplies checkpoint epoch metadata.

There is one optimizer update per batch, no accumulation, AMP, or separate
stateful LR scheduler. ResNet18 uses a CIFAR stem (3x3 stride-1 convolution,
no max pool), 10 classes, SGD, and ordinary DDP BatchNorm. Model state stores
a configuration fingerprint; changed run/seed/world/batch/goal/hyperparameters
fail on restore. World-size changes during an evaluation are unsupported.

An epoch permutation uses a private seeded RNG. Each rank takes disjoint full
batches; the incomplete global tail is dropped. Crop and flip are keyed by
seed, epoch, and image index. A resumed sampler starts directly at the loaded
step, and the loader uses implicit iteration, bypassing FluidCR's transparent
`iter`/`enumerate` skip hooks. Loader RNG is separate from restored model RNG.
The image disables all three transparent iteration/range patches before
sitecustomize runs. Deterministic PyTorch algorithms are required; equivalence
across GPU architectures or library versions is not promised.

## Tests

```bash
python -B -m unittest discover -s evaluation/resnet18 -p 'test_*.py' -v
```

Unit tests need only Python. They exercise exact resumed batch suffixes across
epoch boundaries, disjoint ranks, RNG isolation, run binding, goal enforcement,
hook failure detection, JSON events, and completion keepalive. These do not
substitute for CUDA/NCCL checkpoint/restore and live two-Pod collector testing.
