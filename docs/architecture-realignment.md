# Architecture Realignment: Ownership and Delivery

Updated: 2026-09-29. This document separates the target architecture from changes
implemented in the ownership, checkpoint, telemetry and safety slices. It is not
an end-to-end completion claim. See implementation-progress.md for remaining work.

## System Boundaries

| System and location | Keep | Move or repair responsibility | Additional implementation |
|---|---|---|---|
| Hybrid Spot management, MGMT using Karmada API | Collector feed/status validation; UID-bound capacity/replacement/recovery; measured checkpoint costs | User owns TrainingPolicy and SpotRiskProfile inputs; Policy Manager owns Runtime discovery and replacement intent; Coordinator must become the sole checkpoint request writer | Paper-to-code risk/objective crosswalk; full-world initial placement and capacity reservation; lifecycle/cancellation and retention |
| Hybrid Spot member components | TrainingRuntime Controller and Spot Watcher; workload/pod identity and freshness checks | Runtime observes member C/R evidence, not CRIU internals; Watcher reports actual interruption events, not predicted risk | Whole-iteration timing windows and rank aggregation; versioned runtime evidence |
| MultiCluster Checkpoint/Restore, management | RestoreRequest/RestorePlan orchestration; artifact/volume/identity checks; source fencing | Own cross-cluster execution coordination and restore verdict; expose typed results to Hybrid Spot instead of leaking runtime-specific state | Stable attempt identity, group fallback, cancellation and evidence aggregation |
| Checkpoint/Restore, member and workload | FluidCR launcher/checkpoint backend; checkpoint controller; artifact export/stage/hash verification; restore-owned release | Keep archive completion separate from live survivor/release conditions; member owns exec/CRIU/lock operations | Explicit checkpoint-load success and world progress report; survivor-loss handling; no manual lock/status patches |
| Cloud Node Provisioner, member/provider | NodeProvision VM create/join/fence/delete; runtime bootstrap/capability checks | Own provider lifecycle and its status contract/interpreter; do not calculate training policy or risk | Verify provider termination and cleanup idempotence; certify restore capability on each new node |

These are logical ownership boundaries, not a requirement to split every controller
into a new repository. Stateful-Migration-System currently hosts both management
and member C/R executables. PV migration remains a supporting execution service.

Karmada RICs reflect and aggregate CR state. They do not provision VMs or execute
CRIU. Each system must own the interpreter/schema contract of the CRs it produces.
DDP ranks must remain in one member cluster; public bursting moves the whole world.

## Existing Code Crosswalk

Paths below are relative to the project directory containing the sibling repositories.

| Area | Existing implementation | Decision |
|---|---|---|
| Enrollment | System/internal/management/discovery.go | Replace defaults-based policy creation with explicit user policy discovery (implemented in this slice) |
| Capacity and recovery | System/internal/management/capacity.go, replacement_controller.go, recovery_controller.go | Keep UID-bound safety and verified cleanup; do not bypass deletion gates |
| Checkpoint authority | System/internal/management/checkpoint_controller.go, replacement_checkpoint.go | Coordinator now creates periodic, emergency and replacement checkpoints; Replacement reads them |
| Risk/objective | System/internal/collector; System/internal/policy | Keep ingestion, freshness and input identity; audit availability prediction/objective against the paper before claiming equivalence |
| Runtime | System/internal/member/runtime.go; config/karmada/runtime-interpreter.yaml | Aligned rank timing and worker-session checks implemented; schema preserves member and aggregated measurements |
| Iteration measurement | My_FluidCR-work/fluidcr/backends/pytorch.py; fluidcr/iteration.py | Completed-update boundary windows replace optimizer-call-only latency; GPU overhead remains unmeasured |
| Restore execution | Stateful-Migration-System/internal/member/reconciler.go; internal/management/restore.go | Keep staging/fencing/verification; formalize member evidence and its management adapter |
| Checkpoint/release | Stateful-Migration-System/internal/checkpoint/fluidcrmigration_controller.go | Completed archives remain Completed; SurvivorReleased condition records later release progress/failure |
| Runtime restore | My_FluidCR-work/fluidcr/launcher.py; fluidcr/backends/pytorch.py | Mandatory model/optimizer/step/RNG load failures now raise; typed attempt-bound load receipt remains pending |
| VM lifecycle | In-aws-create-WorkerNode/internal/controller/ml/nodeprovision_controller.go, restore_runtime.go | Keep cloud execution here; validate node capability and deletion without policy ownership leakage |

## Implemented Slice: User-Owned Enrollment

- Discovery no longer creates TrainingPolicy or reads automatic-policy-defaults.
  A StatefulSet without a user policy produces no Runtime or cloud capacity.
- One policy must reference the live workload API version, kind, name and UID.
  Duplicate allocators, recreated workloads and foreign Runtime identities fail closed.
- Policy Manager creates TrainingRuntime and its propagation policy. An omitted
  spec.runtimeRef defaults to <policy-name>-runtime. An explicit name remains supported,
  but an existing object must have the expected policy UID label and matching spec.
- Discovery records the current policy generation and workload UID. Both capacity
  allocation and periodic checkpoint creation wait for current discovery, including
  policies without the historical automatic label.
- A verified group transition records status.placement, not changes to user-owned
  spec.sourceCluster or spec.runtimeRef. Operational readers resolve the active
  cluster/runtime only when the policy/workload/initial intent identities match.
- The shared management ClusterRole cannot create TrainingPolicy or SpotRiskProfile.
  Risk input is read-only; Collector can still patch its status. Policy patch remains
  necessary for operation annotations: RBAC alone cannot restrict patch to metadata.
  Per-component service accounts and admission enforcement remain future hardening.

The new placement status is execution-location bookkeeping after existing restore
verification. It is not a new CRIU attestation or a substitute for restore evidence.
It is also not the missing on-premise capacity decision or initial bursting scheduler.

## Enrollment and Upgrade

1. Apply the updated TrainingPolicy CRD before replacing controllers. Otherwise the
   API server can prune status.placement, preventing operational placement handoff.
2. Prepare user-owned SpotRiskProfile and TrainingPolicy for each workload. Use
   config/samples/12-training-policy.yaml and the risk feed documentation. Network
   settings belong in TrainingPolicy; risk observations come from SpotRiskProfile.status.
3. Ensure exactly one policy references each workload and a UID-matching ResourceBinding
   assigns the entire positive replica count to one cluster. Runtime discovery does
   not require the workload Pods to be Ready, avoiding a capacity/readiness bootstrap loop.
4. Rebuild and roll out the affected policy-manager and checkpoint-coordinator binaries
   from this revision together, and apply config/karmada/access.yaml. Other management
   executables sharing changed readers must also use this revision when enabled.
   This task has not built/pushed container images or changed a live cluster.
5. Observe status.discovery.ready, observedGeneration and workloadUID; then Runtime
   collection, risk freshness, capacity decisions and periodic checkpoints.

Do not delete an existing automatic policy merely to rename it: its UID labels own
capacity and active operations. An existing correctly configured policy can be retained
as user-managed intent; the old automatic label is no longer required or authoritative.
Legacy defaults ConfigMaps are ignored and may be archived independently.
Do not remove an explicit Runtime reference from an existing policy casually: doing so
changes the selected Runtime and invalidates previously recorded initial runtime identity.

Example controller-owned placement status after an already verified migration
(illustration only; users must not manually patch this evidence):

```yaml
status:
  placement:
    policyUID: "<current-policy-uid>"
    workloadUID: "<referenced-workload-uid>"
    initialSourceCluster: onprem
    initialRuntimeName: trainer-runtime
    activeCluster: aws
    runtimeRef:
      name: "<controller-created-target-runtime>"
    verified: true
    verifiedAt: "2026-09-29T00:00:00Z"
```

## Delivery Units

| Order | Scope | Required evidence before completion |
|---|---|---|
| 2 | Implemented locally: Coordinator checkpoint ownership and separate release condition | Contention/attempt identity tests pass; live multi-controller restart and upgrade tests remain |
| 3 | Partial: fail-closed mandatory loading; RIC generation envelope compatibility | Typed load receipt, survivor-loss fallback and fully automated GPU restore remain |
| 4 | Implemented locally: iteration windows and Runtime CR/RIC schema propagation | Timing/aggregation/freshness/schema tests pass; real GPU overhead and Lua round trip remain |
| 5 | Partial: paper Eq.1-5 opt-in calculator with feasibility/input checks | Forecast holdout evaluation, Eq.6-7 allocation integration and exact iteration-triggered execution remain |
| 6 | On-premise shortage and whole-world public placement | Sufficient/insufficient capacity, concurrent submissions, heterogeneous GPU and pending-cause tests; no split world |
| 7 | Partial: policy suspension blocks new intent without cancelling existing work | Automatic cancellation/cleanup/retention state machine and live provider termination remain |

Existing static-risk experiments and manually assisted restores are useful diagnostic
evidence, but do not satisfy these automated acceptance criteria.

## Validation

Regression coverage includes no implicit policy creation, automatic Runtime creation
for explicit user policies, duplicate/stale identity refusal, generation-based gates,
preservation of user spec through verified transitions, runtime watch routing, placement
identity rejection, CRD status pruning, and management RBAC ownership.

Run go test -mod=readonly ./..., go vet ./..., and go build -mod=readonly ./cmd/....
For this slice, go test -mod=readonly ./... -count=1, go vet ./..., and
go build -mod=readonly ./cmd/... all passed locally on 2026-09-29.
The repository diff also passed git diff --check.
Unit/fake-client and schema checks do not prove Karmada propagation or EC2/GPU restore.
Live admission, scheduling, cloud cost, NCCL/CRIU and multi-cluster E2E remain untested
for this revision until a controlled rollout.
