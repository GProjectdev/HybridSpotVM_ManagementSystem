# On-Premise-first control foundations

This package contains decision functions, identity-aware observations and staged
TrainingRun / Checkpoint reconciler processes. Execute mode currently supports
initial On-Premise placement, runtime registration and the existing FluidCR
periodic checkpoint engine. Observe mode writes Run status only.

Cloud activation, capacity lifecycle and transition execution are NOT enabled.
A due Cloud preparation is reported as CloudActivationBlocked. No paid allocation,
source fencing, workload migration or restore is initiated by this revision.
TrainingCapacity/Transition schemas reserve the control contracts; their CRDs do
not imply that corresponding execution controllers are deployed.

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

Remaining integration: Capacity/Transition reconciler loops, Cloud execution
permit and cancellation/retirement integration, checkpoint evidence/restore
receipt adapters and full cross-cluster recovery verification. A node connection
or initial On-Premise checkpoint does not validate these paths.

Both staged binaries require TRAINING_NAMESPACE. Keep credentials scoped to that
namespace; member credentials need read-only StatefulSet and Pod access. Old
NodeRequest workloads are rejected rather than silently adopted. Existing E1/E2
results and images remain immutable.

Karmada reference: https://karmada.io/docs/v1.15/userguide/scheduling/resource-propagating/
Ordered clusterAffinities and a single cluster spread group are used. No scheduler
fork or direct ResourceBinding cluster assignment is introduced.
