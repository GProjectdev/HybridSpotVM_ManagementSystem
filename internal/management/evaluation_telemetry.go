package management

import (
	"context"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Explicit scalar allow-list: never serialize a policy spec, credentials or
// arbitrary annotations. A calculation observation is not proof of actuation.
func evaluationDecisionFields(actor string, input p.PolicyInput, runtime p.RuntimeSnapshot, risk p.RiskSnapshot, decision p.Decision, now time.Time) []interface{} {
	return []interface{}{
		"decisionActor", actor, "observationSemantics", "policy calculation; reconcile gates and resource events determine actuation",
		"policyUID", string(input.PolicyUID), "policyNamespace", input.Namespace, "policyName", input.PolicyName, "policyGeneration", input.Generation,
		"workloadUID", string(input.WorkloadRef.UID), "riskName", risk.Name, "riskGeneration", risk.Generation, "riskObservedGeneration", risk.ObservedGeneration,
		"riskObservedAt", risk.ObservedAt, "riskValidUntil", risk.ValidUntil, "riskFresh", p.RiskFreshForPolicy(input, risk, now),
		"lambdaPerHour", risk.LambdaPerHour, "riskSource", risk.Source, "alpha", decision.Alpha, "forecastHorizonSeconds", decision.ForecastHorizonSeconds,
		"spotPricePerHour", risk.SpotPricePerHour, "onDemandPricePerHour", risk.OnDemandPricePerHour, "priceEvaluated", decision.PriceEvaluated,
		"method", input.VMPolicyMethod, "vmPolicySource", decision.VMPolicySource, "vmPolicyStage", decision.VMPolicyStage, "decisionStage", decision.DecisionStage,
		"onDemandWorkers", decision.OnDemandWorkers, "spotWorkers", decision.SpotWorkers, "costEvaluated", decision.CostEvaluated, "economicsSource", decision.EconomicsSource,
		"lossCostPerEviction", input.Economics.LossCostPerEviction, "lossCostObservedAt", input.Economics.ObservedAt, "onDemandDurationSeconds", input.Economics.OnDemandDurationSeconds,
		"checkpointIntervalSeconds", decision.CheckpointIntervalSeconds, "intervalSource", decision.IntervalSource, "intervalCostEvaluated", decision.IntervalCostEvaluated,
		"checkpointSeconds", input.Checkpoint.MeasuredCosts.CheckpointSeconds, "copySeconds", input.Checkpoint.MeasuredCosts.CopySeconds, "checkpointCostsObservedAt", input.Checkpoint.MeasuredCosts.ObservedAt,
		"checkpointMinSeconds", input.Checkpoint.MinIntervalSeconds, "checkpointMaxSeconds", input.Checkpoint.MaxIntervalSeconds, "paperAsyncProfileEnabled", input.Checkpoint.Paper.Enabled,
		"iterationTimeSeconds", runtime.IterationTimeSeconds, "iterationObservedAt", runtime.IterationObservedAt, "globalStep", runtime.GlobalStep, "worldSize", runtime.WorldSize, "readyRanks", runtime.ReadyRanks,
		"runtimeReadyForCheckpoint", p.RuntimeReadyForCheckpoint(input, runtime, now), "onDemandHoldUntil", decision.OnDemandHoldUntil, "reason", decision.Reason,
		"decisionObservedAt", now.UTC().Format(time.RFC3339Nano),
	}
}

func logEvaluationDecision(ctx context.Context, actor string, input p.PolicyInput, runtime p.RuntimeSnapshot, risk p.RiskSnapshot, decision p.Decision, now time.Time) {
	ctrl.LoggerFrom(ctx).Info("EvaluationPolicyDecision", evaluationDecisionFields(actor, input, runtime, risk, decision, now)...)
}
