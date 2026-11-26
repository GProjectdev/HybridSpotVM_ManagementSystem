package framework

import (
	"context"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (r *FrameworkCheckpointReconciler) cloudInterval(ctx context.Context, run, work, schedule *unstructured.Unstructured, s RunSpec, o WorkloadObservation, now time.Time) (int64, map[string]interface{}, error) {
	cap := named("TrainingCapacity", run.GetNamespace(), initialCapacityName(run))
	if err := r.Get(ctx, key(run.GetNamespace(), cap.GetName()), cap); err != nil {
		return 0, nil, err
	}
	if !owned(cap, run) || string(cap.GetUID()) != str(run, "status", "initialCapacityUID") || str(cap, "spec", "runRef", "uid") != string(run.GetUID()) {
		return 0, nil, fmt.Errorf("current capacity identity not verified")
	}
	var cs CapacitySpec
	if decodeSpec(cap, &cs) != nil {
		return 0, nil, fmt.Errorf("invalid capacity")
	}
	spot := int64(0)
	for _, a := range cs.Allocations {
		if a.State == "Retire" {
			continue
		}
		if a.Market == "spot" {
			spot++
		}
	}
	cp := p.CheckpointPolicy{Resume: true, MinIntervalSeconds: 1, MaxIntervalSeconds: min(s.CheckpointSeconds, MaximumCheckpointSeconds)}
	reader, err := r.Members(o.SelectedCluster)
	if err != nil {
		return 0, nil, err
	}
	costs, measured, costReason := latestCheckpointCosts(ctx, reader, schedule, run, work, o, now, p.CheckpointCostMaxAge(cp))
	if measured {
		cp.MeasuredCosts = costs
	}
	meta := map[string]interface{}{"actualSpotWorkers": spot, "costSource": costReason, "costMeasured": measured}
	if measured {
		meta["measuredCosts"] = encode(costs)
	}
	if spot == 0 {
		meta["intervalSource"] = "user-interval-no-spot"
		return cp.MaxIntervalSeconds, meta, nil
	}
	d, receipt, err := initialCloudDecision(ctx, r.Client, run, s, now)
	if err != nil {
		// Unknown risk never means zero and must not disable periodic saving.
		// Reuse the established policy's explicitly labelled unknown-risk fallback.
		fallback := p.DecideAt(p.PolicyInput{TargetWorkers: s.WorldSize, Checkpoint: cp}, o.Runtime, p.RiskSnapshot{}, now)
		meta["intervalSource"] = "risk-unavailable-conservative-fallback"
		meta["riskError"] = err.Error()
		return fallback.CheckpointIntervalSeconds, meta, nil
	}
	interval, evaluated, source := p.CheckpointIntervalDecision(cp, p.RiskSnapshot{Ready: true, LambdaPerHour: d.LambdaPerHour}, o.Runtime, spot, now)
	meta["riskEvidence"], meta["intervalSource"], meta["intervalCostEvaluated"] = receipt, source, evaluated
	interval, err = CheckpointInterval(s.CheckpointSeconds, interval, true, true)
	return interval, meta, err
}
