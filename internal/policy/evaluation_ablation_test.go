package policy

import (
	"testing"
	"time"
)

// The same decision function must support all four evaluation arms. Changing
// one axis must not silently enable the other, including at high hazard.
func TestEvaluationAblationAxesRemainIndependent(t *testing.T) {
	now := time.Now().UTC()
	fixed := int64(1)
	for _, tc := range []struct {
		name                 string
		resource, checkpoint bool
	}{{"B1", false, false}, {"B2", false, true}, {"B3", true, false}, {"B4", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			in := PolicyInput{TargetWorkers: 2, MinOnDemand: 1, Alpha: .8, ForecastSeconds: 3600, FixedOnDemand: &fixed, Checkpoint: CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 60, MeasuredCosts: MeasuredCosts{CheckpointSeconds: 2, ObservedAt: now.Format(time.RFC3339Nano)}}}
			if tc.resource {
				in.FixedOnDemand = nil
			}
			if tc.checkpoint {
				in.Checkpoint.MinIntervalSeconds = 10
				in.Checkpoint.MaxIntervalSeconds = 600
			}
			low := DecideAt(in, RuntimeSnapshot{}, RiskSnapshot{Ready: true, LambdaPerHour: .05}, now)
			high := DecideAt(in, RuntimeSnapshot{}, RiskSnapshot{Ready: true, LambdaPerHour: 2}, now)
			if low.ProvisioningBlocked || high.ProvisioningBlocked {
				t.Fatal("valid comparison arm blocked")
			}
			if low.OnDemandWorkers != 1 || high.OnDemandWorkers != map[bool]int64{false: 1, true: 2}[tc.resource] {
				t.Fatalf("resource axis mismatch: low=%+v high=%+v", low, high)
			}
			if !tc.checkpoint && (low.CheckpointIntervalSeconds != 60 || high.CheckpointIntervalSeconds != 60) {
				t.Fatal("fixed checkpoint changed")
			}
			if tc.checkpoint && (!low.IntervalCostEvaluated || !high.IntervalCostEvaluated || low.CheckpointIntervalSeconds == high.CheckpointIntervalSeconds) {
				t.Fatalf("adaptive checkpoint did not independently respond: low=%+v high=%+v", low, high)
			}
			t.Logf("%s low: OD=%d Spot=%d interval=%ds; high: OD=%d Spot=%d interval=%ds", tc.name, low.OnDemandWorkers, low.SpotWorkers, low.CheckpointIntervalSeconds, high.OnDemandWorkers, high.SpotWorkers, high.CheckpointIntervalSeconds)
		})
	}
}
