# Public Cloud VM Risk Feed

The VM Spot Risk Collector is implemented in `internal/collector` and consumes
an externally supplied hourly preemption hazard, or a static value for controlled experiments.
It does not call a cloud-provider "interruption probability" API, does not use
AWS Spot price or placement score as a hazard estimate, and does not implement
AWS hazard estimation. Providers, experiments, or offline models must publish
the normalized HTTPS contract below.

## SpotRiskProfile Shape

The collector currently expects this schema:

```yaml
spec:
  endpoint: https://risk.example.test/feed
  maxAgeSeconds: 3600
  pollSeconds: 300
  credentialSecretRef:
    name: risk-feed-token
    key: token
status:
  observedGeneration: 7
  lambdaPerHour: 0.05
  observedAt: "2026-09-26T04:00:00Z"
  validUntil: "2026-09-26T05:00:00Z"
  ready: true
  source:
    type: https
    host: risk.example.test
  prices:
    spotPricePerHour: 0.017
    onDemandPricePerHour: 0.05
```

`status.observedGeneration` is written on both success and failure so consumers
can tell whether `ready` reflects the current spec generation. Price fields are
intentionally nested under `status.prices`; consumers should not read flat price
fields from status.

Use either `spec.endpoint` or `spec.staticLambdaPerHour`. Static values are only
for controlled experiments and are reported with `source.type: static` plus
`source.provenance: static-experiment-spec`. Do not document static values as
live provider telemetry.

The HTTPS sample is `config/samples/11-spot-risk-profile-https.yaml`. It creates
`SpotRiskProfile` named `aws-risk-feed` in namespace `fluidcr-demo` and contains
the placeholder endpoint `https://risk-feed.example.invalid/aws/ap-northeast-2/g4dn.xlarge`.
Replace that endpoint with a real provider, experiment, or offline-model feed before applying
it. Also update the `TrainingPolicy` risk reference to point at `aws-risk-feed`;
applying the HTTPS sample next to the static sample does not automatically switch
an existing policy from one profile to the other.

## Endpoint Contract

`spec.endpoint` must be HTTPS and return JSON:

```json
{
  "lambdaPerHour": 0.05,
  "observedAt": "2026-09-26T04:00:00Z",
  "validUntil": "2026-09-26T05:00:00Z",
  "spotPricePerHour": 0.017,
  "onDemandPricePerHour": 0.05
}
```

`lambdaPerHour` is the hourly preemption hazard supplied by the endpoint. The
collector validates that hazard and optional prices are finite, nonnegative
numbers. `observedAt` and `validUntil` must be RFC3339 timestamps; observations
older than `spec.maxAgeSeconds` or already expired are rejected.

## Transport And Credentials

The collector limits response size, uses request timeouts, and does not follow
redirects so bearer credentials cannot be forwarded to another origin. Optional
bearer credentials are read from `credentialSecretRef`, and that Secret must be
in the same namespace as the `SpotRiskProfile`. The controller reads credential
Secrets through an uncached API reader so RBAC only needs `get`; it does not need
Secret `list` or `watch` permissions.
