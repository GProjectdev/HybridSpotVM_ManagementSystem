package policy

import (
	"testing"
	"time"
)

func TestDecisionStagesWithoutSyntheticMeasurements(t *testing.T) {
	now := time.Now().UTC()
	input := PolicyInput{TargetWorkers: 2, MinOnDemand: 1, Alpha: .8, ForecastSeconds: 3600}
	risk := RiskSnapshot{Ready: true, LambdaPerHour: .01, SpotPricePerHour: .2, OnDemandPricePerHour: 1}
	check := func(stage, source string) Decision {
		t.Helper()
		d := DecideAt(input, RuntimeSnapshot{}, risk, now)
		if d.ProvisioningBlocked || d.DecisionStage != stage || d.IntervalSource != source || d.CheckpointIntervalSeconds <= 0 {
			t.Fatalf("unexpected decision: %+v", d)
		}
		return d
	}
	d := check("Bootstrap", "risk-band-bootstrap")
	if !d.PriceEvaluated || d.CostEvaluated || d.IntervalCostEvaluated || d.SpotWorkers != 1 {
		t.Fatalf("bootstrap: %+v", d)
	}
	input.Checkpoint.MeasuredCosts = MeasuredCosts{CheckpointSeconds: 10, CopySeconds: 2, ObservedAt: now.Format(time.RFC3339)}
	check("CheckpointMeasured", "checkpoint-cost-adaptive")
	input.Economics = EconomicsPolicy{Enabled: true, LossCostPerEviction: 2, ObservedAt: now.Format(time.RFC3339)}
	d = check("EconomicsEvaluated", "checkpoint-cost-adaptive")
	if !d.CostEvaluated || d.EconomicsSource != "operator-calibration" {
		t.Fatalf("economics: %+v", d)
	}
	input.Economics.ObservedAt = now.Add(-time.Hour).Format(time.RFC3339)
	input.Checkpoint.MeasuredCosts.ObservedAt = now.Add(-time.Hour).Format(time.RFC3339)
	check("Bootstrap", "risk-band-bootstrap")
	risk.SpotPricePerHour = 2
	d = check("Bootstrap", "risk-band-bootstrap")
	if d.SpotWorkers != 0 || d.OnDemandWorkers != 2 || d.Reason != "spot_price_not_cheaper" {
		t.Fatalf("more expensive Spot: %+v", d)
	}
}
