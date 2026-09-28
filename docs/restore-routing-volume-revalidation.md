# Restore Routing And Volume Revalidation

## Supported Contract

Checkpoint coordination still requires one shared NFS claim at `/checkpoint`.
This change does NOT make independent per-rank checkpoint directories work as
one shared checkpoint world. Use `volumeClaimTemplates` for ordinal data volumes
alongside the shared checkpoint claim. No files are copied by PVMigration.

- Same-cluster policy replacements now prefer partial, including rank zero.
  A live interruption also selects partial only with exact event/instance
  evidence. A fresh, complete runtime world, exact current source Pod UIDs,
  unfenced Ready source node and at least one preserved survivor are required.
  Current source identities are never inferred from historical archive UIDs.
  Set `training.dcnlab.com/planned-partial=disabled` on the TrainingPolicy to
  retain group routing for new operations during rollout.
- Stale/unavailable runtime, no survivors and cross-cluster moves retain
  full-group restore. A generation namespace does not roll back live optimizer
  state after an uncoordinated loss.
- Existing partial operations retain ownership. An interruption during partial
  now reports `emergency_replacement_blocked` with the operation name instead of
  disappearing silently. Automatic cancellation/handoff of an in-flight partial
  is NOT implemented. Complete existing partial operations before testing a
  different interruption; do not remove ownership/fence records to bypass this.

The FluidCR-anhvt target manifest, generation-scoped rendezvous and survivor
pause/rebuild protocol are the runtime basis. Deploy the integrated My_FluidCR
payload, not the unmodified upstream example: the controller also requires
checkpointID, immutable round artifacts, survivor pause evidence and owned
resume generation checks. Resume only after all mapped target Pods are ready;
rank-zero rendezvous is re-created while peers reconnect. GPU/NCCL migration
and timeout behavior require the acceptance run below.

## Volume Handling

For cross-cluster group placement with ordinal data claims:

1. Enable metadata discovery on the source StatefulSet using
   `migration.dcnlab.com/pv-metadata=enabled`; run the PV management/member
   controllers and wait for source PVMetadata snapshots.
2. Provision the target shared checkpoint claim against the same NFS server/path.
   The group prepare Job independently verifies this shared backing.
3. Management validates the UID-bound source fence receipt and target Prepared.
4. Management places an operation-owned dispatch hold on the ResourceBinding,
   retaining its source placement, and automatically creates `group-pv-<RR UID>`.
   Its mappings cover every restore ordinal and every volumeClaimTemplate.
5. The PV operator reconstructs target PVs from source metadata, preserving NFS
   paths. It detaches completed Works without deleting their PVs.
6. Both placement controller and admission require the exact PVMigration UID,
   current Completed status, all claim mappings and applied/detached Works.
   Only then is placement released and this operation's dispatch hold removed.

Shared checkpoint preparation does not attest ordinal data volumes. A stale PV
operation, changed workload UID or missing metadata leaves placement blocked.
Existing holds owned by other operations are not removed. Same-cluster AWS
replacement reuses existing claims; it should not create a fake PVMigration.

## Local Verification

Run in subshells so a failed command does not exit the interactive root shell:

```bash
( set -euo pipefail
  cd /root/hybridspot-validation/System
  go test -p 4 -mod=readonly ./...
  go vet -p 4 -mod=readonly ./...
)
( set -euo pipefail
  cd /root/hybridspot-validation/stateful
  go test -p 4 -mod=readonly ./...
  go vet -p 4 -mod=readonly ./...
)
```

New tests cover automatic PV operation creation/UID binding/idempotency, volume
release refusal, actual admission JSON with VCT, source-snapshot mismatch,
partial selection and interruption ownership conflicts. These are local tests,
not a claim of GPU or multi-cluster E2E success.

## Image And RBAC Update

Transfer or commit the verified source changes before building on MGMT. Do not
build an older remote commit and assume these local changes are present.
Build new immutable tags for System `policy-manager`, `checkpoint-coordinator`,
`spot-recovery-controller`, `placement-webhook`, and `stateful-migration-operator`. Use the existing
Dockerfiles (`--build-arg COMPONENT=...` for System). Record pushed digests and
roll out by digest, preserving controller arguments and service accounts.

Apply System `config/karmada/access.yaml` to Karmada: management now needs
`get,list` on `pvmetadata.migration.dcnlab.com`. Update every Stateful management
deployment using the changed suspension controller. Rebuild the My_FluidCR
payload: v2.0.2 still rejects rank-zero partial targets in its HTTP contract.
The Stateful runtime overlay is updated too; rebuild it if that build path is
used. Pin the new payload digest in FluidCR admission and create fresh test
workloads/checkpoints with it. Changing admission does not update running
Pods or the payload captured in old archives. The runtime .deb is unchanged.

## AWS Acceptance And Screenshots

Keep on-prem GPU migration deferred. For AWS-only acceptance:

1. Start from the captured empty workload/node baseline. Deploy the DDP workload
   with the shared checkpoint claim and optional ordinal data claim templates.
2. Capture automatic TrainingPolicy, NodeProvision creation and two Ready ranks.
3. Capture a full checkpoint with exported durableRef/SHA256 for both ranks.
4. Change risk policy and validate automatic partial replacement and fresh
   runtime advancement for every rank; no partial opt-in is required.
5. Test healthy planned partial with rank zero and a nonzero rank. Capture survivor
   Pod UID before/after, target UID replacement and fresh increasing steps on
   both ranks. Complete this operation before any interruption test.
6. Test a live interruption with no active partial. Capture the notice
   instance/event identity, new NodeProvision, exact target UID replacement,
   unchanged survivor UID, checkpoint generation and owned resume receipt.
   Separately test unavailable-source fallback using a complete durable group
   checkpoint. Capture source fences, prepare/resume Jobs and fresh Verified
   runtime. Running Pods alone are not restore proof.

Useful capture commands (MGMT, after loading release environment):

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicies,spotreplacements,restorerequests -o wide
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get pvmetadata,pvmigrations -o yaml
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions,pods,pvc -o wide
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get restoreplans -o yaml
```

PVMigration automatic creation/rebinding belongs to the cross-cluster case;
do not present an AWS-only replacement as its E2E verification. A CPU storage-only
cross-cluster test can validate the PV component separately, but is not GPU DDP
restore validation. Retain all request/plan/fence evidence until verification.
