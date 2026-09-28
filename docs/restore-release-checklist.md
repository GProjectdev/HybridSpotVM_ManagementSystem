# Restore integration release checklist

## Release boundary

This is an integration snapshot, NOT acceptance of all recovery scenarios.
See [the scenario matrix](restore-automation-validation.md). Rank-0/source-loss
recovery and direct cross-cluster placement changes still require orchestration
integration; image replacement alone does not implement them.

No Kubernetes/EC2 changes are performed by local development verification.
Preserve in-flight operations and checkpoints during upgrades.

## Source and artifacts

Record exact source commits and registry digests before deployment.

The integration branch in all four changed repositories is
`restore-automation-20260928`; stable branches are not changed by this push.
In each clean Linux checkout, fetch and select that branch before building:

```bash
git fetch origin
git switch --track origin/restore-automation-20260928
git rev-parse HEAD
```

If a local branch with that name already exists, switch to it and use
`git pull --ff-only`. Preserve local modifications; do not reset them to make
the checkout succeed. Record all four commits alongside image digests.

| Component | Repository | Build |
| --- | --- | --- |
| Policy, checkpoint, recovery and collectors | HybridSpotVM_ManagementSystem | Dockerfile, COMPONENT argument |
| Stateful controllers | Stateful-Migration-Operator-with-PV | Dockerfile |
| Node provisioning | PublicCloud-VM-Provisioner_test | Dockerfile |
| Injected Python payload | My_FluidCR | Dockerfile.payload |
| Runtime .deb | custom-crio + provisioner package builder | Provisioner runtime rollout guide |

The runtime .deb is a separate versioned artifact installed on new workers.
Changing the provisioner Deployment does not upgrade existing Ready workers.
Follow the provisioner's `docs/restore-runtime-rollout.md` for its reviewed
CRI-O baseline, package digest, NetConfig and GPU qualification.

## Buildah on Linux

Set the following paths to the local checkouts and repositories to the registry
names. Use a new immutable TAG. This builds/pushes images, but does not deploy.

```bash
set -euo pipefail
: "${SYSTEM_SRC:?}" "${STATEFUL_SRC:?}" "${PROVISIONER_SRC:?}" "${FLUIDCR_SRC:?}"
: "${SYSTEM_REPO:?}" "${STATEFUL_REPO:?}" "${PROVISIONER_REPO:?}" "${FLUIDCR_REPO:?}" "${TAG:?}"
for COMPONENT in policy-manager checkpoint-coordinator spot-recovery-controller \
                 training-runtime-collector spot-watcher vm-spot-risk-collector; do
  buildah bud -f "$SYSTEM_SRC/Dockerfile" --build-arg COMPONENT="$COMPONENT" \
    -t "$SYSTEM_REPO:$COMPONENT-$TAG" "$SYSTEM_SRC"
  buildah push "$SYSTEM_REPO:$COMPONENT-$TAG"
done
buildah bud -f "$STATEFUL_SRC/Dockerfile" -t "$STATEFUL_REPO:$TAG" "$STATEFUL_SRC"
buildah push "$STATEFUL_REPO:$TAG"
buildah bud -f "$PROVISIONER_SRC/Dockerfile" -t "$PROVISIONER_REPO:$TAG" "$PROVISIONER_SRC"
buildah push "$PROVISIONER_REPO:$TAG"
buildah bud -f "$FLUIDCR_SRC/Dockerfile.payload" \
  -t "$FLUIDCR_REPO:payload-$TAG" "$FLUIDCR_SRC"
buildah push "$FLUIDCR_REPO:payload-$TAG"
```

Match the payload Python version to the training image using PYTHON_VERSION
when needed. Registry authentication uses the existing Buildah login.

## Deployment order

1. Finish or explicitly suspend existing operations. Back up CRs, image digests,
   webhook arguments, replica counts and runtime NetConfig.
2. Apply System CRDs on Karmada. Apply NodeProvision/NetConfig CRDs where those
   resources are stored, including AWS and Karmada when propagated.
3. Apply Stateful CRDs/RBAC on their existing management/member clusters.
   Preserve each Deployment's mode, arguments, credentials and volume mounts.
4. Update provisioner and Stateful images, then the FluidCR webhook's existing
   --payload-image argument. Preserve its port/certificate arguments. Already
   running Pods retain their old injected payload: update only at a controlled
   workload restart with recoverable checkpoints preserved.
5. Publish the new runtime .deb at a stable HTTPS URL. Update NetConfig package
   digest and commit metadata together; qualify one isolated new GPU worker.
   Never add capability labels to bypass failed verification.
6. Update System controllers/collectors. Restore saved replica counts only when
   prerequisites are ready. Check rollout status and controller errors.

## Acceptance and rollback

- Test periodic checkpoint/export first: every rank, one round, hash-matching
  durable archives, resumed training and increasing globalStep.
- Test a supported nonzero-rank planned market replacement in isolation.
  Only Verified permits cleanup of old capacity.
- Notice handling with a reachable source is not proof of recovery from an
  already terminated instance.
- A full-group prepare endpoint does not authorize fencing by itself. The
  orchestration caller must supply validated operation-bound fence evidence.
- Stop on missing UID/archive, native restore failure or absent progress.
  Preserve evidence; never patch success status or remove safety finalizers.
- Check CR compatibility before rolling back image digests. Runtime rollback
  requires isolated-node maintenance, not a live binary overwrite.

Cross-cluster, rank-0 and lost-source acceptance remain blocked until the
scenario matrix's missing orchestration stages are implemented and tested.
