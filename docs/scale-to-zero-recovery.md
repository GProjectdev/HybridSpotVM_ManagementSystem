# Managed DDP stop before a fresh training run

The placement webhook normally holds cross-cluster moves until restore proof
is available. A zero-replica scheduler update must not remove the source
StatefulSet or create a new placement intent merely to stop training.

The stop exception reads the authoritative StatefulSet through the API reader.
It requires an explicit spec.replicas=0 and the same nonempty workload UID.
The binding must have zero desired replicas (an omitted binding value is zero)
and exactly one existing source cluster. The webhook normalizes the proposed
placement to that source with replicas=0. It does not delete the binding or PVCs.
Normal scale-up and cross-cluster restore validation remain unchanged.

Stop is denied if placement/group restore intent remains, or a related
SpotReplacement, RestoreRequest or RestorePlan exists, including deleting or
failed objects. Related FluidCRMigration objects may remain only when their
source-cluster status is terminal at the current generation. Read/list errors
fail closed. This uses the existing hybridspot-management Karmada RBAC.

## Deployment

Build the System Dockerfile with --build-arg COMPONENT=placement-webhook.
Push the image and pin its digest in the MGMT cluster deployment
hybridspot-system/placement-webhook, container manager. Do not update
stateful-checkpoint for this fix, disable admission, or edit binding clusters
manually. Existing kubeconfig and TLS mounts must be retained.

Keep replacement risk disabled during maintenance. After rollout, allow the
scheduler to retry the failed binding update. Inspect Scheduled conditions if
it remains blocked; a remaining operation must be reconciled or retired, not
silently bypassed. Confirm Karmada and AWS StatefulSets both request zero and
AWS trainer Pods have fully terminated before archiving shared files.

Preserve checkpoint files and node resources. Starting new training is not
proof of successful native restore. Resume with replicas=2 only after the
checkpoint workspace and restore-specific settings have been prepared.

Unit tests cover empty/expanded scheduler placements, UID and replica checks,
pending operations, terminal checkpoint generations, and preservation of the
source placement. Live Karmada dispatch and GPU restore require cluster testing.
