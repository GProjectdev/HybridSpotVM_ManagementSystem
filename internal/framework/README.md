# On-Premise-first control foundations

This package contains decision functions, identity-aware observations and staged
TrainingRun / Checkpoint reconciler processes. Execute mode currently supports
initial On-Premise placement, runtime registration and the existing FluidCR
periodic checkpoint engine. Observe mode writes Run status only.

Cloud activation and transition execution are NOT enabled. The separate
TrainingCapacity controller implements UID-bound AWS VPC provisioning and
provider-receipted retirement; TrainingRun does not yet submit capacities.
A due Cloud preparation is reported as CloudActivationBlocked. No paid allocation,
source fencing, workload migration or restore is initiated by this revision.
TrainingTransition remains a reserved execution contract. CRD installation alone
does not establish that any controller or recovery path is deployed.

Covered contracts:

- Prepare Cloud capacity at the first confirmed shortage plus `max(0, wait - lead)`;
  default wait and lead are both 600 seconds. Earlier readiness cannot shorten
  an explicit wait. On-Premise admission wins before execution commitment.
- Bound a user's checkpoint interval with the existing risk-policy result and
  the 3,600-second safety limit; an unavailable risk estimate is not zero risk.
- Prepare the planned target before requesting a fresh checkpoint. For actual
  interruption, prepare capacity and attempt emergency save concurrently; only
  complete, integrity-verified, matching all-rank archives are eligible.
- Require source fencing, verified restore, all-rank progress and confirmed
  source retirement in sequence. Observation booleans must be produced from
  trusted engine receipts, never copied from user annotations.
- Verify workload / binding / member / Pod identities and fresh rank telemetry.
  Pod readiness alone is insufficient evidence that training has started.

Remaining integration: Transition reconciler loop, Cloud execution
permit and cancellation/retirement integration, checkpoint evidence/restore
receipt adapters and full cross-cluster recovery verification. A node connection
or initial On-Premise checkpoint does not validate these paths.

All staged controller binaries require TRAINING_NAMESPACE. Keep credentials scoped to that
namespace; member credentials need read-only StatefulSet and Pod access. Old
NodeRequest workloads are rejected rather than silently adopted. Existing E1/E2
results and images remain immutable.

Karmada reference: https://karmada.io/docs/v1.15/userguide/scheduling/resource-propagating/
Ordered clusterAffinities and a single cluster spread group are used. No scheduler
fork or direct ResourceBinding cluster assignment is introduced.

Optional executionGate injects a read-only init container before first propagation.
The Run controller grants the exact all-rank Pod UIDs only after scheduling. An
initial permit cannot be reassigned to replacement Pods or retrofitted after a
ResourceBinding exists. This startup guard does not replace source fencing during
recovery. Cloud activation remains blocked until Capacity and Transition receipt
integration is ready. `suspend: true` pauses new control/checkpoint actions; it does
not terminate existing workloads or retained infrastructure.

Capacity templates are immutable ConfigMaps containing `nodeProvisionSpec.json`.
Their UID, SHA256 and provisioning spec (credential references only) are pinned
before allocation. The capacity owner, cluster, template, worker limit and absolute
deadline cannot change; allocation identities/markets and retirement records must
remain in the spec. Never use this path to adopt retained Member1 infrastructure.

Cancellation before dispatch does not start a VM. Existing allocations need a
current-generation provider fence receipt tied to capacity UID + instance ID before
NodeProvision deletion. The receipt is persisted before deletion, and a missing
resource alone never proves termination. Owner deletion/deadline triggers cleanup
using the original approval snapshot. A failed creation without a confirmed
instance ID remains unresolved rather than being declared cleaned up.
