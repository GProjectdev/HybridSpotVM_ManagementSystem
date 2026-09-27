# Public Cloud VM Risk Feed

The VM Spot Risk Collector is implemented in `internal/collector` and consumes
an externally supplied hourly preemption hazard, a static experimental value,
or a ConfigMap-backed historical availability trace. See
[trace calculation and validation](trace-risk-validation.md) for the experimental
availability-proxy estimator, its limitations, and deployment commands.
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

Use exactly one of `spec.endpoint`, `spec.staticLambdaPerHour`, `spec.trace`, or `spec.paperEstimator`. Static values are only
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

## Paper SARIMA Estimator

The paper method is implemented as an external Python HTTPS producer, not inside the Go collector image. Configure `spec.paperEstimator.feedEndpoint` to point at that service. The collector fetches the normal feed contract, verifies freshness, and marks `status.source.type: paper-sarima` with the SARIMA metadata.

Paper evidence: `C:/Users/JeongSeungJun/Desktop/졸업준비/관련논문전체/Cost_efficient_paper.pdf`, Section 4.1 on PDF page 4 / paper page 34 states that the target is user-experienced spot preemption rate, not aggregate spot availability. The same section says training time is split into `PT = 1 hour`, availability count samples are converted to raw counts with `c_i = max(0, a_{i-1} - a_i)`, raw counts are aggregated into hourly `r_i`, sparse counts are smoothed with `W = 3` hour causal rolling mean, and `SARIMA(1,1,1)(1,0,1)24` is fit. It also states weekly retraining over the most recent `M = 4 weeks` of data.

Use `internal/collector/paper_sarima_feed.py` with `internal/collector/paper_sarima_requirements.txt`, or build the separate service image. The collector uses the standard Go HTTPS trust store; a self-signed in-cluster certificate will not silently work unless that CA is trusted by the collector container/host through the platform trust store. Use a public/private CA chain trusted by the collector, or add explicit CA bundle support before relying on a self-signed Kubernetes Service endpoint.

```bash
cd internal/collector
docker build -f paper_sarima.Dockerfile -t paper-sarima-risk-feed:local .
# buildah build -f paper_sarima.Dockerfile -t paper-sarima-risk-feed:local .
docker run --rm -p 8443:8443   -v "$PWD/input:/input:ro"   -v "$PWD/tls:/tls:ro"   paper-sarima-risk-feed:local   --input /input/availability.json   --host 0.0.0.0   --port 8443   --certfile /tls/tls.crt   --keyfile /tls/tls.key
```

Input must be a fixed-gap availability-count trace, for example:

```json
{
  "metadata": {
    "gap_seconds": 300,
    "start_time": "2026-09-26T00:00:00Z",
    "risk_population": 16
  },
  "data": [16, 16, 15, 16, 14]
}
```

`risk_population` is required because the SARIMA model forecasts aggregate downward preemption counts per hour for the sampled population. The service does not accept `max_request_size` as a synonym: request size alone is not proof that every requested VM was actually at risk. The feed publishes policy-compatible approximate per-instance `lambdaPerHour = aggregateForecastPreemptionsPerHour / riskPopulation`, caps only after that division, and preserves `aggregateForecastPreemptionsPerHour` under `source`. Do not publish aggregate 16-node counts as the per-VM lambda consumed by policy; policy multiplies lambda by spot worker count.

The service rejects missing hourly evidence instead of filling absent hours with zero. Availability transitions are assigned to the hour in which the transition interval starts, and inputs must cover complete hour bins from an hour-aligned `start_time`. Future evidence fails closed. By default it requires the full 4-week history window (`672` hourly rates), matching the paper retraining window; `--min-history-hours` exists only for controlled smoke tests. HTTPS mode caches fitted SARIMA parameters for `--retrain-seconds` seconds, defaulting to one week. New input data before that retrain deadline refreshes the forecast using cached parameters; it does not refit parameters on every HTTP request. Cached responses are never re-dated and are not returned after `validUntil`, so old traces expire instead of becoming fresh.

A `SpotRiskProfile` for this mode looks like:

```yaml
spec:
  paperEstimator:
    method: sarima
    feedEndpoint: https://paper-sarima-risk-feed.fluidcr-demo.svc:8443/feed
    rollingMeanHours: 3
    retrainWindowWeeks: 4
    seasonalPeriodHours: 24
  maxAgeSeconds: 3600
```

The ConfigMap trace mode remains an experimental availability-loss proxy and must not be described as the paper SARIMA estimator.

## Measured Checkpoint Costs

`TrainingPolicy.spec.checkpoint.measuredCosts` remains a manual compatibility input, but automatically collected values should be written under `status.checkpoint.measuredCosts`. Policy parsing prefers the status value when present. Missing, future, stale, negative, nonfinite, or zero checkpoint duration values are ignored; a legitimate fresh `copySeconds: 0` is valid when checkpoint completion and export timestamps are equal.
