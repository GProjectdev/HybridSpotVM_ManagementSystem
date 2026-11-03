# E1 Shared PVC Release v5.0.3

## Scope And Stop Conditions

E1: two-rank DDP on onpre1 -> PropagationPolicy change -> cold AWS capacity ->
same complete checkpoint restored -> both ranks advance. This is an integration
evaluation, not a Spot cost or dynamic checkpoint interval comparison.

This release adds standalone dataset/checkpoint PVC migration, including the
checkpoint claim injected by the FluidCR webhook. Target PV and PVC use the
source NFS backing. This is metadata rebinding, NOT a storage-data copy.
No experiment VM or PVC is precreated in AWS. Shared NFS infrastructure and
controllers must already be reachable. Source StorageClass may differ from
AWS's default; the target PVC is explicitly prebound, not dynamically provisioned.

Do not run during another migration. Do not delete old resources by namespace
or wildcard. Keep the two existing onpre1 GPU workers and their NodeProvisions.
Do not reuse the previous checkpoint.py prepare/start/cleanup for this E1 run.

## 1. Update And Build On MGMT

Inspect local changes before each fast-forward. Preserve local configuration.
System branch: restore-automation-20260928. PV branch: main.

```bash
cd /root/hybridspot-validation/System
git status --short
git fetch origin restore-automation-20260928
git merge --ff-only origin/restore-automation-20260928
git rev-parse HEAD
(
  set -euo pipefail
  for COMPONENT in policy-manager placement-webhook; do
    IMAGE="docker.io/jeongseungjun/hybrid-spot-vm-system:$COMPONENT-v5.0.3"
    buildah build --build-arg COMPONENT="$COMPONENT" -t "$IMAGE" .
    buildah push "$IMAGE" "docker://$IMAGE"
  done
)

cd /root/hybridspot-validation/pv
git status --short
git fetch origin main
git merge --ff-only origin/main
git rev-parse HEAD
IMAGE=docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.3
buildah build -t "$IMAGE" . && buildah push "$IMAGE" "docker://$IMAGE"
```

Record pushed digests. Use digest-pinned images in deployment when available.
These commands have not been run against the user's registry by this patch.

## 2. CRDs, Interpreters, Images

Save existing deployments, CRDs and interpreters before changing them. Upgrading
only System is insufficient: old PVMetadata snapshots lack standalone PVC data.
The PV image is the same binary for management/member; preserve existing args,
kubeconfig mounts, RBAC, service accounts and cluster-name settings.

```bash
cd /root/hybridspot-validation/pv
(
  set -euo pipefail
  for CONFIG in "$KARMADA_KUBECONFIG" "$ONPRE1_KUBECONFIG" "$AWS_KUBECONFIG"; do
    kubectl --kubeconfig="$CONFIG" apply -f config/crd/bases/migration.dcnlab.com_pvmetadata.yaml
    kubectl --kubeconfig="$CONFIG" apply -f config/crd/bases/migration.dcnlab.com_pvmigrations.yaml
  done
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -k config/karmada/ric
)
```

Update exactly these containers (first inspect names with `get deployment -o json`):

| Cluster | Namespace / Deployment | Image |
| --- | --- | --- |
| MGMT | hybridspot-system / policy-manager | hybrid-spot-vm-system:policy-manager-v5.0.3 |
| MGMT | hybridspot-system / placement-webhook | hybrid-spot-vm-system:placement-webhook-v5.0.3 |
| MGMT | pv-migration-system / pv-migration-management | multicluster-pvmigration-system:v5.0.3 |
| ONPRE1, AWS | pv-migration-system / pv-migration-member | multicluster-pvmigration-system:v5.0.3 |

All image names have prefix `docker.io/jeongseungjun/`.
Use `kubectl --kubeconfig=... -n ... set image deployment/... CONTAINER=IMAGE`,
then `rollout status deployment/... --timeout=180s` for each. Back up first.
Do not replace provisioner, Stateful, GPU Operator or training images blindly.

## 3. Preflight

- Both onpre1 GPU workers Ready, GPU allocatable 1 each, no other GPU workload.
- Both workers: CRI-O runtime manifest, CRIU CUDA plugin, effective CRI-O driver
  library path and cuda-checkpoint execution checked. Worker1 results alone do
  not certify Worker2.
- NFS and WireGuard running; source/target workers can reach the same exports.
- Stateful management/member group restore, artifact agents, checkpoint executor,
  payload and group-control image configured; AWS member v5.0.1 retained.
- Karmada placement webhook reachable with valid CA and failurePolicy=Fail.
- Namespace fluidcr-realign-121040 exists on Karmada and both members.
- Source StorageClass legacy-nfs-client is available. Verify actual name.
- No stale StatefulSet, migration or restore operation for this fresh run ID.
- No target PVC with any of the new workload's claim names. Existing managed
  Pending PVC adoption is not fully protected by a dedicated admission gate;
  this release requires a clean target claim area. A conflicting claim is a
  preflight failure, not permission to delete or overwrite it.
- Capture baseline nodes, NodeProvisions, PV/PVC and component image digests.

## 4. Render And Prepare Source Only

Use the existing validated ResNet-50 config as input. Set storage_class to the
onpre1 class and restore_audit=true. Set a sufficiently high goal_steps so the
job remains active through migration; it is a stop limit, not a performance
measurement. No risk schedule or FIS interruption is used in E1.

```bash
cd /root/hybridspot-validation/System
export NS=fluidcr-realign-121040
export RUN="e1-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e1/$RUN"
# CONFIG must point to the reviewed source config, with legacy-nfs-client.
python3 evaluation/e1_render.py --config "${CONFIG:?}" \
  --policy-source evaluation/policy-source.json --run-id "$RUN" --out "$EVIDENCE"
export APP="eval-$RUN"

kubectl --kubeconfig="$ONPRE1_KUBECONFIG" apply --dry-run=server -f "$EVIDENCE/onpre-storage.json"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply --dry-run=server -f "$EVIDENCE/karmada.json"
```

Inspect outputs. onpre-storage.json contains source PV/PVC only. karmada.json
contains a replicas=0 StatefulSet and separate dependency/workload placement.
The policy remains suspended to prevent premature AWS capacity allocation.

```bash
(
  set -euo pipefail
  kubectl --kubeconfig="$ONPRE1_KUBECONFIG" create -f "$EVIDENCE/onpre-storage.json"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" create -f "$EVIDENCE/karmada.json"
  UID_VALUE=$(kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get sts "$APP" -o jsonpath='{.metadata.uid}')
  test -n "$UID_VALUE"
  PATCH=$(jq -nc --arg uid "$UID_VALUE" '{metadata:{labels:{"training.dcnlab.com/workload-uid":$uid}},spec:{template:{metadata:{labels:{"training.dcnlab.com/workload-uid":$uid}}}}}')
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch sts "$APP" --type=merge -p "$PATCH"
  jq --arg uid "$UID_VALUE" '.spec.workloadRef.uid=$uid' "$EVIDENCE/policy.unbound.json" > "$EVIDENCE/policy.json"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" create -f "$EVIDENCE/policy.json"
)
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" get pvc "$APP-data" "$APP-checkpoint"
```

Stop on any error; do not rerun create blindly. Both source PVCs must be Bound.
There must be no AWS experiment PVC and no experiment-owned AWS VM yet.
Check each claim name in AWS explicitly before starting, for example:

```bash
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get pvc "$APP-data" "$APP-checkpoint" --ignore-not-found
```

Expected: no resources. Include "$APP-model" when using GPT-2.
Then scale the Karmada StatefulSet to 2; never independently scale its member copy.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" scale sts "$APP" --replicas=2
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n "$NS" get pods -l "app=$APP" -o wide
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingruntime,pvmetadata -o json > "$EVIDENCE/source-observation.json"
```

## 5. Checkpoint And Move Gate

Do not change placement yet unless ALL are verified:

1. Rank 0 and 1 both advance, world_size=2, distinct source Pod UIDs/nodes.
2. Current TrainingRuntime matches those live UIDs and the Karmada workload UID.
3. Fresh PVMetadata includes dataset and injected checkpoint claim, PVC UID,
   bound PV spec and PVC spec for each; NFS server/path are correct.
4. Create one full-world FluidCRMigration using this policy's workloadRef,
   policy ownership labels and checkpoint-id; propagate only to onpre1.
   Do not use the one-rank application_failover_demo.py command unchanged.
5. Both checkpoint entries complete in the SAME checkpoint round, each with
   durableRef and SHA256; export completion can follow phase=Completed.

After gates 1-3, create the full-world checkpoint (no partialCheckpoint selector):

```bash
export CP="$APP-e1-checkpoint"
(
  set -euo pipefail
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicy "$APP" -o json > "$EVIDENCE/policy.live.json"
  jq -e '.spec.expectedWorldSize == 2 and .spec.sourceCluster == "onpre1" and (.metadata.uid | length > 0)' "$EVIDENCE/policy.live.json" >/dev/null
  jq --arg name "$CP" '
    . as $p | {
      apiVersion:"fluidcr.dcnlab.com/v1alpha1", kind:"FluidCRMigration",
      metadata:{name:$name, namespace:$p.metadata.namespace,
        labels:{"training.dcnlab.com/managed-by":"hybridspotvm-system",
          "training.dcnlab.com/policy":$p.metadata.name,
          "training.dcnlab.com/policy-uid":$p.metadata.uid,
          "training.dcnlab.com/role":"checkpoint"},
        annotations:{"training.dcnlab.com/checkpoint-id":$name}},
      spec:{workloadRef:$p.spec.workloadRef, resume:true, container:"trainer",
        ctrlPort:8298, appCheckpointTimeoutSeconds:300, kubeletTimeoutSeconds:300}
    }' "$EVIDENCE/policy.live.json" > "$EVIDENCE/checkpoint.json"
  jq '{apiVersion:"policy.karmada.io/v1alpha1", kind:"PropagationPolicy",
    metadata:{name:(.metadata.name+"-placement"),namespace:.metadata.namespace},
    spec:{resourceSelectors:[{apiVersion:.apiVersion,kind:.kind,name:.metadata.name}],
      placement:{clusterAffinity:{clusterNames:["onpre1"]}}}}' \
    "$EVIDENCE/checkpoint.json" > "$EVIDENCE/checkpoint-placement.json"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" create -f "$EVIDENCE/checkpoint-placement.json"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" create -f "$EVIDENCE/checkpoint.json"
)
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get fluidcrmigration "$CP" -o json > "$EVIDENCE/checkpoint-status.json"
```

Inspect both reflected rank entries and exports before moving; this last command
is a snapshot, not a wait or readiness assertion. Retain the member-side snapshot
as well. Never infer full-world completeness only from phase=Completed.

After recording the complete evidence, change ONLY the workload placement:

```bash
date -u +%FT%TZ | tee "$EVIDENCE/move-requested-at.txt"
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" patch propagationpolicy "$APP-placement" \
  --type=merge -p '{"spec":{"placement":{"clusterAffinity":{"clusterNames":["aws"]}}}}'
```

The controller must hold dispatch, create both target NodeProvisions, fence the
source world, create PVMigration and target PV/PVC, verify reciprocal Bound UIDs
and identical NFS backing, prepare restore, then release placement. Service and
script ConfigMap stay propagated to both clusters throughout.

Do not manually remove suspension, patch restore status, create target claims,
or force replicas to recover a stuck run. Record the blocked stage and diagnose.

## 6. Evidence And Acceptance

Collect source/target trainer logs with timestamps, the policy, runtime,
ResourceBinding, RestoreRequest, FluidCRMigration, PVMetadata, PVMigration,
Works, NodeProvisions, actual PV/PVC, events and relevant controller logs.
Start collection before migration: source Pods and temporary Works may disappear.

Pass requires both target ranks to resume from the same checkpoint and advance
beyond the restored step, with no concurrent source training and no manual target
VM/PVC preparation. Running Pods alone are insufficient. Compare dataset/model
identity and checkpoint NFS server/path; report rollback and actual service gap.

Report policy-change-to-resumed-progress, capacity time, volume preparation time,
restore time, manual interventions and failures. Do not subtract abnormal gaps
from measured results or report this as a cost advantage. After evidence backup,
stop only this run and terminate its owned target VMs; keep external onpre workers.

The current onpre1 GPU workers are AWS VMs joined over VPN. Describe this as a
hybrid-cluster testbed, not physical on-premises GPU hardware.
