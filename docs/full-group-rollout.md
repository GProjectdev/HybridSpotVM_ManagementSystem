# Full-group Rollout And Acceptance

This is the current automation guide for branch `restore-automation-20260928`.
The older partial replacement activation instructions are not applicable to
automatic policy or interruption recovery. Manual partial operations remain
supported separately; finish them before upgrading. Do not run partial and
group recovery concurrently for one world.

## Scope And Prerequisites

- A fixed-size, single-container DDP StatefulSet with one writable shared NFS
  PVC mounted at `/checkpoint`, without subPath. All ranks restore one completed
  round, including rank 0. Healthy ranks are also fenced and recreated.
- Cross-cluster target PVC must already be Bound to the SAME NFS server/path
  as the source. Equal claim names or two independently provisioned directories
  are not sufficient. This path does not transfer arbitrary PVC data.
- Every archive must have an exported durableRef and SHA256. Use a fresh round
  created by the upgraded payload with full-world metadata. Historical archive
  Pod UIDs are separate from the collector's CURRENT source Pod UID snapshot.
- Target workers must pass runtime/GPU/serving-TLS qualification and expose
  restore-from-file capability. Use the provisioner's
  `docs/restore-runtime-rollout.md` package/bootstrap steps, with custom CRI-O
  baseline `6d082c56b0212769f58a6acd80ce2879c3d998f0` or a reviewed descendant.
  Its older partial-policy acceptance section is superseded by this guide.
- Install Stateful member admission on BOTH source and target. It is the
  recreation barrier; do not remove it, bypass it, or manually force status.
- Kubernetes RBAC must restrict Plan/status/fence-receipt writes to controllers.
  The receipt is a control-plane trust boundary, not a cryptographic signature.
- Do not upgrade with an active partial operation. A stale source snapshot is
  refused, not silently replaced by historical checkpoint UIDs. Preserve fence
  evidence when diagnosing a failed immutable request.

## Build Images On MGMT

Use clean checkouts of the integration branch in all four repositories. Fetch
and switch that branch explicitly; `git pull` on main does not fetch this release.
Record the four commit IDs and use a new tag for each build. These commands do
not perform a live workload rollout.

```bash
set -euo pipefail
export ROOT=/root/hybridspot-validation
export SYSTEM="$ROOT/System" ST="$ROOT/stateful"
export PROVISIONER="$ROOT/provisioner" FLUIDCR_ROOT="$ROOT/fluidcr"
export TAG=group-restore-20260928-01
export REGISTRY=docker.io/jeongseungjun
buildah login docker.io
for component in vm-spot-risk-collector policy-manager checkpoint-coordinator spot-recovery-controller training-runtime-collector spot-watcher placement-webhook; do
  image="$REGISTRY/hybrid-spot-vm-system:${component}_${TAG}"
  buildah build --build-arg COMPONENT="$component" -t "$image" "$SYSTEM"
  buildah push "$image" "docker://$image"
done
export STATEFUL_IMAGE="$REGISTRY/stateful-migration-operator:$TAG"
export PROVISIONER_IMAGE="$REGISTRY/my-publiccloudvm-provisioner:$TAG"
export INJECTOR_IMAGE="$REGISTRY/myfluidcr-operator:webhook_$TAG"
export PAYLOAD_BASE_IMAGE="$REGISTRY/myfluidcr-operator:payload-base_$TAG"
export PAYLOAD_IMAGE="$REGISTRY/myfluidcr-operator:payload-stateful_$TAG"
export GROUP_CONTROL_IMAGE="$REGISTRY/myfluidcr-operator:group-control_$TAG"
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
buildah build -f "$FLUIDCR_ROOT/Dockerfile.group-control" -t "$GROUP_CONTROL_IMAGE" "$FLUIDCR_ROOT"
buildah push "$GROUP_CONTROL_IMAGE" "docker://$GROUP_CONTROL_IMAGE"
```

Pin published digests in rendered deployment manifests. Group control runs as
an independent Job image; the training payload image is not a substitute.

## Upgrade Order

Run this read-only gate on MGMT before upgrading or re-enabling producers.
Failed legacy partial operations count as active. If it exits nonzero, stop and
inspect the operation's source/target and receipts; do not delete it to bypass
the gate. No automatic ownership handoff from a legacy partial is implemented.

```bash
(
  set -euo pipefail
  : "${KARMADA_KUBECONFIG:?required}"
  kubectl --kubeconfig="$KARMADA_KUBECONFIG" get spotreplacements -A -o json |
    jq -e '[.items[] | select(.status.phase != "Completed" and .status.phase != "Rejected") |
      {namespace:.metadata.namespace,name:.metadata.name,phase:.status.phase}] |
      if length == 0 then true else error("Unfinished legacy partial operations: " + tojson) end'
)
```

1. Back up live resource YAML, image digests and controller replica counts.
   Disable replacement and stop policy/checkpoint/recovery producers only after
   current operations finish. Keep member/artifact actuators available.
2. Apply System, Stateful and NodeProvision CRDs to Karmada and applicable
   members. Apply updated RBAC and resource interpreters, including the
   NodeProvision memberUID reflection and flat RestorePlan cluster status.
3. Upgrade provisioner and configure the reviewed runtime package URL/digest
   in NetConfig. Existing Ready nodes are not automatically reinstalled.
   Verify Karmada NP.status.memberUID equals the AWS object's metadata.uid.
4. Upgrade Stateful management plus member, checkpoint and artifact components.
   Render each member's cluster name and `--group-control-image` digest.
   Preserve existing credentials, admission TLS and artifact-store settings.
5. Upgrade the injector with the Stateful overlay payload. New payload content
   is copied at Pod initialization, not by changing a running Pod's image.
   Complete a planned workload rollout before recording validation baselines.
6. Upgrade all System controllers and member runtime collectors/watchers.
   Install the placement webhook below BEFORE changing PropagationPolicy.
7. Resume producers to their recorded replica counts. Confirm a fresh complete
   periodic checkpoint and current full-world runtime identity snapshot.
   Enable replacement only when the target runtime and NFS prerequisites pass.

Use the repository manifests as templates, not blind overwrites of local
deployment configuration. In Stateful, `config/checkpoint` is separate from
`config/member`; only one controller should execute FluidCRMigration.

## Placement Webhook TLS And Registration

MGMT hosts `placement-webhook` on NodePort 30443. The Karmada API server must
reach it. Choose a DNS name resolving to MGMT from Karmada; restrict ingress to
the control-plane network. Do not expose a plaintext or untrusted endpoint.

```bash
: "${PLACEMENT_HOST:?DNS name reachable from Karmada API server}"
TLS_DIR="$(mktemp -d)"
chmod 700 "$TLS_DIR"
openssl req -x509 -newkey rsa:3072 -nodes -days 90 \
  -keyout "$TLS_DIR/tls.key" -out "$TLS_DIR/tls.crt" \
  -subj "/CN=$PLACEMENT_HOST" -addext "subjectAltName=DNS:$PLACEMENT_HOST"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system \
  create secret tls placement-webhook-tls \
  --cert="$TLS_DIR/tls.crt" --key="$TLS_DIR/tls.key" \
  --dry-run=client -o yaml | kubectl --kubeconfig="$MGMT_KUBECONFIG" apply -f -
```

Render `config/management/placement-webhook.yaml` with the published image and
existing `hybridspot-karmada-kubeconfig` Secret. Set
`PLACEMENT_RELEASE_USERNAME` to the actual Karmada identity used by policy-manager,
not the local Pod's service account name. The example defaults to
`system:serviceaccount:hybridspot-system:hybridspot-management`.
Apply the rendered Deployment/Service and wait for rollout. Confirm TLS from
the Karmada control-plane network using this CA before registering admission.
Render `config/karmada/placement-webhook.yaml`: replace
`__MGMT_REACHABLE_HOST__` with PLACEMENT_HOST and `__CA_BUNDLE__` with base64 of
tls.crt (single line). Apply that file to KARMADA, not MGMT. Keep failurePolicy
Fail. Rotate the certificate and CA before expiry; never commit TLS private keys.

## Expected Workflows

### Periodic Checkpoint

The coordinator creates FluidCRMigration, ranks coordinate one round, kubelet
archives containers, artifact agents export and hash archives, and the workload
resumes. Completed without every durableRef is not recovery-ready. During group
intent, the coordinator finishes an in-flight checkpoint then acknowledges
quiescence; it does not race new rounds against restore preparation.

### Case 1: On-prem To AWS

Change the managed workload's PropagationPolicy to AWS after the baseline.
Admission holds ResourceBinding's old source assignment and records pending
placement. System creates OnDemand capacity and the target runtime, then a
UID-bound group RestoreRequest. Only RestorePlan propagates to both clusters.
Source member fences the full current world. Target verifies identical NFS
backing and runs prepare even before a target StatefulSet exists. A fresh,
request-owned Plan with target Prepared releases placement. Admission then
binds recreated target ranks to their exact archive/node mapping.

### Case 2: Policy Market Change

Enable `spec.replacement.enabled=true`, change SpotRiskProfile SPEC input, and
wait for fresh risk status. The desired market mix must actually change.
System creates replacement NodeProvision capacity and a full-group request.
It restores every rank from one completed round; it does not preserve the
survivors' newer in-memory training progress. Rank 0 may move. The old NP is
deleted only after request-bound verification, never just Pod Running.

### Case 3: Spot Interruption

The watcher publishes a notice bound to the exact NP/instance. The automatic
path also uses full-group recovery. A notice arriving during the same policy
replacement is attached to that operation, not a competing partial restore.
If the source is unreachable/already absent, exact member NP UID, instance ID,
operation UID and current-generation provider Fenced evidence are required.
The last exported full round is the recovery point; the notice window is not
assumed long enough to create a fresh checkpoint. Missing recoverable artifacts
or fencing proof blocks recovery rather than permitting two active worlds.

### Shared Completion

After all bound target ranks become Ready, a generation-bound resume Job runs.
Two fresh complete runtime samples after resume must show increasing steps.
RestoreRequest then becomes Verified. Target enrollment is retired by a
management-owned marker. Cross-source admission remains blocked until the old
Plan is retired AFTER verified target recovery and source placement removal.
Do not reuse the old source world or remove its barrier before that cleanup.
Do not leave an explicit old restore-plan label on the StatefulSet template.

## Capture Acceptance Evidence

```bash
export NS=fluidcr-demo
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get trainingpolicies,trainingruntimes,restorerequests
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get resourcebindings -o yaml
kubectl --kubeconfig="$AWS_KUBECONFIG" -n "$NS" get nodeprovisions,restoreplans,pods,jobs -o wide
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n "$NS" get restorerequest "$RR" -o json |
  jq '{uid:.metadata.uid,generation:.metadata.generation,spec:.spec,status:.status}'
```

For each case save source/current/archive UIDs, full SourceGone receipts,
prepare/resume Job UIDs and logs, current Plan generations, RR verification,
target container IDs with CRI-O Restored-container logs, and two post-resume
runtime samples from every rank. Verify no source recreation during fencing,
no target admission before Prepared, no cold-start fallback, and no early old
NP deletion. Do not accept a lone Running Pod or manually edited status.

Local Go tests/vet validate controller contracts, not GPU/CRIU compatibility,
network reachability, image publication or live cloud end-to-end recovery.
Run the three cases separately on disposable validation workloads before
production activation; real interruption testing can terminate paid instances.
