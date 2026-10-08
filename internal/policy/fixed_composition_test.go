package policy

import (
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFixedCompositionRuntimeBounds(t *testing.T) {
	for _, tc := range []struct {
		name           string
		count, minimum int64
		valid          bool
	}{
		{"negative", -1, 0, false}, {"above-target", 3, 0, false},
		{"below-minimum", 0, 1, false}, {"explicit-zero", 0, 0, true},
		{"minimum", 1, 1, true}, {"all-on-demand", 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := PolicyInput{TargetWorkers: 2, MinOnDemand: tc.minimum, FixedOnDemand: &tc.count}
			if err := ValidateFixedComposition(input); (err == nil) != tc.valid {
				t.Fatalf("validation: %v", err)
			}
			d := DecideAt(input, RuntimeSnapshot{}, RiskSnapshot{Ready: true}, time.Now())
			if !tc.valid && (!d.ProvisioningBlocked || d.Reason != "invalid_spec" || d.OnDemandWorkers != 0 || d.SpotWorkers != 0) {
				t.Fatalf("invalid bounds produced actionable decision: %+v", d)
			}
		})
	}
}

func TestFixedCompositionParsingAndExplicitZero(t *testing.T) {
	obj := NewObject("TrainingPolicy")
	obj.Object["spec"] = map[string]interface{}{"targetWorkers": int64(2), "policy": map[string]interface{}{}}
	if input := ReadPolicyInput(obj); input.FixedOnDemand != nil {
		t.Fatal("omitted fixedOnDemand must preserve adaptive allocation")
	}
	for _, count := range []int64{0, 1, 2} {
		if err := unstructured.SetNestedField(obj.Object, count, "spec", "policy", "fixedOnDemand"); err != nil {
			t.Fatal(err)
		}
		input := ReadPolicyInput(obj)
		if input.FixedOnDemand == nil || *input.FixedOnDemand != count {
			t.Fatalf("fixedOnDemand=%d was not preserved: %+v", count, input)
		}
		d := DecideAt(input, RuntimeSnapshot{}, RiskSnapshot{Ready: true, LambdaPerHour: 80}, time.Now())
		if d.OnDemandWorkers != count || d.SpotWorkers != 2-count || d.ProvisioningBlocked {
			t.Fatalf("fixedOnDemand=%d: %+v", count, d)
		}
	}
}

func TestFixedCompositionKeepsRiskDrivenCheckpointAndEconomicEvidence(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	fixed := int64(1)
	input := PolicyInput{
		TargetWorkers: 2, MinOnDemand: 1, FixedOnDemand: &fixed, Alpha: .8, ForecastSeconds: 3600,
		Checkpoint: CheckpointPolicy{MeasuredCosts: MeasuredCosts{CheckpointSeconds: 90, CopySeconds: 30, ObservedAt: now.Format(time.RFC3339)}},
		Economics:  EconomicsPolicy{Enabled: true, LossCostPerEviction: 100, ObservedAt: now.Format(time.RFC3339)},
	}
	for _, tc := range []struct {
		lambda float64
		want   int64
	}{{0, 600}, {20, 180}, {80, 90}} {
		risk := RiskSnapshot{Ready: true, LambdaPerHour: tc.lambda, SpotPricePerHour: 2, OnDemandPricePerHour: 1}
		d := DecideAt(input, RuntimeSnapshot{}, risk, now)
		if d.DesiredWorkers != 2 || d.OnDemandWorkers != 1 || d.SpotWorkers != 1 || d.ProvisioningBlocked || d.Reason != "fixed_worker_composition" {
			t.Fatalf("lambda=%v changed fixed composition: %+v", tc.lambda, d)
		}
		if d.CheckpointIntervalSeconds != tc.want || !d.IntervalCostEvaluated || d.IntervalSource != "checkpoint-cost-analytic" || d.LambdaPerHour != tc.lambda {
			t.Fatalf("lambda=%v must drive interval=%d: %+v", tc.lambda, tc.want, d)
		}
		if !d.PriceEvaluated || !d.CostEvaluated {
			t.Fatalf("fixed composition discarded economic evidence: %+v", d)
		}
		adaptive := input
		adaptive.FixedOnDemand = nil
		if previous := DecideAt(adaptive, RuntimeSnapshot{}, risk, now); previous.OnDemandWorkers != 2 || previous.SpotWorkers != 0 {
			t.Fatalf("unset fixed composition changed existing price fallback: %+v", previous)
		}
	}
}

func TestFixedCompositionPreservesRiskUnavailableGateAndBootstrap(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	fixed := int64(1)
	input := PolicyInput{TargetWorkers: 2, FixedOnDemand: &fixed, Alpha: .8, ForecastSeconds: 3600}
	for _, risk := range []RiskSnapshot{{}, {Ready: true, LambdaPerHour: -1}, {Ready: true, LambdaPerHour: math.NaN()}} {
		d := DecideAt(input, RuntimeSnapshot{}, risk, now)
		if !d.ProvisioningBlocked || d.Reason != "risk_unavailable" || d.OnDemandWorkers != 0 || d.SpotWorkers != 0 {
			t.Fatalf("fixed composition bypassed unavailable-risk gate: %+v", d)
		}
	}
	for _, tc := range []struct {
		lambda float64
		want   int64
	}{{.01, 600}, {.1, 120}} {
		d := DecideAt(input, RuntimeSnapshot{}, RiskSnapshot{Ready: true, LambdaPerHour: tc.lambda}, now)
		if d.OnDemandWorkers != 1 || d.SpotWorkers != 1 || d.CheckpointIntervalSeconds != tc.want || d.IntervalCostEvaluated || d.IntervalSource != "risk-band-bootstrap" {
			t.Fatalf("bootstrap no longer adapts with fixed composition: %+v", d)
		}
	}
}
