# Emergency checkpoint fallback to group restore

## Recovery contract

An interrupted emergency checkpoint is not a usable recovery point. Recovery must
use one completed, durable full-group checkpoint round for every rank, including
surviving ranks. Never combine independently selected `latest.pt` files or mix
rank archives from different checkpoint rounds.

The management handoff must preserve the original policy, operation, source
NodeProvision UID, instance identity, checkpoint UID/generation and checkpoint ID.
A deadline is a reason to consider fallback, not proof that the old instance is
terminated. A completed partial checkpoint should retain its partial recovery
path rather than roll back unnecessarily.

## Safety gates

The deadline path is evaluated after replacement capacity becomes Ready. It does
not promise recovery within the interruption notice window. It requires the
recorded interruption deadline to have elapsed and the partial checkpoint to lack
valid completion evidence; a missing deadline does not activate this path.

- Require a validated interruption for the same source instance.
- Use the recorded interruption deadline, not a locally assumed 120-second timer.
- Pin a validated complete full-group recovery round before destructive actions.
- Prevent concurrent partial restore and group restore for the same operation.
- Quiesce checkpoint production before starting the group recovery actuator.
- Require provider-confirmed source-instance fencing for an unreachable source.
- Fence the surviving source Pods by UID before starting the restored world.
- Require group runtime verification before cleanup is considered successful.

If checkpoint evidence or source identity is missing, fail closed and expose the
reason. Do not remove finalizers or mark resources Completed to bypass a gate.

## Existing downstream prerequisites

The member checkpoint controller acknowledges the annotation
`training.dcnlab.com/group-fallback-cancel=<SpotReplacement UID>` only for a
managed replacement checkpoint with the same recovery-operation UID and explicit
`resume: false`. It reports current-generation `Failed` with message
`group fallback checkpoint cancelled: <SpotReplacement UID>`. This is a scoped
cancellation receipt, not a claim that restoration failed. The original Pod
checkpoint evidence is retained. Management must wait for this member report;
deleting the management-side CR alone is not an acknowledgement.

The Stateful Migration group actuator must support UID-bound source deletion and
provider fencing receipts. The Provisioner must implement `spec.fence` and report
`status.fence` only after positive EC2 terminal-state evidence. A Kubernetes
NotReady node or a missing Pod is not sufficient evidence of EC2 termination.

Check the deployed CRD and controller versions before a live experiment. Local
source code and passing unit tests do not prove these versions are installed.

This change requires the management controller images and the Stateful Migration
checkpoint-controller image to be rebuilt. Deploy the member cancellation support
before enabling the management handoff. The training image, FluidCR injection
image and Provisioner are not changed by this cancellation protocol. The existing
Provisioner fencing capability is still a prerequisite.

Apply `config/crd/spotreplacements.yaml` on the management API before rolling out
the updated management controllers. Its new `status.partialFallback` fields keep
the pinned source identities, interruption deadline and cancelled checkpoint
reference; an old schema may prune these fields. Keep the existing training and
Provisioner images unless their separately documented prerequisites are missing.

Rollout order: member checkpoint controller, management CRD, then management
controllers. Do not perform this rollout during an active evaluation. Building
images, pushing source and deploying to clusters are separate from local tests.

## Live validation evidence

Local regression coverage includes deadline gating, completed partial preservation,
member cancellation acknowledgement, pinned-round selection, CRD status pruning,
matching versus foreign infrastructure fences, and verified group cleanup through
the recovery controller. Both repositories passed `go test -count=1 ./...`.
These are local tests, not evidence of a successful AWS interruption experiment.

Use a separate short recovery-validation run, not a cost-evaluation run. Preserve:

1. The complete full-group checkpoint available before the interruption.
2. Spot notice identity and interruption deadline for the affected instance.
3. The incomplete emergency checkpoint and the recorded fallback decision.
4. The selected checkpoint reference and source Pod/NodeProvision identities.
5. Provider fencing, source-world shutdown and group RestoreRequest evidence.
6. Both ranks resuming from the same round and advancing after restoration.
7. Successful recovery cleanup and actual EC2 termination evidence.

Also test no prior complete checkpoint, a notice before its deadline, an already
valid partial checkpoint, and a changed source UID. These must not cause an
unverified rollback or a duplicate restored world.

Do not inject a real destructive interruption into an active cost experiment.
Live fault injection and container rollout are separate operator actions.
