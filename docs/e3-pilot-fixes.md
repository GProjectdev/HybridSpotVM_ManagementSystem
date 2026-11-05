# E3 pilot fixes and rollout

The first pilot verified replacement and resumed training, but exposed report
event misclassification and a cost freshness budget shorter than its interval.
This update changes Python reporting and the policy-manager/checkpoint-coordinator.
Karmada, FluidCR, StatefulMigration, PV Migration and training images stay unchanged.
No CRD update is needed. Live rollout and a new pilot remain required.

## Freshness rationale

Cost ingestion and policy evaluation share a bounded age budget derived from
the configured maximum checkpoint interval plus ten minutes for completion and
reconciliation. Without a positive maximum, retain the ten-minute fallback.
For E3 maxIntervalSeconds=3600, this is 4200 seconds. Old timestamps are not
rewritten. Expired/future costs and invalid archive evidence remain rejected.
This allowance is an operational rule, not proof that costs remain statistically
stable for that duration. Existing ownership and generation checks stay intact.
Do not change the window separately for each arm after observing outcomes.

## Update

Keep local configuration and original evidence. Do not approve the main plan yet.

```bash
cd /root/hybridspot-validation/System
git fetch origin
git merge --ff-only origin/restore-automation-20260928
python3 -m unittest discover -s evaluation -p 'test_e3*.py'
```

## Build and deploy

No experiment may be active. Verify MGMT_KUBECONFIG targets the management cluster.
Buildah needs registry credentials. Both pushes finish before rollout begins.
Backups preserve the old image references. Container name manager is checked.
Use commit-specific tags and deploy the resulting digests, not a mutable tag.

```bash
cd /root/hybridspot-validation/System
(
  set -euo pipefail
  : "${MGMT_KUBECONFIG:?}"
  TAG="e3-fix-$(git rev-parse --short=12 HEAD)"
  REPOSITORY=docker.io/jeongseungjun/hybrid-spot-vm-system
  BACKUP=$(mktemp -d /root/e3-controller-rollout.XXXXXX)
  printf 'Backup: %s\n' "$BACKUP"
  for COMPONENT in policy-manager checkpoint-coordinator; do
    kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system get deployment "$COMPONENT" -o json > "$BACKUP/$COMPONENT.json"
    jq -e '[.spec.template.spec.containers[] | select(.name=="manager")] | length==1' "$BACKUP/$COMPONENT.json" >/dev/null
    IMAGE="$REPOSITORY:$COMPONENT-$TAG"
    buildah bud --arch amd64 --build-arg "COMPONENT=$COMPONENT" -t "$IMAGE" .
    buildah push --digestfile "$BACKUP/$COMPONENT.digest" "$IMAGE" "docker://$IMAGE"
    DIGEST=$(cat "$BACKUP/$COMPONENT.digest")
    [[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]
  done
  for COMPONENT in policy-manager checkpoint-coordinator; do
    DIGEST=$(cat "$BACKUP/$COMPONENT.digest")
    kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image "deployment/$COMPONENT" "manager=$REPOSITORY@$DIGEST"
    kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status "deployment/$COMPONENT" --timeout=300s
  done
)
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system get deployments policy-manager checkpoint-coordinator -o json |
jq '.items[] | {name:.metadata.name,containers:.spec.template.spec.containers,desired:.spec.replicas,updated:.status.updatedReplicas,ready:.status.readyReplicas,available:.status.availableReplicas}'
```

If either rollout fails, do not start experiments. Restore both manager image
references from the saved JSON, then verify both rollouts. Do not apply the full
old Deployment JSON over unrelated configuration changes.

## Revalidate

Run a NEW 30-minute C pilot using the guide in e3-controlled-comparison.md.
Keep --pilot; no FIS. Preserve the old pilot and keep approved_for_measurement=false.
Verify analytic selection remains beyond ten minutes while the measured timestamp
is inside the new budget, then verify replacement Completed, restore Verified,
and both ranks progressing. A short pilot does not verify an entire 70-minute
expiry or subsequent measurement renewal; unit tests cover expiry boundaries.
Record the new pilot and image digests before approving/freezing the main plan.
Retain failures. Clean up all generated VMs and verify actual EC2 termination.

## Rebuild the old report safely

The presentation archive is filtered. Regenerate from the original raw server
evidence directory, not by pretending the presentation archive is raw evidence.
Keep the downloaded original archive and back up report outputs before replacing.

```bash
export EVIDENCE=/root/hybridspot-validation/System/evidence/e3/e3p-1104072306
(
  set -euo pipefail
  BACKUP=$(mktemp -d "$EVIDENCE/report-before-fix.XXXXXX")
  for FILE in e3-summary.json e3-summary.md e3-evidence.tar.gz e3-manifest.json; do
    if [ -f "$EVIDENCE/$FILE" ]; then cp -- "$EVIDENCE/$FILE" "$BACKUP/$FILE"; fi
  done
  python3 evaluation/e3_report.py --evidence "$EVIDENCE"
)
```

Boundary sampling and missing-cost warnings remain valid. Actual malformed
progress must still warn; no training counter is rewritten by this correction.

