# v6 NodeRequest workflow verification

## Scope

This revision changes the user input from TrainingPolicy to NodeRequest and
checks controller contracts locally. It does not certify a live AWS/Karmada/GPU
deployment. No cloud instances, interruption experiments, or image publications
are implied by these tests.

## Execution contract

1. User creates SpotRiskProfile and NodeRequest with VM specifications/count and
   workload apiVersion/kind/name. The current supported workload is an apps/v1
   StatefulSet in the same namespace; this reference is not a workload template.
2. Spot VM Management Controller validates fresh risk and allocates initial
   NodeProvisions before the workload exists. Duplicate requests for the same
   workload are rejected before allocation. The Cloud Node Provisioner performs
   actual VM creation and node/GPU qualification.
3. User deploys the prepared workload and propagation policy after capacity is
   ready. Training Lifecycle Controller binds the workload UID once and discovers
   placement. It creates TrainingRuntime only through the existing placement and
   identity gates. Recreated workloads require a new request, not silent rebinding.
4. Training Runtime Controller supplies runtime measurements. Checkpoint/Recovery
   Coordinator updates the periodic schedule using fresh evidence; schedule
   changes do not authorize overlapping save/restore operations.
5. Planned replacements wait for target readiness and checkpoint quiescence.
   Interruption handling prepares replacement capacity independently of the
   schedule pause acknowledgement on the eligible partial-replacement route.
   Urgent saves still respect active operations.
   Once the save deadline expires, fallback selection must not wait for a new VM.
6. Restore requests require durable checkpoint evidence, target readiness, source
   fencing and identity checks. Cleanup requires verified rank coverage and
   unchanged survivor identities, not just a Ready Pod.
7. Completion alone does not delete the user-owned NodeRequest. Workload deletion
   and NodeRequest deletion remain separate user operations. Generated
   NodeProvisions currently carry ownership labels, not NodeRequest ownerReferences.
   Deleting NodeRequest alone therefore does NOT implement whole-pool teardown.
   Explicit, quiesced NodeProvision cleanup and provider finalizer verification
   remain necessary; a coordinated request-deletion controller is a remaining gap.

## Paper correspondence

Reference: Cost-Efficient Training and Checkpointing for Large Models on
Preemptible Cloud VMs (local paper in the project reference collection).

- The paper checkpoint cost equations and memory constraint have local equation
  tests. Runtime iteration/checkpoint measurements feed interval selection.
- The optional SARIMA feed uses an hourly horizon, causal smoothing, seasonal
  modeling and history/refit constraints. Availability drops are a risk proxy,
  not observed g5.xlarge interruption probabilities.
- Fixed-size DDP capacity selection is a project adaptation: the implementation
  bounds joint survival using floor(-ln(alpha)/(lambda * horizonHours)), then
  applies worker-count and On-Demand constraints. This is NOT a literal
  implementation of the paper's throughput/shard-based resource expansion.
- Aggregate forecast normalization by population is also a project adaptation.
  The collector now checks consistency against the forecast, population and cap
  rather than accepting an arbitrarily understated per-instance hazard.
- Price-based decisions require real positive price inputs and calibrated loss
  costs. The SARIMA feed alone does not provide this economic calibration.
- The checkpoint model assumes compatible pure data parallelism. The current
  wall-clock schedule approximates an iteration-based interval; asynchronous
  GPU-to-DRAM/storage overlap is not proven by controller unit tests. Mixed
  Spot/On-Demand and all-On-Demand checkpoint economics are not a faithful
  reproduction of the paper.

Direct group recovery still requires a recoverable full checkpoint and group
quiescence before provisioning. Therefore the stronger requirement "every
preemption starts group save and VM provisioning in parallel" is NOT implemented
for every route. Emergency partial recovery with full-group fallback is distinct
from a forced-group configuration; do not present them as identical workflows.

Use "paper-inspired fixed-size DDP adaptation", not "identical paper policy",
when describing the combined VM-count and checkpoint implementation.

## Local evidence and limits

- Go tests cover policy equations, CRD schema/CEL, controller ownership, initial
  provisioning, UID binding, schedule changes, partial/group recovery and cleanup.
- Python installer/render tests: 44 passed.
- Python risk-feed tests: 13 run, one real SARIMA-fit test skipped because
  statsmodels is not installed in this Python environment.
- Fake Kubernetes clients do not prove API-server watch timing, Karmada
  propagation/status reflection, cloud capacity, GPU checkpoint compatibility,
  distributed training continuity or provider finalizer completion.

## Live acceptance checks before release

Use a dedicated namespace, pinned source/image versions and an explicit budget.
Do not reuse existing TrainingPolicy objects as NodeRequests; follow
v6-migration.md.

1. Submit a two-worker NodeRequest without UID; verify exactly two owned VMs and
   qualified GPU nodes, then deploy the StatefulSet and verify one-time binding.
2. Verify all ranks report matching workload identity/world size and advancing
   steps. Change iteration timing and confirm the schedule updates without
   concurrent checkpoints.
3. Exercise a planned policy transition; verify target readiness precedes save,
   restore verification precedes old VM deletion, and survivor identities match.
4. Inject an interruption with an explicit approved test. Verify urgent save and
   replacement allocation proceed independently; force save failure/deadline
   expiry and check restoration from the latest valid full checkpoint.
5. Reject incomplete/duplicated recovery receipts and terminating targets without
   deleting source capacity. Quiesce operations and explicitly clean up the owned
   NodeProvisions before removing the request; confirm provider termination, not
   just disappearance of the CR. Whole-pool deletion automation is not certified.

Capture object UIDs/generations, timestamps, instance IDs, checkpoint hashes,
per-rank restored/advanced steps and finalizer results. Until this evidence exists,
report cloud E2E as not run rather than passed.
