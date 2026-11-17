package policy

import (
	"testing"
	"time"
)

func TestPaperVMPolicyBootstrapAndMeasuredEquationSeven(t *testing.T) {
	now := time.Date(2026, 11, 17, 4, 0, 0, 0, time.UTC)
	in := PolicyInput{VMPolicyMethod: "paper-eq6-7", TargetWorkers: 4, MinOnDemand: 1, Alpha: .8, ForecastSeconds: 3600, Economics: EconomicsPolicy{Enabled: true, LossCostPerEviction: -1, OnDemandDurationSeconds: 3600}}
	risk := RiskSnapshot{Ready: true, LambdaPerHour: .1, SpotPricePerHour: .2, OnDemandPricePerHour: 1}
	d := DecideAt(in, RuntimeSnapshot{}, risk, now)
	if d.SpotWorkers != 3 || d.CostEvaluated || d.VMPolicyStage != "ThroughputOnlyBootstrap" {
		t.Fatalf("per-shard bootstrap: %+v", d)
	}
	risk.LambdaPerHour = .3
	if d = DecideAt(in, RuntimeSnapshot{}, risk, now); d.SpotWorkers != 0 {
		t.Fatalf("throughput switch: %+v", d)
	}
	risk.LambdaPerHour = .1
	in.Economics.LossCostPerEviction = 2
	in.Economics.ObservedAt = now.Format(time.RFC3339)
	// Separate OD residence from the one-hour forecast: .095*2 > .1.
	in.Economics.OnDemandDurationSeconds = 360
	d = DecideAt(in, RuntimeSnapshot{}, risk, now)
	if d.SpotWorkers != 0 || !d.CostEvaluated || d.VMPolicyStage != "MeasuredLossAndThroughput" {
		t.Fatalf("measured switch: %+v", d)
	}
	in.Economics.OnDemandDurationSeconds = 3600
	if d = DecideAt(in, RuntimeSnapshot{}, risk, now); d.SpotWorkers != 3 {
		t.Fatalf("OD duration ignored: %+v", d)
	}
	in.Economics.ObservedAt = now.Add(-time.Hour).Format(time.RFC3339)
	if d = DecideAt(in, RuntimeSnapshot{}, risk, now); d.CostEvaluated {
		t.Fatal("stale measurement used")
	}
	in.VMPolicyMethod = "typo"
	if d = DecideAt(in, RuntimeSnapshot{}, risk, now); !d.ProvisioningBlocked {
		t.Fatal("invalid method permitted")
	}
}
