# Trace Risk Calculation Validation

## Meaning and Limits

Static mode publishes the supplied lambda; it does not estimate it.
Trace mode reads raw JSON from a ConfigMap in the profile's namespace on
Karmada and calculates an experimental availability-loss proxy.

The [SkyPilot dataset](https://github.com/skypilot-org/spot-traces) availability
files contain available instance counts, not running-instance eviction events.
Capacity decreases must NOT be claimed as measured preemptions. Explicit
`allowAvailabilityProxy: true` opts into this approximation for experiments.
Actual preemption hazard estimation needs event and at-risk exposure data,
which this availability source cannot supply.

For samples x[start] through x[end] with spacing gap_seconds:

```text
downwardUnits = sum(max(x[i] - x[i+1], 0)), start <= i < end
exposureInstanceHours = sum(x[i] * gap_seconds / 3600), start <= i < end
lambdaPerHour = downwardUnits / exposureInstanceHours
```

This is a simple proxy, not a calibrated predictor and not the SARIMA method from the cost-efficient training paper. The paper SARIMA path is the external `internal/collector/paper_sarima_feed.py` service wired through `spec.paperEstimator.feedEndpoint`; trace mode remains a proxy and is not paper-equivalent. Capacity recovery is not
subtracted from earlier losses. No transitions after endIndex are used.
Zero exposure, null/negative/fractional counts, invalid JSON and incomplete
windows fail closed with ready=false. Input is limited to 1 MiB/100000 samples.

`endIndex` is zero-based and inclusive. `windowSamples` includes both
endpoints: 13 samples at 300-second spacing represent one hour.
The full-file example below is an OFFLINE aggregate, not an online backtest.
For replay, begin with a past window and explicitly advance endIndex; there
is no automatic replay clock. The collector rereads the ConfigMap on each poll.
Updating the data or selected window recalculates the result.

observedAt means calculation time; validUntil bounds the calculated result's
cache lifetime, NOT historical trace freshness. Polling renews these times.
The SHA256, ConfigMap resourceVersion, selected indexes and calculation terms
are retained in status.source. Historical output stays marked experimental.

## Update the Installed Collector

Run from MGMT. Existing kubeconfig variables must point to the appropriate
APIs. Karmada stores the CRD, profile, ConfigMap and access role; the MGMT
host runs the collector Deployment. These commands do not modify worker VMs.

```bash
cd /root/hybridspot-validation/System
git pull --ff-only
export IMG=docker.io/jeongseungjun/hybrid-spot-vm-system:vm-spot-risk-collector_v1.1
buildah build --build-arg COMPONENT=vm-spot-risk-collector -t "$IMG" .
buildah push "$IMG" "docker://$IMG"

kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/crd/spotriskprofiles.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/karmada/access.yaml
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system set image deployment/vm-spot-risk-collector manager="$IMG"
kubectl --kubeconfig="$MGMT_KUBECONFIG" -n hybridspot-system rollout status deployment/vm-spot-risk-collector --timeout=180s
```

The updated access role adds ConfigMap GET permission. No Member API access
is required. Keep the existing controller Karmada credential Secret.

## Upload and Calculate

Set TRACE to the actual uploaded file path. Inspect its length first; the
sample profile selects 4736 samples and must be adjusted for other files.
Keep the profile separate from trainer-risk-01 during this calculation test.

```bash
export TRACE=/root/aws-spot-test/spot-data/us-west-2c_v100_1.json
test -s "$TRACE"
jq '{gap_seconds:.metadata.gap_seconds,samples:(.data|length)}' "$TRACE"
mkdir -p /root/hybridspot-validation/rendered
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo create configmap skypilot-availability --from-file=trace.json="$TRACE" --dry-run=client -o yaml > /root/hybridspot-validation/rendered/spot-trace-configmap.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f /root/hybridspot-validation/rendered/spot-trace-configmap.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" apply -f config/samples/11-spot-risk-profile-trace.yaml
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo get spotriskprofile trainer-risk-trace -o json | jq '{generation:.metadata.generation,status:.status}'
```

Poll until ready=true and observedGeneration equals metadata.generation.
For the tested aws-08-27-2023/us-west-2c_v100_1.json full file:

- gap_seconds: 300; samples: 4736
- downwardUnits: 1366
- exposureInstanceHours: approximately 2695.083333333
- lambdaPerHour: approximately 0.5068488915

This is per hour, NOT a 50.68% interruption probability.
Do not silently attach this profile to an active TrainingPolicy: changing its
risk input may change real VM provisioning decisions and charges.
First validate the numbers, then intentionally select the risk profile.

To test a one-hour historical window:

```bash
kubectl --kubeconfig="$KARMADA_KUBECONFIG" -n fluidcr-demo patch spotriskprofile trainer-risk-trace --type=merge -p '{"spec":{"trace":{"windowSamples":13,"endIndex":12}}}'
```

A zero-exposure window intentionally returns ready=false instead of inventing
a hazard. No automatic AWS feed ingestion is provided by this trace mode.

## Local Tests

```bash
go test -mod=readonly ./internal/collector ./internal/manifests
SPOT_TRACE_FILE="$TRACE" go test -mod=readonly ./internal/collector -run TestLocalSkyPilotTrace -v
```

The optional local-file test is specific to the 4736-sample dataset above.
Deployment and live-cluster integration must still be verified on your MGMT.
