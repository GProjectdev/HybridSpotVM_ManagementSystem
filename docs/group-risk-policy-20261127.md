# Optional group risk policy

`TrainingRun.spec.resourcePolicy.method: group-risk-v1` selects a bounded Spot
count for a fixed DDP world. Omission still selects `paper-eq6-7`. Existing
NodeRequest policies, frozen experiments, manifests and running services are
unchanged. This change has CPU validation only; it does not install a CRD or deploy
a controller. Roll out the reviewed CRD and controller version together before
using the new fields (an old schema could prune them).

```yaml
# Fragment for a NEW reviewed TrainingRun, not a patch to a frozen experiment.
spec:
  worldSize: 4
  resourcePolicy:
    method: group-risk-v1
    minOnDemand: 0
    forecastSeconds: 3600
    groupRisk:
      minimumSurvivalProbability: 0.8  # example, explicit choice; not a default
      hazardBasis: per-instance-independent
```

For each integer `s` in `0..worldSize-minOnDemand`, calculate
`P(no Spot interruption in H hours) = exp(-s * lambdaPerHour * H)`.
Select the largest count satisfying `P >= minimumSurvivalProbability` if Spot
is cheaper. Equal/more expensive Spot selects all On-Demand. Unknown risk or
invalid/missing prices blocks allocation; it does not mean zero risk. The
framework's market identity/generation/freshness validation remains in force.

The hazard basis is an explicit modelling assumption, **not a certification** of
the input. It requires per-VM hourly hazard and independent, equal-rate Spot
interruptions. An aggregate group event rate must not be multiplied by `s` again.
Current availability-drop traces and EC2 launch-probe success rates are not
validated natural interruption rates for Seoul G5 VMs. Proxy inputs can support
labelled What-if analysis only. Correlated interruptions and non-Spot failures
are not modelled by this probability.

This is a project extension, not an exact implementation of Desai equation 7.
The group probability floor `q` is distinct from the paper's throughput `alpha`.
`alpha` does not choose group composition. `q` has no implicit default or alpha
fallback. Existing legacy-survival arithmetic already used a joint survival
idea; the mathematical expression is not claimed as a novel contribution.

With H=1 hour, q=.8, world=4 and minOD=0:

| lambda/hour | Spot | On-Demand |
|---:|---:|---:|
| 0 or .05 | 4 | 0 |
| .06 | 3 | 1 |
| .08 or .10 | 2 | 2 |
| .15 | 1 | 3 |
| .25 | 0 | 4 |

`minOnDemand: 1` caps Spot at three. At lambda=.1, 2-Rank/minOD1 selects
1OD+1Spot, and 4-Rank/minOD1 selects 2OD+2Spot.

This version is **risk-only**: `VMPolicyStage=GroupRiskOnly`, `CostEvaluated=false`.
It does not apply the legacy per-VM expected-loss fallback, claim full-group cost
optimality, or invent missing recovery costs. A candidate cost/finish-time model
requires separate calibration and validation. Existing Checkpoint calculations
continue to use the chosen/actual Spot count through their existing paths; no
new checkpoint algorithm or schedule side effect is introduced by candidate
evaluation. The coordinator uses actual committed capacity while a transition
is pending.

Initial and operating decisions record `groupRisk` (q, assumption, chosen group
hazard, probability and all candidate risk eligibility), alongside existing risk
source/UID/version, lambda, prices and policy source. Candidate risk eligibility
does not imply supply availability or economic optimality. Actual replacement
still goes through the existing additive capacity, headroom, minimum residency,
checkpoint, fencing, full-group restore verification and retirement gates.

CPU verification:

```bash
go test ./internal/policy ./internal/framework ./internal/management -count=1
go run ./evaluation/group_risk_probe -trace /path/to/risk_hourly_alignment.csv
```

The probe uses explicit fixture prices and outputs JSON on stdout only. It does
not access AWS or a cluster. Its current 168-hour trace comparison still produces
no composition difference for 2/4 ranks at q=.8 versus paper alpha=.8: intermediate
risk bands have no samples. Preserve that negative result. Synthetic risk sweeps
demonstrate software behavior, not a measured benefit.

Paper reference: Desai et al., *Cost-Efficient Training and Checkpointing for Large
Models on Preemptible Cloud VMs*, Section 4.3, equations 6–7,
https://doi.org/10.1145/3805621.3807617.
