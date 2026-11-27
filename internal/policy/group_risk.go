package policy

import (
	"fmt"
	"math"
	"time"
)

const GroupRiskMethod = "group-risk-v1"
const IndependentPerInstanceHazard = "per-instance-independent"

// GroupRiskPolicy is an explicit modelling contract, not proof that an
// availability proxy is a measured VM hazard. Q is not Desai's throughput alpha.
type GroupRiskPolicy struct {
	MinimumSurvivalProbability float64 `json:"minimumSurvivalProbability"`
	HazardBasis                string  `json:"hazardBasis"`
}

type GroupRiskCandidate struct {
	SpotWorkers         int64   `json:"spotWorkers"`
	OnDemandWorkers     int64   `json:"onDemandWorkers"`
	SurvivalProbability float64 `json:"survivalProbability"`
	RiskEligible        bool    `json:"riskEligible"`
}

type GroupRiskDecision struct {
	MinimumSurvivalProbability float64              `json:"minimumSurvivalProbability"`
	HazardBasis                string               `json:"hazardBasis"`
	GroupLambdaPerHour         float64              `json:"groupLambdaPerHour"`
	SurvivalProbability        float64              `json:"survivalProbability"`
	Candidates                 []GroupRiskCandidate `json:"candidates"`
}

func validateGroupRisk(input PolicyInput) error {
	if input.VMPolicyMethod != GroupRiskMethod {
		if input.GroupRisk != nil {
			return fmt.Errorf("groupRisk requires method %s", GroupRiskMethod)
		}
		return nil
	}
	g := input.GroupRisk
	if g == nil || !finite(g.MinimumSurvivalProbability) || g.MinimumSurvivalProbability <= 0 || g.MinimumSurvivalProbability > 1 || g.HazardBasis != IndependentPerInstanceHazard {
		return fmt.Errorf("group-risk-v1 requires explicit q in (0,1] and per-instance-independent hazard basis")
	}
	if input.TargetWorkers < 1 || input.TargetWorkers > 128 || input.MinOnDemand < 0 || input.MinOnDemand > input.TargetWorkers || input.ForecastSeconds < 1 || input.ForecastSeconds > 86400 || input.FixedOnDemand != nil {
		return fmt.Errorf("invalid group capacity/horizon or conflicting fixed composition")
	}
	return nil
}

// decideGroupRisk selects the largest risk-eligible Spot count if Spot is cheaper.
// This is a group-risk constraint, NOT a throughput or expected-cost optimizer.
// In particular, do not run the legacy per-VM economic fallback a second time.
func decideGroupRisk(input PolicyInput, runtime RuntimeSnapshot, risk RiskSnapshot, now time.Time) Decision {
	d := Decision{DesiredWorkers: input.TargetWorkers, Alpha: clampAlpha(input.Alpha),
		ForecastHorizonSeconds: input.ForecastSeconds, LambdaPerHour: risk.LambdaPerHour,
		VMPolicySource: "independent-poisson-group-risk-v1", VMPolicyStage: "GroupRiskOnly",
		DecisionStage: "GroupRiskOnly", Reason: "group_survival_constraint"}
	if !finite(risk.SpotPricePerHour) || risk.SpotPricePerHour <= 0 || !finite(risk.OnDemandPricePerHour) || risk.OnDemandPricePerHour <= 0 {
		d.ProvisioningBlocked, d.Reason = true, "group_prices_unavailable"
		return d
	}
	d.PriceEvaluated = true
	hours := float64(input.ForecastSeconds) / 3600
	if !finite(float64(input.TargetWorkers)*risk.LambdaPerHour*hours) || !finite(float64(input.TargetWorkers)*risk.LambdaPerHour) {
		d.ProvisioningBlocked, d.Reason = true, "invalid_group_hazard"
		return d
	}
	g := &GroupRiskDecision{MinimumSurvivalProbability: input.GroupRisk.MinimumSurvivalProbability, HazardBasis: input.GroupRisk.HazardBasis}
	for spot := int64(0); spot <= input.TargetWorkers-input.MinOnDemand; spot++ {
		exposure := float64(spot) * risk.LambdaPerHour * hours
		survival := math.Exp(-exposure)
		// Compare in log space: exp(-tiny) can round to 1. A q=1 contract
		// must not accept any positive Spot hazard, including subnormals.
		eligible := exposure <= -math.Log(g.MinimumSurvivalProbability)
		if g.MinimumSurvivalProbability == 1 && spot > 0 && risk.LambdaPerHour > 0 {
			eligible = false
		}
		g.Candidates = append(g.Candidates, GroupRiskCandidate{spot, input.TargetWorkers - spot, survival, eligible})
		if eligible {
			d.SpotWorkers = spot
		}
	}
	if risk.SpotPricePerHour >= risk.OnDemandPricePerHour {
		d.SpotWorkers, d.Reason = 0, "spot_price_not_cheaper"
	}
	d.OnDemandWorkers = input.TargetWorkers - d.SpotWorkers
	g.GroupLambdaPerHour = float64(d.SpotWorkers) * risk.LambdaPerHour
	g.SurvivalProbability = math.Exp(-g.GroupLambdaPerHour * hours)
	d.GroupRisk = g
	d.CheckpointIntervalSeconds, d.IntervalCostEvaluated, d.IntervalSource = CheckpointIntervalDecision(input.Checkpoint, risk, runtime, d.SpotWorkers, now)
	return d
}
