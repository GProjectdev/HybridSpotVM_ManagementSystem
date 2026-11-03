# E1 multi-manifest Work status repair

## Scope and evidence

Base: upstream Karmada v1.19.0, commit 6d7b233a54d59c8473803901768153f6e4353d02.
The upstream WorkStatusController.mergeStatus discards all existing entries.
PV migration emits a Work containing both a PV and a PVC; alternating reflection
therefore loses one half of the binding evidence. This patch updates one ordinal
without discarding peers, without mutating its input, and removes duplicate entries
for the updated ordinal. Existing resourceVersion conflict retries remain intact.

The E1 shared-PVC layout remains unchanged. Changing to volumeClaimTemplates does
not repair the underlying status overwrite. PV/PVC UID, claimRef, NFS, migration
UID and restore validation must not be bypassed or manually marked Completed.

This is an experiment-specific controller-manager patch, not an upstream release.
It does not assert that every multi-manifest Work behavior is supported. Member
clusters use Push mode here; an agent-mode deployment needs a separate review.

## Build and deploy on MGMT

Keep this checkout separate from the existing /pv checkout (main is unchanged).
All commands below run on MGMT. Existing kubeconfig exports are required.

```bash
cd /root/hybridspot-validation
git clone --single-branch --branch e1-work-status-v1.19.0 https://github.com/GProjectdev/Karmada_with_PVMigration.git karmada-e1-status
cd /root/hybridspot-validation/karmada-e1-status
git log -1 --oneline

(
  set -euo pipefail
  IMAGE=docker.io/jeongseungjun/karmada-controller-manager:v1.19.0-e1.1
  buildah bud -f Containerfile.e1 -t "$IMAGE" .
  buildah push "$IMAGE" "docker://$IMAGE"
)

(
  set -euo pipefail
  : "${MGMT_KUBECONFIG:?}"
  BACKUP=$(mktemp -d /root/e1-karmada-backup.XXXXXX)
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n karmada-system get deployment karmada-controller-manager -o yaml > "$BACKUP/controller-manager.yaml"
  printf 'BACKUP=%s\n' "$BACKUP"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n karmada-system set image deployment/karmada-controller-manager karmada-controller-manager=docker.io/jeongseungjun/karmada-controller-manager:v1.19.0-e1.1
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n karmada-system rollout status deployment/karmada-controller-manager --timeout=300s
)
```

The docker:// string is a Buildah registry transport, not a Docker CLI command.
Only the controller-manager image changes. Preserve its existing arguments,
kubeconfig mounts, service account, and disabled legacy PV controllers. Do not
apply upstream installation manifests. No PV controller, trainer, policy-manager,
or stateful-member rebuild is needed for this patch.

If the new manager fails to start, stop the experiment and restore the old image:

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n karmada-system set image deployment/karmada-controller-manager karmada-controller-manager=docker.io/karmada/karmada-controller-manager:v1.19.0
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n karmada-system rollout status deployment/karmada-controller-manager --timeout=300s
```

Rollback reintroduces the multi-manifest limitation; do not continue E1 on it.

## Fresh experiment

The previous run was deleted and its Spot instances terminated. Do not resume it.
Keep source GPU workers, NFS, WireGuard and downloaded evidence intact.
Use the updated System runner and a NEW run ID:

```bash
cd /root/hybridspot-validation/System
git fetch origin restore-automation-20260928
git merge --ff-only origin/restore-automation-20260928
kubectl --kubeconfig="${ONPRE1_KUBECONFIG:?}" get nodes -o custom-columns='NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,GPU:.status.allocatable.nvidia\.com/gpu'
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" get clusters

export RUN="e1s-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e1/$RUN"
python3 evaluation/e1_experiment.py render --policy-source evaluation/policy-source.json --run-id "$RUN" --out "$EVIDENCE"
jq '.spec.policy' "$EVIDENCE/policy.unbound.json"
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n karmada-system get deployment karmada-controller-manager -o json | jq '{name:.metadata.name,images:[.spec.template.spec.containers[].image],ready:.status.readyReplicas}' > "$EVIDENCE/karmada-controller-image.json"
printf 'RUN=%s\nEVIDENCE=%s\n' "$RUN" "$EVIDENCE"
python3 -u evaluation/e1_experiment.py execute --evidence "$EVIDENCE" --confirm "$RUN"
```

Before execute: both source workers must be Ready with one GPU each; both clusters
Ready; policy alpha=0.05, minOnDemand=0, fixedOnDemand=0. Inspect rendered resources
and confirm the patched manager is ready. Source training, export, and migration
are still real operations and may fail for independent network/storage reasons.

In another terminal use the printed absolute EVIDENCE path:

```bash
tail -n 30 -F "$EVIDENCE/timeline.jsonl"
```

During migration, inspect each PV/PVC Work for the CURRENT run (old Works may remain):

```bash
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n karmada-es-aws get works -o json | jq '.items[] | select(any(.spec.workload.manifests[]?; .kind == "PersistentVolume") and any(.spec.workload.manifests[]?; .kind == "PersistentVolumeClaim")) | {name:.metadata.name,manifests:[.spec.workload.manifests[] | {kind,name:.metadata.name}],statuses:.status.manifestStatuses}'
```

Both PV and PVC entries must coexist, both Bound with matching binding UIDs and
expected NFS mapping. A Ready VM or a Completed prepare Job is not experiment
success. Require target rank 0/1 restore audits and resumed steps, source fencing,
and verified AWS placement. The runner deliberately reports a candidate requiring
evidence review, not an unconditional scientific success.

At completion download the exact tar.gz printed by DOWNLOAD. If final collection
fails, the updated runner still attempts remaining collection and packaging;
final_collection_failed means the archive is incomplete evidence. private/ is
excluded. Do not upload raw secrets or kubeconfigs. A second Ctrl-C can still
interrupt packaging; use `python3 evaluation/e1_experiment.py pack --evidence
"$EVIDENCE"` afterward if necessary.
