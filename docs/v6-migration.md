# v6.0 controller migration

## Plan and scope

The actual-presentation architecture defines names and responsibilities.
Lock registration and rendering with regression tests, split lifecycle from VM
management, integrate checkpoint/recovery coordination, align deployment/build
names, then validate and publish source commits. Image publication and live
deployment are separate operations, not implied by a successful source push.

## Component ownership

- Public Cloud VM Collector publishes VM information and risk profiles.
- Spot VM Management Controller owns baseline/replacement capacity and retires
  old capacity only after validating recovery evidence.
- Checkpoint/Recovery Coordinator owns checkpoint scheduling and restore request
  production; replacement capacity is prepared by VM management first.
- Training Lifecycle Controller owns workload discovery, TrainingRuntime
  registration and placement transitions. Existing fenced cross-cluster target
  bootstrap remains in GroupPlacement; it is not a general VM allocator.
- Training Runtime Controller reports member workload runtime state.
- Spot Watcher reports node-local interruption information.

The placement webhook remains a separate supporting image. Existing member
checkpoint/restore and multi-cluster restore executors are reused, not folded
into these management processes.

## Persisted APIs

NodeRequest replaces TrainingPolicy as the persisted user request API.
Its spec contains VM capacity requirements, targetWorkers, and workloadRef
(apiVersion, kind, name). Currently only apps/v1 StatefulSet in the request's
namespace is supported. NodeProvision remains the provisioner's per-VM API.

Submit NodeRequest without workloadRef.uid to allocate initial public-cloud VMs.
Deploy the referenced workload separately after capacity is ready. Training
Lifecycle Controller binds its UID once, then discovers placement and creates
TrainingRuntime. Checkpoint and recovery require this binding and the existing
placement/runtime gates. Recreated workloads are not automatically adopted.
Training completion does not delete a user-owned NodeRequest.

This is a breaking Kind change, not an in-place rename. Existing TrainingPolicy
objects are not watched by the new controllers. Do not delete their CRD or copy
their status/ownerReferences into NodeRequest. Finish active operations and
perform legacy cleanup with the old controllers before creating fresh requests;
keep checkpoint artifacts. Live-resource adoption is not implemented. Preserve
all UID, durability, source fencing and verified recovery gates.

## Cutover

In a maintenance window, stop legacy hybridspot-management/hybridspot-runtime
if present, plus old policy-manager, checkpoint-coordinator,
spot-recovery-controller, training-runtime-collector and vm-spot-risk-collector.
Wait for their Pods to terminate BEFORE starting new controllers. New names and
leases do not stop existing deployments. Legacy entrypoints are not intended
to run alongside replacements. Preserve CRs, storage, secrets and kubeconfigs.
Training completion alone must not automatically delete user-owned VM requests.

## Release

Run `bash scripts/build-v6.sh` on the Linux Buildah host from this checkout.
Default is build only; PUSH_IMAGES=1 explicitly enables image pushes.
Render scripts/install-system.py stages using image repository
jeongseungjun/hybrid-spot-vm-system and tag v6.0. Review before applying with
explicit kubectl kubeconfig/context. Unit tests do not certify cloud E2E behavior.
