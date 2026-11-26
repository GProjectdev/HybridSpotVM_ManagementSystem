# On-Premise-first control foundations

This package contains decision functions and read-only identity-aware observations
for the TrainingRun / TrainingCapacity / TrainingTransition architecture. It is
not yet a deployed replacement for the existing management controllers. No new
paid allocation or restore is initiated by this package at this revision.

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

Remaining integration: reconciler loops, validated CRD schemas, ownership/RBAC,
checkpoint and restore engine receipt adapters, runtime deployment, actual
On-Premise-to-Cloud activation and recovery verification. CPU tests are not a
claim that these live paths have passed.
