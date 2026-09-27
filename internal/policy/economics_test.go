package policy

import (
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestEconomicFallback(t *testing.T) {
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	base := PolicyInput{ForecastSeconds: 3600, Economics: EconomicsPolicy{Enabled: true, LossCostPerEviction: 20, ObservedAt: now.Format(time.RFC3339)}}
	risk := RiskSnapshot{Ready: true, LambdaPerHour: 0.1, OnDemandPricePerHour: 1}
	if fallback, evaluated := economicFallback(base, risk, now); !fallback || !evaluated {
		t.Fatal("expected fallback")
	}
	zero := base
	zero.Economics.LossCostPerEviction = 0
	if fallback, evaluated := economicFallback(zero, risk, now); fallback || !evaluated {
		t.Fatal("fresh explicit zero is valid")
	}
	for _, mutate := range []func(*PolicyInput, *RiskSnapshot){
		func(p *PolicyInput, r *RiskSnapshot) { p.Economics.Enabled = false },
		func(p *PolicyInput, r *RiskSnapshot) { p.Economics.ObservedAt = "" },
		func(p *PolicyInput, r *RiskSnapshot) {
			p.Economics.ObservedAt = now.Add(time.Hour).Format(time.RFC3339)
		},
		func(p *PolicyInput, r *RiskSnapshot) {
			p.Economics.ObservedAt = now.Add(-11 * time.Minute).Format(time.RFC3339)
		},
		func(p *PolicyInput, r *RiskSnapshot) { p.Economics.LossCostPerEviction = math.NaN() },
		func(p *PolicyInput, r *RiskSnapshot) { p.Economics.LossCostPerEviction = -1 },
		func(p *PolicyInput, r *RiskSnapshot) { r.OnDemandPricePerHour = 0 },
		func(p *PolicyInput, r *RiskSnapshot) { r.LambdaPerHour = math.Inf(1) },
		func(p *PolicyInput, r *RiskSnapshot) { r.Ready = false },
	} {
		p, r := base, risk
		mutate(&p, &r)
		if fallback, evaluated := economicFallback(p, r, now); fallback || evaluated {
			t.Fatal("invalid economic input evaluated")
		}
	}
}

func TestDecideEconomicFallbackAndParser(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{
		"targetWorkers": int64(2), "policy": map[string]interface{}{"minOnDemand": int64(1), "economics": map[string]interface{}{
			"enabled": true, "lossCostPerEviction": float64(20), "observedAt": time.Now().UTC().Format(time.RFC3339),
		}},
	}}}
	input := ReadPolicySpec(obj)
	risk := RiskSnapshot{Ready: true, LambdaPerHour: .1, OnDemandPricePerHour: 1}
	d := Decide(input, RuntimeSnapshot{}, risk)
	if !d.CostEvaluated || d.OnDemandWorkers != 2 || d.SpotWorkers != 0 {
		t.Fatalf("unexpected economic decision %+v", d)
	}
	input.Economics.Enabled = false
	d = Decide(input, RuntimeSnapshot{}, risk)
	if d.CostEvaluated || d.OnDemandWorkers != 1 || d.SpotWorkers != 1 {
		t.Fatalf("legacy decision changed %+v", d)
	}
}
