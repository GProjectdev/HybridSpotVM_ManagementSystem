# Same-cluster market replacement contract

System management owns only Karmada-visible orchestration. It never calls a member
process API directly and never treats a boolean as proof that a rank was stopped.

When a generated policy slot's market differs from the policy decision
(`Spot` or `OnDemand`), PolicyManager stays fail-closed by default. It records
`status.policy.reason=replacement_required`, `replacementOperation`, and
`replacementNodeProvision`, but it does not create replacement capacity or
checkpoint objects. Replacement orchestration starts automatically only when the
TrainingPolicy has `spec.replacement.enabled=true`. The automatic producer
creates one UID-bound `SpotReplacement` from live `TrainingRuntime` and old
`NodeProvision` evidence: target rank, source pod name, source pod UID, source
node, and non-target survivor rank/pod UID/node baselines. It does not populate
future paused proof; survivor generation and pause-lock evidence are copied only
after the partial checkpoint completes.

Rank 0 partial replacement follows the same owned checkpoint and resume
contract as other targets. The operation name includes the
old NodeProvision UID to prevent replay or slot-name collision.

For an accepted operation, SpotRecoveryController's replacement reconciler
creates:

- Replacement `NodeProvision` with a bounded old-name/UID-derived name and
  `spec.marketType` matching the operation's desired market.
- A Karmada `PropagationPolicy` for the replacement node.
- After the replacement NodeProvision is Ready with instance evidence, a
  replacement `FluidCRMigration` with `spec.resume=false` and
  `spec.partialCheckpoint.targetRanks[]` only.

For an interruption/rebalance event with replacement enabled, the checkpoint
coordinator creates a UID-bound emergency operation targeting OnDemand.
Emergency operations request the partial checkpoint immediately after capacity
creation rather than waiting for node readiness. An existing checkpoint must
still finish first. Event handling is independent of periodic-checkpoint dedupe.

`SpotReplacement.spec.oldNodeProvisionRef.{name,uid}` binds the old worker.
`SpotReplacement.status.replacementNodeProvisionRef.{name,uid}` records the
replacement UID after creation. The replacement NodeProvision carries
`training.dcnlab.com/recovery-operation=<operation>`.

CheckpointCoordinator quiesces ordinary periodic checkpoints while a nonterminal
SpotReplacement exists for the policy. The coordinated checkpoint request schema
is `spec.partialCheckpoint.targetRanks[]`, with `spec.resume=false`; unknown
checkpoint fields are not emitted. Completed checkpoint evidence is consumed
from the source cluster `status.pods[]`, not from a synthetic
`status.partialCheckpoint` field. Each target rank must report
`phase=ContainerCheckpointed`, the original source pod UID, checkpointID binding,
and `checkpointFiles[]` entries with `containerName`, `filePath`, `sha256`, and
`durableRef`.
Management translates checkpoint `filePath` into RestoreRequest archive
`sourcePath` and emits a deterministic `targetPath` under
`/var/lib/kubelet/checkpoints/`; RestoreRequest archives do not use `filePath`.

Kubelet/CRIU archive evidence proves the target rank checkpoint archive exists;
it is not fencing evidence. Same-cluster safety also requires producer evidence
from Stateful/FluidCR/member admission that restore-plan admission binding is
ready, the old target pod was deleted gracefully with a UID precondition, and the
old UID is observed gone before the recreated pod is admitted for restore.

SpotRecovery cleanup remains fail-closed. Old NodeProvision deletion requires all
of the following before the existing UID/resourceVersion delete precondition is
used:

- Current TrainingPolicy and TrainingRuntime UID/generation checks.
- Current SpotReplacement typed partial checkpoint/restore contract.
- Replacement NodeProvision UID match, Ready phase, instance evidence, operation
  annotation match, and the operation's desired market.
- RestoreRequest `status.phase=Verified` with matching request, checkpoint,
  workload, source, target, operation, and runtime evidence.
- `status.verification.sourceFence` object with actuator fence evidence ID and
  observedAt; checkpoint archive evidence alone is insufficient.
- Same-cluster replacement evidence: `spec.sourceFenced=false`,
  `spec.volumesReady=true`, `partialRestore.preventPeriodicResume=true`,
  `partialRestore.targetRanks[]` with target pod UID, checkpoint ID, archive
  `durableRef` and `sha256`, plus `survivors[]` with rank, preserved pod UID, and state
  evidence such as `SurvivorPaused`/pause-lock state.

Planned market replacement does not require an AWS interruption notice.
Emergency cleanup additionally requires the operation-bound event and instance
signal. Both paths retain the same Verified restore and source-fence gates.
Completed successor chains preserve the original policy slot across repeated
market changes; original slot names are not recreated.

See [validation and remaining gaps](restore-automation-validation.md) before
claiming cross-cluster or whole-group recovery support.

Unsupported automation is intentionally not faked. If Stateful/FluidCR do not
publish the fence, partial-rank checkpoint/archive, target readiness, and survivor
UID evidence above, management reports Pending and does not delete the old
NodeProvision.

## Opt-in and evidence checks

These commands are for a controlled Karmada validation environment. Activating
replacement can create a paid On-Demand VM, and after all Stateful/member proofs
exist the controllers can gracefully delete the old target Pod and delete the old
NodeProvision. Do not run this against live training unless that side effect is
intended.

```bash
set -euo pipefail
K=${KARMADA_KUBECONFIG:?set KARMADA_KUBECONFIG to the MGMT/Karmada kubeconfig}
NS=fluidcr-demo
POLICY=trainer-auto
RISK="$(kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" -o jsonpath='{.spec.riskProfileRef.name}')"

# Check current rank/UID evidence before enabling replacement. Rank 0 is
# supported; all targets and preserved survivors must form a live world.
kubectl --kubeconfig="$K" -n "$NS" get trainingruntime \
  "$(kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" -o jsonpath='{.spec.runtimeRef.name}')" \
  -o jsonpath='{range .status.clusters[*].status.pods[*]}{.rank}{" "}{.name}{" "}{.uid}{" "}{.nodeName}{"\n"}{end}'

# Controlled risk input through spec only. This clears mutually-exclusive feed
# modes and lets the collector publish status from staticLambdaPerHour.
kubectl --kubeconfig="$K" -n "$NS" patch spotriskprofile "$RISK" --type=merge \
  -p '{"spec":{"endpoint":null,"trace":null,"paperEstimator":null,"staticLambdaPerHour":0.3}}'
kubectl --kubeconfig="$K" -n "$NS" get spotriskprofile "$RISK" -w \
  -o jsonpath='{.metadata.generation}{" "}{.status.observedGeneration}{" "}{.status.ready}{" "}{.status.lambdaPerHour}{" "}{.status.validUntil}{"\n"}'

kubectl --kubeconfig="$K" -n "$NS" patch trainingpolicy "$POLICY" --type=merge \
  -p '{"spec":{"replacement":{"enabled":true}}}'
```

Read-only success checks:

```bash
kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" \
  -o jsonpath='{.status.policy.reason}{" "}{.status.policy.replacementOperation}{"\n"}'

OP="$(kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" -o jsonpath='{.status.policy.replacementOperation}')"
kubectl --kubeconfig="$K" -n "$NS" get spotreplacement "$OP" \
  -o jsonpath='{.spec.oldNodeProvisionRef.uid}{" "}{.spec.partialCheckpoint.targetRanks}{" "}{.spec.pods[*].sourcePodUID}{" "}{.spec.partialRestore.preservedSurvivors[*].podUID}{"\n"}'

# Rank zero is allowed. Verify checkpoint/resume evidence rather than
# treating successful capacity creation as successful recovery.
kubectl --kubeconfig="$K" -n "$NS" get spotreplacement "$OP" \
  -o jsonpath='{.status.phase}{" "}{.status.message}{"\n"}'

MIGRATION="$OP-partial-checkpoint"
kubectl --kubeconfig="$K" -n "$NS" get fluidcrmigration "$MIGRATION" \
  -o jsonpath='{.spec.resume}{" "}{.spec.partialCheckpoint.targetRanks}{" "}{.spec.partialRankCheckpointRequired}{" "}{.spec.noPeriodicResume}{"\n"}'
kubectl --kubeconfig="$K" -n "$NS" get fluidcrmigration "$MIGRATION" \
  -o jsonpath='{range .status.clusters[?(@.clusterName=="aws")].status.pods[*]}{.rank}{" "}{.phase}{" "}{.podUID}{" "}{.checkpointID}{"\n"}{range .checkpointFiles[*]}{.containerName}{" "}{.filePath}{" "}{.sha256}{" "}{.durableRef}{"\n"}{end}{end}'

RR="$OP-restore"
kubectl --kubeconfig="$K" -n "$NS" get restorerequest "$RR" \
  -o jsonpath='{.spec.sourceCluster}{" "}{.spec.targetCluster}{" "}{.spec.sourceFenced}{" "}{.spec.partialRestore.preventPeriodicResume}{" "}{.spec.partialRestore.preservedSurvivors[*].pauseLockPath}{"\n"}'
kubectl --kubeconfig="$K" -n "$NS" get restorerequest "$RR" \
  -o jsonpath='{.status.phase}{" "}{.status.verification.sourceFence.evidenceID}{" "}{.status.verification.partialRestore.targetRanks[*].durableRef}{"\n"}'
```

The shared wire fixture is
`docs/fixtures/spot-replacement-restore-request.json`. Management tests load it
and also assert that pruned fields such as `partialRankCheckpointRequired` and
`noPeriodicResume` are not emitted.
