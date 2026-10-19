# Spot event continuity deployment

r18audit01 was rejected after its source Spot event ID changed, before restore.
This is an incomplete restore audit, not evidence of numerical restore failure.
Keep the original operation/checkpoint identity and fail closed when source
identity is missing. Never manually replace event IDs or remove finalizers.

New emergency partial operations capture the source instance ID, signal type,
and notice timestamp in annotations. The NodeProvision name/UID and policy
ownership remain bound by the operation specification. A changed event ID must
still identify that instance, be at risk, use a supported signal, and have a
notice timestamp no earlier than the captured timestamp. An interruption cannot
be downgraded to a rebalance recommendation. Original event/checkpoint identities
remain unchanged; cleanup receives and checks the same binding.

Legacy operations without captured evidence retain strict event matching. This
does not retroactively repair r18audit01 or convert a planned operation to an
emergency operation. Completed and rejected replacement operations are not reopened.

## MGMT: build

Use the source revision containing the fix. Inspect local changes before pulling.
Do not overwrite local experiment configurations. No experiment should be active
during rollout. Run each stage only after the preceding stage succeeds.

```bash
cd /root/hybridspot-validation/System
git status --short
git rev-parse --short HEAD
go test ./internal/management
export IMAGE_REPO=docker.io/jeongseungjun/hybrid-spot-vm-system
export RELEASE="spot-event-$(git rev-parse --short HEAD)"
buildah login docker.io
for component in policy-manager checkpoint-coordinator spot-recovery-controller; do
  image="$IMAGE_REPO:${component}-${RELEASE}"
  buildah bud --arch amd64 --build-arg "COMPONENT=$component" \
    -f Dockerfile -t "$image" . || break
  buildah push "$image" "docker://$image" || break
done
```

Verify all three pushes succeeded. Never reuse a tag for different source code.
Training, FluidCR, Stateful Migration and Provisioner images are unchanged.

## MGMT: deploy

MGMT_KUBECONFIG must select the cluster hosting these Deployments, not the Karmada
API or AWS member cluster. Confirm their namespace and container names first.

```bash
kubectl --kubeconfig="${MGMT_KUBECONFIG:?}" -n hybridspot-system get deployments
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
  get deployment policy-manager checkpoint-coordinator spot-recovery-controller \
  -o yaml > "management-before-${RELEASE}.yaml"
for component in policy-manager checkpoint-coordinator spot-recovery-controller; do
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    set image "deployment/$component" \
    "manager=$IMAGE_REPO:${component}-${RELEASE}" || break
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
    rollout status "deployment/$component" --timeout=300s || break
done
```

If a rollout fails, do not start an experiment. Inspect logs and restore previous
image references from the backup as needed. These commands are instructions;
local source changes alone do not update the live cluster.

## Fresh validation

Confirm r18audit01 EC2 instances actually terminated; cleanup_requested is not
termination evidence. Preserve the archive. Follow restore-audit-guide-20261019.md
with a new run ID. Require both replacements and cleanup operations Completed,
restores Verified, and passing rank-local audits for both ranks.

Local tests do not prove live AWS recovery or full DDP numerical equivalence.
A real interruption can still prevent recovery for other reasons, such as a
missing checkpoint or loss of a survivor. Do not interpret this fix as a blanket
guarantee of recovery or reuse a failed audit as a successful cost measurement.
