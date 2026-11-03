# E1 split Work release v5.0.5

## Contract

Keep upstream Karmada unchanged. The earlier custom Karmada patch is withdrawn.
PV Migration now emits separate singleton PV and PVC Works for shared claims,
checks reciprocal member UIDs and NFS binding, and persists both UIDs before
detaching Works. System verifies both receipts before releasing placement.
No manual status completion or volume binding bypass is allowed.

This keeps shared PVCs; it does not switch to volumeClaimTemplates. Template
claims retain the existing PV-only Work path. Old combined-Work plans are not
converted automatically. Use a new RUN after cleanup of the stopped experiment.

All commands below run on MGMT. Keep ONPRE1 workers, NFS and WireGuard running.
Existing MGMT_KUBECONFIG, ONPRE1_KUBECONFIG, AWS_KUBECONFIG and
KARMADA_KUBECONFIG exports are required. Back up current deployment manifests
and CRDs before upgrading. No Karmada rollout or RIC reinstallation is required.
If the withdrawn custom Karmada image was already installed, do not start the
experiment until the original deployment has been restored from its backup.

## Fetch and build with Buildah

Run each block only after the previous one succeeds.

```bash
cd /root/hybridspot-validation/pv
git fetch origin main
git merge --ff-only origin/main
buildah bud -t docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.5 .
buildah push docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.5 docker://docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.5
```

```bash
cd /root/hybridspot-validation/System
git fetch origin restore-automation-20260928
git merge --ff-only origin/restore-automation-20260928
buildah bud --build-arg COMPONENT=policy-manager -t docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.5 .
buildah push docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.5 docker://docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.5
buildah bud --build-arg COMPONENT=placement-webhook -t docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-v5.0.5 .
buildah push docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-v5.0.5 docker://docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-v5.0.5
```

## Schema first

```bash
cd /root/hybridspot-validation/pv
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" apply -f config/crd/bases/migration.dcnlab.com_pvmigrations.yaml
kubectl --kubeconfig="${ONPRE1_KUBECONFIG:?}" apply -f config/crd/bases/migration.dcnlab.com_pvmigrations.yaml
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" apply -f config/crd/bases/migration.dcnlab.com_pvmigrations.yaml
```

Do not run `apply -k config/karmada/ric`: the existing persistentvolume-status
and persistentvolumeclaim interpreters already supply the required fields.

## Controller images

The container names below are manager. Verify the current deployment container
names before applying if the installation differs.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n pv-migration-system set image deployment/pv-migration-management manager=docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.5
kubectl --kubeconfig="${ONPRE1_KUBECONFIG:?}" -n pv-migration-system set image deployment/pv-migration-member manager=docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.5
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n pv-migration-system set image deployment/pv-migration-member manager=docker.io/jeongseungjun/multicluster-pvmigration-system:v5.0.5
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/policy-manager manager=docker.io/jeongseungjun/hybrid-spot-vm-system:policy-manager-v5.0.5
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/placement-webhook manager=docker.io/jeongseungjun/hybrid-spot-vm-system:placement-webhook-v5.0.5
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n pv-migration-system rollout status deployment/pv-migration-management --timeout=300s
kubectl --kubeconfig="$ONPRE1_KUBECONFIG" -n pv-migration-system rollout status deployment/pv-migration-member --timeout=300s
kubectl --kubeconfig="$AWS_KUBECONFIG" -n pv-migration-system rollout status deployment/pv-migration-member --timeout=300s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/policy-manager --timeout=300s
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/placement-webhook --timeout=300s
```

Do not start a RUN if any rollout fails. Training image, stateful migration
images, GPU runtime and Karmada images are unchanged by this release.

## New experiment

Confirm the stopped run has no remaining training Pods or active target VM
requests. Preserve its downloaded evidence and existing NFS data.

```bash
cd /root/hybridspot-validation/System
export RUN="e1s-$(date -u +%m%d%H%M%S)"
export EVIDENCE="$PWD/evidence/e1/$RUN"
python3 evaluation/e1_experiment.py render --policy-source evaluation/policy-source.json --run-id "$RUN" --out "$EVIDENCE"
jq '.spec.policy' "$EVIDENCE/policy.unbound.json"
printf 'RUN=%s\nEVIDENCE=%s\n' "$RUN" "$EVIDENCE"
python3 -u evaluation/e1_experiment.py execute --evidence "$EVIDENCE" --confirm "$RUN"
```

Expected policy: alpha 0.05, minOnDemand 0, fixedOnDemand 0. Source remains two
existing ONPRE1 GPU workers; target is two new AWS Spot workers.
In another terminal, set EVIDENCE to the printed absolute directory and run:

```bash
tail -n 30 -F "${EVIDENCE:?}/timeline.jsonl"
kubectl --kubeconfig="${KARMADA_KUBECONFIG:?}" -n fluidcr-realign-121040 get pvmigrations -o json | jq '.items[] | {name:.metadata.name,phase:.status.phase,works:.status.works}'
kubectl --kubeconfig="${AWS_KUBECONFIG:?}" -n fluidcr-realign-121040 get pods,nodeprovisions
```

During migration each standalone claim has two Works; after successful
detachment the Works disappear but status.works retains matching pvUID/pvcUID,
applied=true and detached=true. Completed PVMigration alone is not E1 success:
both restored ranks must resume training and the runner must record its result.
Download the final archive named by DOWNLOAD. It excludes private diagnostics;
inspect those locally and redact credentials before sharing if failures occur.

## Verification boundary

Local Go regression tests exercise split reflection, reciprocal binding,
cleanup and restore-gate rejection. Live Karmada reflection, GPU restoration,
Buildah image publication and the full E1 run still require the MGMT environment.
