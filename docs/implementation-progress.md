# Architecture Realignment Implementation Progress

Updated: 2026-09-29. Local implementation and test evidence only. No image push,
cluster deployment, instance deletion, or automated GPU E2E is claimed.

## Implemented and Verified Locally

| Owner | Change | Evidence |
|---|---|---|
| Hybrid Spot Policy Manager | User-owned policy enrollment and Runtime ownership; placement in status rather than user spec | management ownership/discovery/placement tests |
| Hybrid Spot Coordinator | Sole checkpoint creator; planned capacity readiness and periodic serialization; duplicate active operations rejected | replacement_controller_test.go, checkpoint_ownership_test.go |
| Coordinator / Replacement | Partial checkpoint bound to operation UID, workload identity, target ranks and resume=false | checkpoint_ownership_test.go |
| Member Checkpoint/Restore | Archival Completed and completionTime retained during restore-owned release; separate SurvivorReleased condition | fluidcrmigration_controller_test.go |
| FluidCR / Runtime Controller | Whole-update timing windows, worker sessions, aligned all-rank maximum of means, schema propagation | test_iteration_window.py, test_runtime_contract.py, iteration_test.go, crd_test.go |
| MultiCluster Checkpoint/Restore | Consume actual nested RIC observedGeneration, reject stale/conflicting/duplicate reports | runtime_aggregation_test.go |
| FluidCR | Mandatory model/optimizer/step/RNG coverage and loading errors stop resume; no misleading success log | test_checkpoint_load.py |
| Hybrid Spot Policy / Coordinator | Suspension stops new intent; existing operation execution remains possible | suspend_test.go |
| Cloud Node Provisioner | Existing fencing/deletion safeguards retained; new-node driver linker preparation and clean-environment CUDA library/helper probe | TestRestore*, TestFence*, TestVPCDelete* |

### Timing Contract

- One supported iteration is one completed optimizer update for a single-optimizer
  DDP worker. Elapsed time starts after the preceding FluidCR rendezvous and ends
  after the next optimizer update and device synchronization. Data preparation,
  forward/backward and gradient accumulation between boundaries are included.
- Initial incomplete timing and five warmup updates are excluded. Only a complete
  aligned 20-update window is published. Pause/rebuild starts a new timing session.
- Checkpoint rendezvous and pause time are excluded from this training metric.
  It is not total wall-clock time per update, and synchronization overhead must be measured.
- Runtime promotes a metric only when all ranks report the same step window,
  positive finite measurements and fresh matching worker sessions. Missing timing
  does not make otherwise live training unhealthy, but blocks the paper calculator.
- The aggregate is max(rank mean duration), not the exact mean of per-step rank
  maxima. Multiple optimizers and multi-GPU-per-rank timing are not certified.
- Legacy scalar optimizer-call timing is not silently accepted as this metric.

### Paper Model Boundary

The local reference is Cost-Efficient Training and Checkpointing for Large Models
on Preemptible Cloud VMs (EuroMLSys 2026), section 4.
The new policy/paper.go implements equations 1-5:

    tau = observed parallel update seconds = T / Ndp
    lambda_iteration = lambda_hour * (tau * Ndp) / 3600
    cost(f) = tau + D/f + max(0, St/f - tau) + lambda_iteration*f*tau/2
    f >= ceil(St*S / (C*tau))

Reconstructing T from measured tau and Ndp is an explicit linear-scaling model
assumption, not a new measurement of single-shard execution.
The search is over bounded user candidates, not every positive integer.

Paper calculation uses spec.checkpoint.paperProfile and fresh calibrated asynchronous
GPU-to-DRAM/storage timings, checkpoint size and buffer capacity. Synchronous CRIU
duration and artifact export time are NOT interchangeable with D and St.
Missing/stale/infeasible inputs use the existing bounded risk-band/bootstrap
interval and continue periodic checkpoint creation once runtime safety gates pass.
Status explicitly identifies bootstrap selection, without claiming paper evaluation.
Valid measurements automatically select the paper path on subsequent reconciliation.
Missing runtime cost measurements must not gate initial workload/VM provisioning.
Risk freshness, workload identity and restore/deletion safety gates remain separate.
The profile is operator-supplied calibration, not yet an
automatic member measurement or an identity-bound capability attestation.

The selected iteration interval is currently adapted to ceil(f*tau) wall-clock
seconds. This is not an exact iteration-count checkpoint trigger. Existing
allocation/economics code is retained; full Eq.6-7 policy equivalence is not claimed.
The opt-in calculator is not enabled in the sample deployment.

The existing SARIMA feed pipeline is retained. Availability drops, hourly sums,
three-hour causal smoothing, SARIMA(1,1,1)(1,0,1)[24], training window and retraining
must be evaluated against the paper with real held-out data. Population-based
conversion to per-instance risk is an approximation and must retain provenance.

### Suspension Is Not Cancellation

User-controlled metadata:

    metadata:
      annotations:
        training.dcnlab.com/suspend: "true"

An annotation avoids invalidating generation-bound operations. Policy and
Coordinator watch annotation changes. It blocks new capacity decisions,
replacement intent, periodic/emergency checkpoint intent and new group-placement
requests. It does not delete resources, stop training or cancel existing attempts.
An existing attempt may still create required capacity/checkpoints and complete
restore/cleanup. Runtime and risk observation continue. This is not a transactional
global freeze: a reconcile already in flight may have passed its read gate.
Do not describe this as cleanup complete or provider termination confirmation.

## Upgrade Constraints

1. Update TrainingRuntime and TrainingPolicy CRDs before replacing producers.
2. Roll out Coordinator and Replacement together: old Replacement still writes
   checkpoints, so mixed old/new deployment does not establish single ownership.
3. New partial checkpoints require recovery-operation-uid. An old checkpoint
   lacking it is rejected rather than adopted by name. Drain old attempts before
   upgrade; do not fabricate status or retrofit identity without verified evidence.
4. Update FluidCR injection/runtime distribution and recreate test workers through
   the supported recovery procedure to load new Python code. Controller image
   replacement alone does not update code already loaded inside Pods.
5. New mandatory loading checks reject incomplete legacy payloads. First optimizer
   creation must see the complete supported tracked model/optimizer set. Generic
   multi-optimizer lazy initialization is not a supported automatic restore contract.
6. Legacy checkpoints already regressed to Pending are not automatically repaired.
   Completed archival evidence must not be synthesized from a Running Pod.

## Remaining Work: Not Implemented by This Change

| Sequence | Owner | Deliverable / acceptance gate |
|---|---|---|
| 1 | Member C/R + FluidCR | Typed checkpoint-load receipt tied to attempt, artifact, worker session and world identity; all-rank post-resume progress; survivor restart rejects partial continuity and triggers explicit abort/group fallback |
| 2 | Collector + Policy/Coordinator | Real availability ingestion/model training/retraining and holdout evaluation; complete Eq.6-7 allocation objective and iteration-count triggering; measured async capability/profile or explicit sync-mode distinction |
| 3 | Hybrid Spot placement + member capacity reporting | Fresh heterogeneous GPU/CPU/memory inventory, atomic reservation across concurrent jobs, whole-world onprem admission or one-public-cluster burst; initial placement must not require a prior checkpoint |
| 4 | MultiCluster + PV + Hybrid lifecycle | Explicit cancellation/termination operation, stop-authority handshake, source/target shutdown evidence and resource cleanup; retain referenced checkpoints/PVCs and bound terminal-CR retention |
| 5 | Provisioner + integration | New-node certification path is implemented, including driver linker preparation and clean-environment probe; real provider termination and retry behavior still require controlled cloud E2E |

Do not solve initial bursting by treating every Pending Pod as GPU shortage.
Do not solve cancellation by disabling restoration safety gates or removing
finalizers. These remaining paths need their own regression and integration tests.

## Verification

- System: full Go tests, go vet and cmd builds pass; targeted schema and policy tests included.
- Stateful-Migration-System: full Go tests pass, including checkpoint/release,
  management verification, artifact, interpreter, member and runtime packages.
  go vet and cmd builds also pass.
- Provider: restore linker/probe and fence/delete tests pass; new-node certification now checks CUDA libraries and helper startup.
- Python: 54 Runtime tests, 23 group-restore tests, 3 timing tests and 2 mandatory
  loading tests (11 failure subcases) pass. Backend loading tests use mocks.
- Full Python discovery cannot pass here because pytest and torch are absent.
  No real CUDA/NCCL/CRIU execution was performed.
- All 14 FluidCR Python source files parse. compileall encountered an existing
  __pycache__ write-permission error; read-only AST validation was used instead.
- Unit/fake-client success is not live Karmada scheduling/propagation or cloud E2E.

## Stage Selection and Recovery Calibration

Missing post-start measurements do not block initial provisioning. Policy status
reports decisionStage, intervalSource, priceEvaluated and economicsSource.
Bootstrap uses fresh risk, available prices, user constraints and a bounded
initial interval. CheckpointMeasured uses the existing measured checkpoint-cost
calculator. PaperMeasured requires qualified asynchronous calibration; it is not
equivalent to substituting synchronous CRIU/export durations into paper equations.
EconomicsEvaluated requires opt-in economics and fresh calibration.

When economics is enabled and lossCostPerEviction is omitted, an identity-checked
current-generation Verified RestoreRequest can supply request-to-verification
latency. The cost proxy combines half the evaluated checkpoint interval at the
Spot price with that latency at the OnDemand price. It is not measured lost work,
pure CRIU time, a new restore success certificate, or the complete paper objective.
Explicit operator calibration takes precedence. Calibration is in-memory only;
user spec is not rewritten.

See [release validation](release-validation-20260929.md) and
[architecture/workflows/PPT](architecture-workflows-ppt.md) for this release.
