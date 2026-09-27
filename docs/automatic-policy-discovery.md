# Automatic StatefulSet Policy Discovery

This supersedes manual TrainingPolicy creation in the capacity validation steps.
Users still own workload placement. No per-StatefulSet opt-in annotation is used.
The policy-manager watches all Karmada StatefulSets and ResourceBindings.
No Member kubeconfig or direct Member API access is added.

## Contract and Boundaries

- Discovery waits for one UID-matching StatefulSet RB with one target containing
  the entire positive replica count. Unscheduled, split, duplicated, partial,
  stale-UID and zero-replica bindings do not allocate nodes.
- One replica is interpreted as one dedicated worker VM, not general bin packing.
  This is a training-system deployment assumption, not Kubernetes behavior.
- The global Karmada ConfigMap hybridspot-system/automatic-policy-defaults stores
  infrastructure and constraints, not a manually selected Spot/OD ratio.
  Setting it enables automatic enrollment of eligible workloads cluster-wide.
  Review existing workloads BEFORE creating this ConfigMap: real costs can result.
- Defaults are snapshotted at policy creation. Editing the ConfigMap affects NEW
  policies only. Existing policy constraints may be edited intentionally, but
  capacity shape/placement changes are not implemented by deleting live VMs.
- Spot-to-OnDemand replacement remains opt-in. For an existing TrainingPolicy,
  set `spec.replacement.enabled=true` on that policy. For future automatically
  discovered policies, put `replacement.enabled: true` in
  `hybridspot-system/automatic-policy-defaults` before creation. If omitted,
  replacement stays disabled by default.
- Each namespace needs its referenced SpotRiskProfile. AWS namespaces need the
  credentials Secret and matching NodeProvisionNetConfig. Secrets are not copied.
- Names are deterministic: workload prefix plus name hash. Discovery refuses
  manual-policy adoption, name collision or reuse by a recreated workload UID.
- TrainingRuntime and its source-only PropagationPolicy are automatically created.
  The StatefulSet receives an origin UID label required by the runtime verifier.
  Automatic runtime setup currently requires one application container, port 8298.
- ResourceBinding changes and risk/status updates enqueue policy reconciliation.
  TrainingRuntime changes also enqueue CheckpointCoordinator. Discovery polls
  every 15 seconds to repair missed events; RB selection is checked live before
  automatic cloud allocation. Manual policies retain the previous behavior.
- No RB or workload PropagationPolicy is changed by discovery. Its only workload
  mutation is the origin UID metadata label.
- Policies/runtimes/VMs are NOT garbage-collected by workload deletion. Existing
  capacity requires explicit safe cleanup; policy/source UID mismatch blocks reuse.

See the upstream [RB definition](https://github.com/karmada-io/karmada/blob/master/pkg/apis/work/v1alpha2/binding_types.go)
for target selection versus actual execution state.

## Upgrade from MGMT

Keep existing Karmada credentials and deployments. This changes policy-manager
AND checkpoint-coordinator, not the risk collector or member runtime binary.

```bash
cd /root/hybridspot-validation/System
git pull --ff-only
for component in policy-manager checkpoint-coordinator; do
  IMG="docker.io/jeongseungjun/hybrid-spot-vm-system:${component}_v1.1"
  buildah build --build-arg COMPONENT="$component" -t "$IMG" .
  buildah push "$IMG" "docker://$IMG"
done

kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/crd/trainingpolicies.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/karmada/access.yaml
for component in policy-manager checkpoint-coordinator; do
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image \
    deployment/"$component" manager="docker.io/jeongseungjun/hybrid-spot-vm-system:${component}_v1.1"
  kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status \
    deployment/"$component" --timeout=180s
done
```

All pre-existing System, NodeProvision, FluidCR and RestoreRequest CRDs must
already exist on Karmada. Install the runtime collector and RIC definitions on
each actual training cluster. Restores require a compatible GPU target.

## Test 2: Automatic One Spot and One On-Demand

First check all existing workloads and policies, not only fluidcr-demo:

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" get sts,trainingpolicies -A
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo get spotriskprofile trainer-risk-01 -o json | jq '{spec:.spec,status:.status}'
```

Use the existing STATIC profile lambdaPerHour=0.1, ready=true, matching
observedGeneration and future validUntil. Do NOT use trainer-risk-trace for
this test. With alpha=0.8, horizon=3600, minOnDemand=1, replicas=2, the expected
decision is one Spot and one OD. Cost optimization remains unevaluated.

Prepare the common settings, using actual values from the previously successful
NodeProvision. Add iamInstanceProfile only if required in your environment.

```bash
mkdir -p /root/hybridspot-validation/rendered
cp config/samples/13-automatic-policy-defaults.yaml /root/hybridspot-validation/rendered/automatic-policy-defaults.yaml
nano /root/hybridspot-validation/rendered/automatic-policy-defaults.yaml
```

Replace ALL __REPLACE_ values. The CRD does not validate ConfigMap contents;
the controller rejects placeholder values. The credentialsRef without namespace
uses each workload's namespace. These nodes reuse that namespace's NetConfig,
including runtime package/NFS/GPU bootstrap settings.

The following commands ENABLE REAL EC2 PROVISIONING when matching RBs exist:

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f /root/hybridspot-validation/rendered/automatic-policy-defaults.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/samples/14-capacity-probe.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo get rb
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo get trainingpolicies -w
```

No TrainingPolicy YAML is applied by the user. Stop watch with Ctrl-C.
The sample is a PAUSE capacity probe, NOT a training program. It does not
provide FluidCR runtime endpoints, data PVCs, DDP, Checkpoint or Restore.
Replace its pod specification with the real validated training workload before
tests 3-5; do not delete/recreate the StatefulSet and silently reuse the policy.
If the namespace already contains trainer, do not overwrite it with this probe.

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo get trainingpolicies -o json |
  jq '.items[] | {name:.metadata.name,spec:.spec,discovery:.status.discovery,policy:.status.policy,checkpoint:.status.checkpoint}'
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo get nodeprovisions \
  -o custom-columns='NAME:.metadata.name,MARKET:.spec.marketType,PHASE:.status.phase'
kubectl --kubeconfig="$AWS_KUBECONFIG" -n fluidcr-demo get nodeprovisions \
  -o custom-columns='NAME:.metadata.name,MARKET:.spec.marketType,PHASE:.status.phase'
kubectl --kubeconfig="$AWS_KUBECONFIG" get nodes -o wide
```

Pass criteria: automatic policy source=aws, discovery Stable, desiredWorkers=2,
spotWorkers=1, onDemandWorkers=1; exactly two owned NodeProvisions propagated to
AWS; eventually two worker nodes Ready. A policy decision alone is not proof
of successful EC2 provisioning. GPU probe Pods need the GPU driver and resource
advertiser; Pending Pods do not necessarily mean the capacity decision failed.
Checkpoint status runtime_not_ready is EXPECTED for the pause probe.

Troubleshooting: inspect policy status and both management/member controller
logs. No policy: check RB UID/target, defaults, pre-existing manual policy,
RBAC, and policy-manager logs. Policy with no nodes: check discovery.ready,
risk freshness, region/AZ/type match and fixed-capacity replacement gates.

## Other Cluster to AWS: Recognition and Handoff

Start discovery while the workload is still assigned to its original cluster.
It creates an automatic policy with that source and a source TrainingRuntime;
it does not allocate AWS nodes for a non-AWS target. Source checkpointing starts
only when real runtime evidence and a fresh risk profile meet existing gates.

Before changing placement, run the existing migration suspension workflow.
Observe dispatch suspension in the actual Work, finish required source
checkpoint/artifact preparation, and fence the old writer as required. A
reactive RB watcher CANNOT guarantee preservation of the old Pods if placement
is changed first. Do not infer fencing from dispatch suspension.

When the RB target becomes aws:

1. Discovery retains spec.sourceCluster and records status.discovery target,
   binding UID and transitionStartedAt. MigrationRequired means a handoff is
   needed, NOT that migration has completed.
2. AWS capacity preparation is allowed only while RB dispatch is suspended.
   UnsafePlacementChange blocks new automatic AWS allocation; it cannot undo
   a placement change already dispatched by Karmada.
3. Target TrainingRuntime is created and propagated separately. Existing source
   Checkpoint orchestration keeps using the old source while runtime remains valid.
4. Use existing Migration/PVMigration/Restore components to perform the operation.
   Cross-cluster placement migration still does NOT auto-create a migration
   operation/RestorePlan, fence the source, copy artifacts, write placement,
   release suspension or delete nodes. Spot-to-OnDemand replacement inside AWS
   uses the separate `SpotReplacement` contract in `spot-replacement-contract.md`.
   PVMigration's target-not-selected preparation rule still applies: prepare PVs
   BEFORE adding the target to RB.clusters. Discovery is not a replacement for
   that ordered migration workflow.
5. Restore verification must reference the auto-created TARGET TrainingRuntime
   name and UID. Discovery accepts only a current-generation Verified RestoreRequest,
   with durable sourceFence evidence plus matching workload/source/target/
   checkpoint/request/runtime evidence, created after transitionStartedAt.
   Historical restore evidence is not accepted. Complete any earlier source
   checkpoint before switching.
6. After the existing migration controller releases dispatch suspension, fresh
   target runtime evidence plus that restore proof allow automatic sourceCluster
   and runtimeRef switching. Subsequent checkpoints use the new source.

Migration detection is automated; a full end-to-end autonomous placement-to-
migration orchestrator is NOT part of this change. If the existing migration
workflow creates RestoreRequest before the transition is detected, it will
not be adopted as new proof. Plan this handoff explicitly, not by forging status.
Cold-start discovery after an untracked migration cannot reconstruct old source
history from target intent alone. Keep discovery active before migration.

Multiple target clusters, dynamic worker-count changes, GPU compatibility and
economic optimization require separate supported workflows. Spot-to-OnDemand
capacity replacement is supported only through UID-bound SpotReplacement plus
verified RestoreRequest evidence. NodeReady alone never proves DDP restore
correctness.

## Verification Performed

Local tests cover automatic 1+1 allocation/idempotence, non-AWS no allocation,
risk-event mapping, source retention, suspended migration gate, stale/split/
partial/zero bindings, workload recreation, current restore proof and rejection
of historical/stale-generation proof. Real AWS, GPU training and migration need
the live checks above; fake-client tests do not prove those integrations.
