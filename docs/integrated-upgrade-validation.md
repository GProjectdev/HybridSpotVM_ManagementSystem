# Integrated upgrade and validation

> Historical partial-replacement guide. For this release, use
> [Full-group rollout and acceptance](full-group-rollout.md) instead.
> Automatic policy and interruption recovery now use full-group restore,
> including rank 0. Do not run the partial activation commands below for them.

This coordinated upgrade does not authorize deleting existing NodeProvisions,
training Pods, checkpoint PVCs, or NFS artifacts. Build and test compatible
controller and FluidCR payload images before activating replacement.

The last supplied live snapshot placed `trainer-0` (rank 0) on the Spot worker.
That layout is not supported by this partial-replacement path: the rendezvous
owner cannot be moved while preserving the other ranks with the current runtime.
Do not enable the test on that layout. First verify the live placement; a
supported test needs a nonzero rank on the candidate Spot node and rank 0 on a
surviving node. Do not move or delete an existing training Pod to bypass this gate.

## Evidence before activation

Record repository commits, image digests, policy generation, workload UID,
Pod UIDs and ranks, node identities, checkpoint IDs and archive hashes.
Keep credentials outside Git. A completed periodic checkpoint proves durable
storage, not successful partial restoration.

## Partial replacement acceptance

- Replacement node must have verified GPU, serving TLS and artifact access.
- Use a fresh coordinated partial round, not an old periodic archive.
- Automatic replacement is off until `spec.replacement.enabled=true`.
- The automatic producer records target pod UID/rank/node and non-target
  survivor UID/rank/node baselines before checkpoint; survivor pause-lock proof
  is accepted only after the completed partial checkpoint reports it.
- Verify target source fencing after Stateful/member actuator proof, not from
  kubelet archive existence.
- Resume only the operation-owned checkpoint generation.
- Require advancing global steps across the full world after restore.
- Delete the old NodeProvision only after UID-bound restore verification.
- Unsupported rank-0 rendezvous migration must remain blocked.

Changing a payload image does not update existing training Pods: the injector
copies it at Pod initialization. Plan the payload rollout separately from the
partial-replacement test, then record new baseline Pod UIDs.

Local tests do not establish two-GPU native restore correctness. A separate
AWS integration run is required before enabling automatic replacement.

## Optional economic fallback

TrainingPolicy supports `spec.policy.economics` with `enabled` (default false),
`lossCostPerEviction`, `observedAt`, and `maxAgeSeconds` (default 600).
Loss and the risk feed's `onDemandPricePerHour` must use the same currency.
The loss is per individual VM eviction, not the combined loss of an entire job.
The operator must supply a justified observation; no synthetic zero is inferred
from missing recovery measurements.

The conservative fallback condition is exactly
`OD_hourly_price * horizon_hours < (1-exp(-lambda*horizon_hours)) * loss`.
For the current homogeneous pool, that condition applies to every Spot slot,
so a satisfied condition requests all On-Demand. Otherwise the stability rule
chooses the minimum On-Demand count. This is not a heterogeneous VM portfolio
optimizer or a job-level expected-loss optimizer. Stale/missing inputs preserve
the stability-only decision and report `costEvaluated=false`.

A changed desired count is not permission to mutate an EC2 instance's market
type. Existing nodes still require the verified replacement workflow.

## Activation smoke commands

Run these only after the upgraded System, Stateful, FluidCR, and member payloads
are installed. Run on MGMT, using the Karmada API kubeconfig, not the MGMT host
cluster kubeconfig, for the commands below. Enabling
replacement can create a paid On-Demand VM, and after all proofs exist the
controllers can gracefully delete the old target Pod and delete the old
NodeProvision. Do not run this against live training unless that side effect is
intended.

```bash
set -euo pipefail
K=${KARMADA_KUBECONFIG:?set KARMADA_KUBECONFIG to the Karmada API kubeconfig}
NS=fluidcr-demo
POLICY=trainer-auto-06ea17a8
RISK="$(kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" -o jsonpath='{.spec.riskProfileRef.name}')"

# Precheck before opt-in. If the candidate Spot target is rank 0, stop; System
# must reject rank-0 partial replacement before capacity or checkpoint creation.
kubectl --kubeconfig="$K" -n "$NS" get trainingruntime \
  "$(kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" -o jsonpath='{.spec.runtimeRef.name}')" \
  -o jsonpath='{range .status.clusters[*].status.pods[*]}{.rank}{" "}{.name}{" "}{.uid}{" "}{.nodeName}{"\n"}{end}'

# Controlled static risk input through spec only. This clears mutually-exclusive
# endpoint/trace/paperEstimator modes and lets the collector publish status.
kubectl --kubeconfig="$K" -n "$NS" patch spotriskprofile "$RISK" --type=merge \
  -p '{"spec":{"endpoint":null,"trace":null,"paperEstimator":null,"staticLambdaPerHour":0.3}}'
kubectl --kubeconfig="$K" -n "$NS" wait spotriskprofile "$RISK" \
  --for=jsonpath='{.status.lambdaPerHour}'=0.3 --timeout=90s
kubectl --kubeconfig="$K" -n "$NS" get spotriskprofile "$RISK" -o json |
  jq -e '.status.ready == true and .status.observedGeneration == .metadata.generation'

kubectl --kubeconfig="$K" -n "$NS" patch trainingpolicy "$POLICY" --type=merge \
  -p '{"spec":{"replacement":{"enabled":true}}}'
```

Expected read-only evidence:

```bash
kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" \
  -o jsonpath='{.status.policy.reason}{" "}{.status.policy.replacementOperation}{" "}{.status.policy.replacementNodeProvision}{"\n"}'

OP="$(kubectl --kubeconfig="$K" -n "$NS" get trainingpolicy "$POLICY" -o jsonpath='{.status.policy.replacementOperation}')"
kubectl --kubeconfig="$K" -n "$NS" get spotreplacement "$OP" \
  -o jsonpath='{.status.phase}{" "}{.spec.oldNodeProvisionRef.uid}{" "}{.spec.partialCheckpoint.targetRanks}{" "}{.spec.partialRestore.preservedSurvivors[*].podUID}{"\n"}'

# Rank 0 remains fail-closed before any replacement capacity is created.
kubectl --kubeconfig="$K" -n "$NS" get spotreplacement "$OP" \
  -o jsonpath='{.status.phase}{" "}{.status.message}{"\n"}'

MIGRATION="$OP-partial-checkpoint"
kubectl --kubeconfig="$K" -n "$NS" get fluidcrmigration "$MIGRATION" \
  -o jsonpath='{.spec.resume}{" "}{.spec.partialCheckpoint.targetRanks}{"\n"}'
kubectl --kubeconfig="$K" -n "$NS" get fluidcrmigration "$MIGRATION" \
  -o jsonpath='{range .status.clusters[?(@.clusterName=="aws")].status.pods[*]}{.rank}{" "}{.phase}{" "}{.podUID}{" "}{.checkpointID}{"\n"}{range .checkpointFiles[*]}{.containerName}{" "}{.filePath}{" "}{.sha256}{" "}{.durableRef}{"\n"}{end}{end}'

RR="$OP-restore"
kubectl --kubeconfig="$K" -n "$NS" get restorerequest "$RR" \
  -o jsonpath='{.spec.partialRestore.preservedSurvivors[*].pauseLockPath}{" "}{.status.verification.sourceFence.evidenceID}{" "}{.status.verification.partialRestore.targetRanks[*].archiveEvidenceID}{"\n"}'
```

If any command needs a manual `kubectl delete` to make progress, stop the run;
the replacement path is not safely actuating the coordinated contract.

## Build on the MGMT host

Use the cloned source directories on MGMT. These commands build and publish
images; they do not install controllers or mutate running training Pods.
Choose a new immutable tag, not one already used by existing Pods.

```bash
set -euo pipefail
export ROOT=/root/hybridspot-validation
export SYSTEM="$ROOT/System" ST="$ROOT/stateful"
export PROVISIONER="$ROOT/provisioner" FLUIDCR_ROOT="$ROOT/fluidcr"
export TAG=integration-20260927-01
export REGISTRY=docker.io/jeongseungjun
for repo in "$SYSTEM" "$ST" "$PROVISIONER" "$FLUIDCR_ROOT"; do
  git -C "$repo" pull --ff-only
done
buildah login docker.io

for component in vm-spot-risk-collector policy-manager checkpoint-coordinator spot-recovery-controller training-runtime-collector spot-watcher; do
  image="$REGISTRY/hybrid-spot-vm-system:${component}_${TAG}"
  buildah build --build-arg COMPONENT="$component" -t "$image" "$SYSTEM"
  buildah push "$image" "docker://$image"
done

export STATEFUL_IMAGE="$REGISTRY/stateful-migration-operator:$TAG"
export PROVISIONER_IMAGE="$REGISTRY/my-publiccloudvm-provisioner:$TAG"
export INJECTOR_IMAGE="$REGISTRY/myfluidcr-operator:webhook_$TAG"
export PAYLOAD_BASE_IMAGE="$REGISTRY/myfluidcr-operator:payload-base_$TAG"
export PAYLOAD_IMAGE="$REGISTRY/myfluidcr-operator:payload-stateful_$TAG"
buildah build -t "$STATEFUL_IMAGE" "$ST"
buildah push "$STATEFUL_IMAGE" "docker://$STATEFUL_IMAGE"
buildah build -t "$PROVISIONER_IMAGE" "$PROVISIONER"
buildah push "$PROVISIONER_IMAGE" "docker://$PROVISIONER_IMAGE"
buildah build -f "$FLUIDCR_ROOT/Dockerfile.webhook" -t "$INJECTOR_IMAGE" "$FLUIDCR_ROOT"
buildah push "$INJECTOR_IMAGE" "docker://$INJECTOR_IMAGE"
buildah build -f "$FLUIDCR_ROOT/Dockerfile.payload" -t "$PAYLOAD_BASE_IMAGE" "$FLUIDCR_ROOT"
buildah push "$PAYLOAD_BASE_IMAGE" "docker://$PAYLOAD_BASE_IMAGE"
buildah build -f "$ST/Dockerfile.payload-overlay" --build-arg FLUIDCR_PAYLOAD_IMAGE="$PAYLOAD_BASE_IMAGE" -t "$PAYLOAD_IMAGE" "$ST"
buildah push "$PAYLOAD_IMAGE" "docker://$PAYLOAD_IMAGE"
```

Buildah must support the Dockerfile syntax in each repository. Run on an amd64
Linux build host for the current amd64 workers. Do not assume an unused
TARGETARCH build argument performs cross-compilation.

## Upgrade order

1. Save baseline resource YAML and image digests in a protected evidence folder.
2. Install updated System and Stateful CRDs in Karmada and the AWS member before
   any producer writes new fields. Old CRDs can prune partial-restore fields.
3. Install updated least-privilege Karmada and member RBAC, including the separate
   Stateful checkpoint RBAC. Preserve existing kubeconfig Secrets.
4. Upgrade NodeProvisioner in the AWS member. Configure serving-CA trust and
   explicit CSR approval opt-in as documented in that repository. Never publish
   the cluster CA private key in a ConfigMap or copy it to workers.
5. Upgrade Stateful management in MGMT, and checkpoint/member/artifact controllers
   in AWS. `config/member` does not include `config/checkpoint`; render and apply
   both. Keep only one controller responsible for FluidCRMigration execution.
6. Upgrade the FluidCR webhook to inject PAYLOAD_IMAGE (the Stateful overlay).
   Schedule a separate workload rollout to load the new payload; record the new
   baseline before attempting partial replacement.
7. Upgrade System component Deployments in MGMT and member collectors/watchers.
   Keep automatic replacement disabled until all prerequisites are verified.

Do not delete existing NodeProvisions, NFS/PVCs, or training Pods as an upgrade
shortcut. An image rollout is not evidence of successful GPU restore. Activation
and acceptance checks are in the component-specific partial-restore documents.
