package policy

import (
	"math"
	"testing"
	"time"
)

func groupInput(n, minOD int64) PolicyInput {
	return PolicyInput{VMPolicyMethod: GroupRiskMethod, TargetWorkers: n, MinOnDemand: minOD, ForecastSeconds: 3600,
		GroupRisk: &GroupRiskPolicy{MinimumSurvivalProbability: .8, HazardBasis: IndependentPerInstanceHazard}}
}

func groupRisk(lambda float64) RiskSnapshot {
	return RiskSnapshot{Ready: true, LambdaPerHour: lambda, SpotPricePerHour: .6, OnDemandPricePerHour: 1.5}
}

func TestGroupRiskCompositionAcrossRanks(t *testing.T) {
	for _, tc := range []struct {
		n, floor int64
		lambda   float64
		spot     int64
	}{
		{2, 1, .1, 1}, {4, 1, .1, 2}, {8, 1, .1, 2},
		{4, 0, 0, 4}, {4, 0, .05, 4}, {4, 0, .06, 3},
		{4, 0, .08, 2}, {4, 0, .15, 1}, {4, 0, .25, 0},
		{4, 1, 0, 3}, {4, 4, .1, 0}, {1, 0, .1, 1}, {1, 1, 0, 0},
	} {
		d := Decide(groupInput(tc.n, tc.floor), RuntimeSnapshot{}, groupRisk(tc.lambda))
		if d.ProvisioningBlocked || d.SpotWorkers != tc.spot || d.OnDemandWorkers != tc.n-tc.spot || d.GroupRisk == nil || d.GroupRisk.SurvivalProbability < .8 || d.CostEvaluated {
			t.Fatalf("%+v -> %+v", tc, d)
		}
	}
}

func TestGroupRiskBoundaryMonotonicityAndSeparateThreshold(t *testing.T) {
	in := groupInput(4, 0)
	for spot := 1; spot <= 4; spot++ {
		boundary := -math.Log(.8) / float64(spot)
		for _, side := range []float64{1 - 1e-8, 1 + 1e-8} {
			want := int64(spot)
			if side > 1 {
				want--
			}
			if d := Decide(in, RuntimeSnapshot{}, groupRisk(boundary*side)); d.SpotWorkers != want {
				t.Fatal(spot, side, d)
			}
		}
	}
	for n := int64(1); n <= 8; n++ {
		for floor := int64(0); floor <= n; floor++ {
			in := groupInput(n, floor)
			previous := n
			for i := 0; i <= 400; i++ {
				lambda := float64(i) / 1000
				d := Decide(in, RuntimeSnapshot{}, groupRisk(lambda))
				if d.ProvisioningBlocked || d.SpotWorkers > previous || d.SpotWorkers+d.OnDemandWorkers != n || d.OnDemandWorkers < floor || d.GroupRisk.SurvivalProbability < .8 {
					t.Fatal(n, floor, lambda, d)
				}
				if d.SpotWorkers < n-floor && math.Exp(-float64(d.SpotWorkers+1)*lambda) >= .8 {
					t.Fatal("not maximal", d)
				}
				previous = d.SpotWorkers
			}
		}
	}
	in.Alpha = .99 // throughput alpha must not silently become the group threshold.
	if d := Decide(in, RuntimeSnapshot{}, groupRisk(.1)); d.SpotWorkers != 2 {
		t.Fatal(d)
	}
	in.ForecastSeconds = 7200
	if d := Decide(in, RuntimeSnapshot{}, groupRisk(.1)); d.SpotWorkers != 1 {
		t.Fatal(d)
	}
	in.GroupRisk.MinimumSurvivalProbability = 1
	if d := Decide(in, RuntimeSnapshot{}, groupRisk(.1)); d.SpotWorkers != 0 {
		t.Fatal(d)
	}
	for _, tiny := range []float64{1e-20, math.SmallestNonzeroFloat64} {
		if d := Decide(in, RuntimeSnapshot{}, groupRisk(tiny)); d.SpotWorkers != 0 {
			t.Fatal("rounding treated positive hazard as certainty", tiny, d)
		}
	}
	if d := Decide(in, RuntimeSnapshot{}, groupRisk(0)); d.SpotWorkers != 4 {
		t.Fatal(d)
	}
}

func TestGroupRiskRejectsAmbiguousOrInvalidContracts(t *testing.T) {
	for _, mutate := range []func(*PolicyInput){
		func(i *PolicyInput) { i.GroupRisk = nil },
		func(i *PolicyInput) { i.GroupRisk.MinimumSurvivalProbability = 0 },
		func(i *PolicyInput) { i.GroupRisk.MinimumSurvivalProbability = 1.1 },
		func(i *PolicyInput) { i.GroupRisk.MinimumSurvivalProbability = math.NaN() },
		func(i *PolicyInput) { i.GroupRisk.MinimumSurvivalProbability = math.Inf(1) },
		func(i *PolicyInput) { i.GroupRisk.HazardBasis = "aggregate" },
		func(i *PolicyInput) { i.TargetWorkers = 0 },
		func(i *PolicyInput) { i.TargetWorkers = 129 },
		func(i *PolicyInput) { i.MinOnDemand = -1 },
		func(i *PolicyInput) { i.MinOnDemand = 5 },
		func(i *PolicyInput) { i.ForecastSeconds = 0 },
		func(i *PolicyInput) { i.ForecastSeconds = 86401 },
		func(i *PolicyInput) { v := int64(0); i.FixedOnDemand = &v },
		func(i *PolicyInput) { i.VMPolicyMethod = "paper-eq6-7" },
	} {
		in := groupInput(4, 0)
		mutate(&in)
		if d := Decide(in, RuntimeSnapshot{}, groupRisk(.1)); !d.ProvisioningBlocked || d.Reason != "invalid_spec" {
			t.Fatal(in, d)
		}
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.MaxFloat64} {
		if d := Decide(groupInput(4, 0), RuntimeSnapshot{}, groupRisk(value)); !d.ProvisioningBlocked {
			t.Fatal(value, d)
		}
	}
	risk := groupRisk(0)
	risk.Ready = false
	if d := Decide(groupInput(4, 0), RuntimeSnapshot{}, risk); !d.ProvisioningBlocked {
		t.Fatal("unknown became zero", d)
	}
}

func TestGroupRiskPricesCheckpointAndNoLegacyEconomicDoubleCount(t *testing.T) {
	in := groupInput(4, 0)
	now := time.Now().UTC()
	in.Economics = EconomicsPolicy{Enabled: true, LossCostPerEviction: 1e6, ObservedAt: now.Format(time.RFC3339)}
	in.Checkpoint = CheckpointPolicy{MinIntervalSeconds: 60, MaxIntervalSeconds: 3600}
	d := DecideAt(in, RuntimeSnapshot{}, groupRisk(.1), now)
	want, evaluated, source := CheckpointIntervalDecision(in.Checkpoint, groupRisk(.1), RuntimeSnapshot{}, 2, now)
	if d.SpotWorkers != 2 || d.CostEvaluated || d.EconomicsSource != "" || d.VMPolicyStage != "GroupRiskOnly" || d.CheckpointIntervalSeconds != want || d.IntervalCostEvaluated != evaluated || d.IntervalSource != source {
		t.Fatal(d)
	}
	for _, price := range []float64{1.5, 2} {
		risk := groupRisk(.1)
		risk.SpotPricePerHour = price
		d := DecideAt(in, RuntimeSnapshot{}, risk, now)
		if d.SpotWorkers != 0 || d.GroupRisk.SurvivalProbability != 1 || d.Reason != "spot_price_not_cheaper" {
			t.Fatal(d)
		}
	}
	for _, price := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		for _, spot := range []bool{true, false} {
			risk := groupRisk(.1)
			if spot {
				risk.SpotPricePerHour = price
			} else {
				risk.OnDemandPricePerHour = price
			}
			if d := DecideAt(in, RuntimeSnapshot{}, risk, now); !d.ProvisioningBlocked {
				t.Fatal(d)
			}
		}
	}
}

func TestPaperPolicyRemainsDistinctFromGroupRisk(t *testing.T) {
	group := groupInput(4, 1)
	paper := group
	paper.VMPolicyMethod = "paper-eq6-7"
	paper.GroupRisk = nil
	paper.Alpha = .8
	if d := Decide(paper, RuntimeSnapshot{}, groupRisk(.1)); d.SpotWorkers != 3 || d.GroupRisk != nil {
		t.Fatal(d)
	}
	if d := Decide(group, RuntimeSnapshot{}, groupRisk(.1)); d.SpotWorkers != 2 {
		t.Fatal(d)
	}
	if d := Decide(paper, RuntimeSnapshot{}, groupRisk(.3)); d.SpotWorkers != 0 {
		t.Fatal(d)
	}
}
