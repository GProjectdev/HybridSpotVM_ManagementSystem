# Restore automation: rollout and validation

## Current implementation boundary

This guide distinguishes implemented controller paths from live validation.
Passing unit tests does not establish CUDA/DDP restore compatibility.

| Scenario | Current support |
| --- | --- |
| Periodic coordinated checkpoint and durable archive export | Existing implementation; verify every rank's exported archive |
| Policy-driven Spot to OnDemand | Same AWS cluster, nonzero target ranks, preserved survivors |
| Policy-driven OnDemand to Spot | Same restrictions; successor chains retain logical worker slots |
| Spot interruption notice | With replacement enabled and fresh runtime evidence, creates an OnDemand replacement operation |
| On-prem to AWS by directly changing PropagationPolicy | NOT end-to-end automated; do not use this as a restore trigger |
| Rank 0 replacement, complete group loss, notice arriving after source loss | NOT supported by the partial-rank replacement path |

The last two rows are implementation gaps, not merely unexecuted tests. Do not
report all three requested recovery scenarios as complete. The current member
fence actuator requires a reachable source and does not treat a missing or
NotReady Kubernetes Node as proof that an EC2 instance cannot still write.

## Prerequisites

### Recovery primitives added on 2026-09-28

These primitives are tested independently but do NOT change the unsupported
scenario rows above into end-to-end support:

- `internal/management/group_checkpoint.go` selects a whole completed,
  exported, UID-bound source round and rejects mixed/incomplete rank sets.
  It is not yet called by the recovery state machine.
- Provisioner `spec.fence` persists an operation-bound AWS instance termination
  receipt. See its `docs/recovery-fencing.md`; requesting it is destructive.
- FluidCR `POST /prepare-group` and `POST /resume-group`, and the matching
  Stateful `ctrlapi.Client.PrepareGroup/ResumeGroup`, use an operation-bound
  whole-world contract. Older payloads fail closed; there is no fallback to
  unscoped resume. Producer metadata, source-world identity and artifact hashes
  are required; old artifacts without that metadata are rejected.
- New injected Pods can obtain source-world identity from their UID-verified
  StatefulSet owner's management-origin label. Existing Pods are not restarted,
  and unlabelled parents do not enable producer metadata automatically.
- FluidCR's independent group-control CLI/image can prepare shared checkpoint
  state without reaching the lost rank's HTTP server. It remains a tool awaiting
  an operation-owned Job/controller caller, not a completed recovery path.
- The Stateful group HTTP client requires the expected world size and immutable
  producer generation and rejects smaller worlds or stale success responses.
  The producer generation is not a Kubernetes resource generation.

The resume endpoint releases the prepared world's operation-owned locks, not
one rank at a time. Call prepare only after the old world is fenced and keep
targets stopped until it succeeds. Call resume only after every target rank
has restore evidence. The control API checks scope, not cloud fencing truth:
the orchestration caller must verify the supplied evidence. Selecting a
container archive alone is insufficient. These clients are not yet invoked
by the automatic recovery state machine.

Remaining integration sequence:

1. Introduce one durable full-group operation owning checkpoint, replacement
   capacity, source fencing and the restore request, with restart-safe phases.
2. Quiesce periodic checkpoints and block new source/target Pods until the
   operation can prove source fencing. Node disappearance alone is insufficient.
3. For cross-cluster migration, stage intent before placement dispatch, then
   fence source and prepare PVs. A controller observing an already-dispatched
   PropagationPolicy change cannot retroactively prevent a cold start.
4. Bind every restored Pod to the operation, restore the same round's shared
   state, resume the group and verify fresh per-rank step advancement.
5. Only then remove old capacity. Test rank-0 loss, entire-source loss,
   controller restart, stale receipts and duplicate interruption events.

Roll out schemas before controllers and rebuild the FluidCR payload as well
as the operator. Do not enable an unfinished full-group recovery path merely
because the new images build.

- Updated Stateful migration operator, FluidCR payload, CRDs and RBAC.
- Provisioner runtime package containing the tested custom CRI-O restore fixes.
  Follow the provisioner's `docs/restore-runtime-rollout.md`; new nodes must
  pass runtime verification and GPU registration, not receive capability labels
  manually as a substitute for verification.
- Durable checkpoint store reachable from replacement nodes; identical required
  workload volumes available. No assumption that NFS path existence proves the
  application state is consistent.
- Current TrainingRuntime rank/Pod UID/node/checkpoint evidence and a supported
  surviving rank. Keep rank 0 on a node that this partial test will not replace.
- Controllers must be running; earlier cleanup scaled them to zero.
- Budget for one temporary extra EC2 worker during a replacement.

## Build and deploy System

Run in the updated System checkout on the Linux build host. These commands do
not rebuild the separate provisioner, Stateful operator or runtime .deb.

```bash
set -euo pipefail
export REPOSITORY=docker.io/jeongseungjun/hybrid-spot-vm-system
export TAG=restore-auto-$(date -u +%Y%m%dT%H%M%SZ)
for COMPONENT in policy-manager checkpoint-coordinator spot-recovery-controller; do
  buildah bud --build-arg COMPONENT="$COMPONENT" \
    -t "$REPOSITORY:$COMPONENT-$TAG" .
  buildah push "$REPOSITORY:$COMPONENT-$TAG"
done

kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k config/crd
for COMPONENT in policy-manager checkpoint-coordinator spot-recovery-controller; do
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    set image deployment/"$COMPONENT" manager="$REPOSITORY:$COMPONENT-$TAG"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    scale deployment/"$COMPONENT" --replicas=1
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    rollout status deployment/"$COMPONENT" --timeout=300s
done
```

Existing operations must finish before upgrading their schemas/contracts; do
not edit an in-flight operation to fabricate newly required evidence.

## Enable and observe

Set NS and POLICY to the newly deployed test workload, not the names of resources
deleted during the preceding cleanup.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  patch trainingpolicy "$POLICY" --type=merge \
  -p '{"spec":{"replacement":{"enabled":true}}}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" \
  get spotreplacements,spotrecoveries,restorerequests -w
```

For the policy case, change the configured risk input using
`docs/risk-feed.md`, and confirm the desired market mix actually changes.
Changing lambda alone need not cross the decision boundary. Test the reverse
direction only after the first operation completes.

For the interruption case, use the existing spot-watcher with an actual notice
in an isolated test environment. Editing NodeProvision.status manually tests
only the orchestration entry point, not IMDS detection or AWS interruption
handling. Do not terminate a production instance as a smoke test.

The emergency path creates replacement capacity without waiting for it to become
Ready before requesting its typed partial checkpoint. An already in-flight
checkpoint must first finish; the controller never overlaps checkpoint rounds.
Ordinary periodic checkpoints are quiesced during the operation.
The target source may disappear before this completes; that is a failure, not
an excuse to bypass fence checks or declare recovery successful.

## Success evidence

Capture the following JSON and logs before cleanup:

1. Original NodeProvision UID, instance ID, rank-to-Pod UID/node mapping.
2. UID-bound SpotReplacement and desired market; one operation for duplicate notices.
3. New NodeProvision UID/instance ID, verified runtime and GPU allocatable.
4. Completed partial checkpoint with every selected archive's durableRef and SHA256.
5. RestoreRequest/RestorePlan identity binding, old Pod UID fence evidence,
   new Pod UID, preserved survivor UIDs and restore annotations.
6. Runtime's matching checkpoint round and increasing globalStep after resume.
7. RestoreRequest Verified, then cleanup and SpotReplacement Completed.
8. Cloud instance termination confirmation for the old node and unchanged
   final logical worker count.

Running/Ready alone is not success. A restored paused process alone is also not
success. Check CRI-O's restored-container evidence, app resume, progress, and the
controller's Verified receipt together.

## Remaining implementation before the full three-case final test

Cross-cluster transfer needs one durable operation coordinating intent capture
before placement removal, full-group checkpoint, source write fencing, NFS/PV
staging, target capacity, restore admission, dispatch release and application
resume/progress verification. Current PVMigration assumes a caller-provided
sourceFenced assertion and supports claim-template NFS Retain volumes; it is
not an automatic fence actuator or generic shared-PVC migration.

Rank-0/full-group loss additionally needs coordinated rendezvous reconstruction
and an authoritative infrastructure fence for an unreachable source, plus
selection of the latest fully exported consistent checkpoint after abrupt loss.
Keep these cases blocked rather than weakening the existing partial-rank gates.
