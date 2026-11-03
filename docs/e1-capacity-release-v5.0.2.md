# E1 Capacity Release v5.0.2

Historical release: for automatic standalone shared PVC preparation, use
[v5.0.3](e1-shared-pvc-v5.0.3.md). Do not prebind target PVCs for that evaluation.

## Scope

E1 validates a managed two-rank DDP move from `onpre1` to `aws`, including
cold target VM provisioning and continuation from one complete checkpoint.
It is not a checkpoint-period comparison or proof of Spot cost savings.
This release changes only policy-manager's group placement capacity loop.
Previously rank 1 was not requested until rank 0 had finished VM bootstrap.
Both ranks are now requested before waiting. RestoreRequest creation still
requires both NodeProvisions to report Ready with nonempty node names.

Unit coverage checks both NodeProvisions and their PropagationPolicies,
idempotence while pending, one-ready and missing-node-name barriers, and the
two-rank RestoreRequest mapping. This is not a live AWS restore certification.

## E1 Preconditions

1. Use a two-rank, single-container DDP StatefulSet enrolled in TrainingPolicy
   with the correct workload UID, sourceCluster onpre1, expected world size 2,
   and AWS capacity settings. Do not use the one-rank Existing-capacity
   `scripts/application_failover_demo.py` unchanged for this experiment.
2. Placement admission must hold the old ResourceBinding assignment and record
   pending AWS placement. Changing PropagationPolicy without this webhook can
   bypass checkpoint/fence ordering. Preserve its TLS and release identity.
3. Before moving, collect a complete exported checkpoint for both ranks and a
   current TrainingRuntime source Pod UID/node snapshot. Completed alone is not
   sufficient: each archive needs durableRef and SHA256.
4. The source and target writable checkpoint PVC must refer to the same NFS
   server/path at /checkpoint, without subPath. Two claims with the same name
   but independently provisioned directories do not satisfy this contract.
   The Provisioned path does not automatically prepare a standalone shared
   checkpoint claim. Prebind it on AWS. VolumeClaimTemplate migration is a
   separate PVMigration path and does not copy arbitrary storage data.
5. AWS capacity configuration must bootstrap the reviewed CRI-O/CRIU/CUDA
   runtime. Target nodes require restore capability certification and fresh
   artifact reports. Ready/GPU allocatable alone does not certify restore.
6. Stateful management/member, artifact agent, checkpoint executor, workload
   payload, group-control Job image and admission must implement the group
   contract. Inspect actual arguments/images on MGMT and both members; an old
   image tag or a Running Pod alone does not prove compatibility. Do not
   replace the customized onpre1 provisioner image blindly.
7. No competing legacy checkpoint/restore controller may own this workload.
   Identify selectors/ownership before disabling anything; unrelated workloads
   must not be interrupted.

See [full-group-rollout.md](full-group-rollout.md) for integration manifests,
RBAC/interpreters, webhook registration and component-specific settings.

## Build And Publish On MGMT

No container was built by the Windows code-review environment. Run on MGMT
after fetching the pushed integration branch. Stop on a dirty tracked tree or
failed fast-forward; do not reset local changes.

```bash
cd /root/hybridspot-validation/System
git status --short
git fetch origin restore-automation-20260928
git merge --ff-only origin/restore-automation-20260928
git log -1 --oneline

export IMAGE=docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.2
buildah build --build-arg COMPONENT=policy-manager -t "$IMAGE" . &&
buildah push "$IMAGE" "docker://$IMAGE"
```

Record the published digest and source commit with E1 evidence. Use the digest
in IMAGE for deployment when available. Do not reuse this tag for other code.

## Deploy Only The Changed Component

Run with no active migration/recovery operation. This preserves live args,
environment, credentials, admission and provisioner settings. The deployment
must contain exactly one container; the check stops otherwise.

```bash
(
  set -euo pipefail
  : "${MGMT_KUBECONFIG:?}" "${IMAGE:?}"
  BACKUP=$(mktemp -d /root/e1-policy-manager-v5.0.2.XXXXXX)
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    get deployment policy-manager -o json > "$BACKUP/deployment.json"
  CONTAINER=$(jq -er '.spec.template.spec.containers |
    if length == 1 then .[0].name else error("Inspect container selection") end' \
    "$BACKUP/deployment.json")
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    set image deployment/policy-manager "$CONTAINER=$IMAGE"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    rollout status deployment/policy-manager --timeout=180s
  printf 'Backup: %s\n' "$BACKUP"
)
```

This deployment fixes serial capacity requests; it does not upgrade or certify
older Stateful management, webhook, payload or provisioner images. Review those
against the prerequisites before E1. Do not claim an end-to-end pass from rollout.

## Acceptance Evidence

Record policy change time, each NP creation/Ready time, checkpoint identity and
exports, source fencing, target Prepared, placement release, and both ranks'
first resumed/increasing global steps. Keep checkpoint step, restored step and
last pre-move step distinct to expose rollback. Success requires both ranks to
continue from the same checkpoint, not merely Pod Running or step zero restart.
Report total service gap and its stages without deleting provisioning or manual
repair delays. Mark operator interventions explicitly. Preserve failed runs.

The current onpre1 GPU workers are AWS VMs joined to the onpre1 control plane
over VPN. Describe this as a hybrid-cluster testbed, not physical on-prem GPU
hardware or measured commercial WAN performance.
