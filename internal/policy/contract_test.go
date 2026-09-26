package policy

import (
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func TestDecideAllowsAllSpotWhenLambdaZeroAndMinOnDemandDefaultZero(t *testing.T) {
	decision := Decide(PolicyInput{TargetWorkers: 4, Alpha: 0.8, ForecastSeconds: 3600}, RuntimeSnapshot{}, RiskSnapshot{Ready: true, LambdaPerHour: 0})

	if decision.OnDemandWorkers != 0 {
		t.Fatalf("on-demand workers = %d, want 0", decision.OnDemandWorkers)
	}
	if decision.SpotWorkers != 4 {
		t.Fatalf("spot workers = %d, want 4", decision.SpotWorkers)
	}
}

func TestDecideRiskUnavailableIsNotUnknownZero(t *testing.T) {
	decision := Decide(PolicyInput{TargetWorkers: 3, Alpha: 0.8, ForecastSeconds: 3600}, RuntimeSnapshot{}, RiskSnapshot{})

	if decision.Reason != "risk_unavailable" {
		t.Fatalf("reason = %q, want risk_unavailable", decision.Reason)
	}
	if !decision.ProvisioningBlocked {
		t.Fatal("provisioning not blocked for missing risk")
	}
	if decision.OnDemandWorkers != 0 || decision.SpotWorkers != 0 {
		t.Fatalf("workers = OD %d Spot %d, want no provisioning plan", decision.OnDemandWorkers, decision.SpotWorkers)
	}
	if decision.CostEvaluated {
		t.Fatal("cost evaluated with missing prices")
	}
}

func TestReadRiskStatusUsesNestedPrices(t *testing.T) {
	risk := NewObject("SpotRiskProfile")
	risk.SetGeneration(7)
	risk.Object["status"] = map[string]interface{}{
		"observedGeneration": int64(7),
		"validUntil":         "2026-09-26T00:05:00Z",
		"ready":              true,
		"lambdaPerHour":      float64(0.04),
		"prices": map[string]interface{}{
			"spotPricePerHour":     float64(0.12),
			"onDemandPricePerHour": float64(0.48),
		},
	}
	snapshot := ReadRiskStatus(risk)

	if snapshot.SpotPricePerHour != 0.12 || snapshot.OnDemandPricePerHour != 0.48 {
		t.Fatalf("prices = %#v, want nested status.prices values", snapshot)
	}
	if !RiskFreshForPolicy(PolicyInput{Generation: 99}, snapshot, mustTime(t, "2026-09-26T00:00:00Z")) {
		t.Fatal("risk freshness incorrectly depended on TrainingPolicy generation")
	}
	snapshot.ObservedGeneration = 6
	if RiskFreshForPolicy(PolicyInput{Generation: 7}, snapshot, mustTime(t, "2026-09-26T00:00:00Z")) {
		t.Fatal("risk freshness accepted stale SpotRiskProfile observedGeneration")
	}
}

func TestReadRuntimeStatusSelectsClusterStatusShape(t *testing.T) {
	runtime := NewObject("TrainingRuntime")
	runtime.SetGeneration(5)
	runtime.Object["status"] = map[string]interface{}{
		"clusters": []interface{}{
			map[string]interface{}{"clusterName": "onprem", "status": map[string]interface{}{"worldSize": int64(1)}},
			map[string]interface{}{"clusterName": "aws", "status": map[string]interface{}{
				"observedGeneration": int64(5),
				"observedAt":         "2026-09-26T00:00:00Z",
				"phase":              "Running",
				"workloadUID":        "workload-uid",
				"memberWorkloadUID":  "member-workload-uid",
				"readyRanks":         int64(2),
				"worldSize":          int64(2),
				"pods": []interface{}{
					map[string]interface{}{"name": "rank-0", "uid": "pod-0", "rank": int64(0), "checkpointID": "ckpt-1", "observedAt": "2026-09-26T00:00:00Z"},
				},
			}},
		},
	}

	snapshot := ReadRuntimeStatus(runtime, "aws")
	if snapshot.WorldSize != 2 || snapshot.ReadyRanks != 2 {
		t.Fatalf("runtime snapshot = %#v, want aws member status", snapshot)
	}
	if len(snapshot.Pods) != 1 || snapshot.Pods[0].UID != "pod-0" {
		t.Fatalf("pods = %#v, want flattened member pods", snapshot.Pods)
	}
}

func TestRuntimeReadyUsesRuntimeGenerationNotPolicyGeneration(t *testing.T) {
	now := mustTime(t, "2026-09-26T00:00:30Z")
	input := PolicyInput{Generation: 99, TargetWorkers: 2, WorkloadRef: WorkloadRef{UID: types.UID("workload-uid")}}
	runtime := RuntimeSnapshot{Generation: 5, ObservedGeneration: 5, WorkloadUID: types.UID("workload-uid"), StatusWorkloadUID: types.UID("workload-uid"), MemberWorkloadUID: types.UID("member-workload-uid"), Phase: "Running", ReadyRanks: 2, WorldSize: 2, ObservedAt: "2026-09-26T00:00:00Z", Pods: []PodRuntime{{Name: "rank-0", UID: "pod-0", Rank: 0, CheckpointID: "", ObservedAt: "2026-09-26T00:00:00Z"}, {Name: "rank-1", UID: "pod-1", Rank: 1, CheckpointID: "", ObservedAt: "2026-09-26T00:00:00Z"}}}
	if !RuntimeReadyForCheckpoint(input, runtime, now) {
		t.Fatal("runtime readiness incorrectly rejected member UID mismatch or initial empty checkpoint IDs")
	}
	runtime.ObservedGeneration = 4
	if RuntimeReadyForCheckpoint(input, runtime, now) {
		t.Fatal("runtime readiness accepted stale TrainingRuntime observedGeneration")
	}
	runtime.ObservedGeneration = 5
	runtime.MemberWorkloadUID = ""
	if RuntimeReadyForCheckpoint(input, runtime, now) {
		t.Fatal("runtime readiness accepted missing member workload UID")
	}
	runtime.MemberWorkloadUID = types.UID("member-workload-uid")
	runtime.Pods[1].Rank = 0
	if RuntimeReadyForCheckpoint(input, runtime, now) {
		t.Fatal("runtime readiness accepted duplicate rank evidence")
	}
	runtime.Pods[1].Rank = 1
	runtime.Pods[1].CheckpointID = "ckpt-1"
	if RuntimeReadyForCheckpoint(input, runtime, now) {
		t.Fatal("runtime readiness accepted mixed checkpoint IDs")
	}
}

func TestAdaptiveIntervalCostEvaluationSeparateFromSpotODCost(t *testing.T) {
	now := mustTime(t, "2026-09-26T00:10:00Z")
	checkpoint := CheckpointPolicy{CandidateIntervals: []int64{30, 60, 120}, MeasuredCosts: MeasuredCosts{CheckpointSeconds: 12, CopySeconds: 4, ObservedAt: "2026-09-26T00:09:00Z"}}
	interval, evaluated := AdaptiveCheckpointInterval(checkpoint, RiskSnapshot{Ready: true, LambdaPerHour: 0.05}, RuntimeSnapshot{}, 2, now)
	if !evaluated || interval == 0 {
		t.Fatalf("interval = %d evaluated = %v, want measured interval objective", interval, evaluated)
	}
	decision := Decide(PolicyInput{TargetWorkers: 2, Alpha: 0.8, ForecastSeconds: 3600, Checkpoint: checkpoint}, RuntimeSnapshot{}, RiskSnapshot{Ready: true, LambdaPerHour: 0.05})
	if decision.CostEvaluated {
		t.Fatal("Spot/OD cost evaluated without expectedLossCost/expectedODUseSeconds implementation")
	}
}

func TestAdaptiveIntervalMeasuredCostRejectsInvalidObservations(t *testing.T) {
	now := mustTime(t, "2026-09-26T00:10:00Z")
	base := CheckpointPolicy{CandidateIntervals: []int64{30, 60}, MeasuredCosts: MeasuredCosts{CheckpointSeconds: 12, CopySeconds: 0, ObservedAt: "2026-09-26T00:09:00Z"}}
	if _, evaluated := AdaptiveCheckpointInterval(base, RiskSnapshot{Ready: true, LambdaPerHour: 0.05}, RuntimeSnapshot{}, 2, now); !evaluated {
		t.Fatal("zero copySeconds should be a valid measured objective value")
	}

	cases := map[string]MeasuredCosts{
		"future":               {CheckpointSeconds: 12, CopySeconds: 1, ObservedAt: "2026-09-26T00:11:00Z"},
		"zero-checkpoint":      {CheckpointSeconds: 0, CopySeconds: 1, ObservedAt: "2026-09-26T00:09:00Z"},
		"negative-copy":        {CheckpointSeconds: 12, CopySeconds: -1, ObservedAt: "2026-09-26T00:09:00Z"},
		"nonfinite-checkpoint": {CheckpointSeconds: math.Inf(1), CopySeconds: 1, ObservedAt: "2026-09-26T00:09:00Z"},
		"nonfinite-copy":       {CheckpointSeconds: 12, CopySeconds: math.NaN(), ObservedAt: "2026-09-26T00:09:00Z"},
	}
	for name, measured := range cases {
		checkpoint := base
		checkpoint.MeasuredCosts = measured
		if _, evaluated := AdaptiveCheckpointInterval(checkpoint, RiskSnapshot{Ready: true, LambdaPerHour: 0.05}, RuntimeSnapshot{}, 2, now); evaluated {
			t.Fatalf("%s measured costs evaluated, want fallback", name)
		}
	}
}

func TestAdaptiveIntervalMeasuredCandidatesRespectBounds(t *testing.T) {
	now := mustTime(t, "2026-09-26T00:10:00Z")
	checkpoint := CheckpointPolicy{MinIntervalSeconds: 30, MaxIntervalSeconds: 300, CandidateIntervals: []int64{0, 10, 600}, MeasuredCosts: MeasuredCosts{CheckpointSeconds: 12, CopySeconds: 0, ObservedAt: "2026-09-26T00:09:00Z"}}
	interval, evaluated := AdaptiveCheckpointInterval(checkpoint, RiskSnapshot{Ready: true, LambdaPerHour: 0.05}, RuntimeSnapshot{}, 2, now)
	if !evaluated {
		t.Fatal("measured interval not evaluated")
	}
	if interval < checkpoint.MinIntervalSeconds || interval > checkpoint.MaxIntervalSeconds {
		t.Fatalf("interval = %d outside [%d,%d]", interval, checkpoint.MinIntervalSeconds, checkpoint.MaxIntervalSeconds)
	}
}

func TestNewFluidCRMigrationPinsDeterministicCheckpointID(t *testing.T) {
	input := PolicyInput{
		Namespace:  "default",
		PolicyName: "train",
		PolicyUID:  types.UID("policy-uid"),
		WorkloadRef: WorkloadRef{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
			Name:       "trainer",
			UID:        types.UID("workload-uid"),
		},
		Checkpoint: CheckpointPolicy{Resume: true},
	}
	migration := NewFluidCRMigration(input, RuntimeSnapshot{Port: 8298, Container: "main"}, mustTime(t, "2026-09-26T00:10:00Z"), 600)

	if got := migration.GetLabels()[LabelPolicyUID]; got != "policy-uid" {
		t.Fatalf("policy uid label = %q", got)
	}
	if got := migration.GetAnnotations()["training.dcnlab.com/checkpoint-id"]; got != migration.GetName() {
		t.Fatalf("checkpoint id annotation = %q, want migration name %q", got, migration.GetName())
	}
	resume, _, _ := unstructured.NestedBool(migration.Object, "spec", "resume")
	if !resume {
		t.Fatal("resume = false, want true")
	}
	uid, _, _ := unstructured.NestedString(migration.Object, "spec", "workloadRef", "uid")
	if uid != "workload-uid" {
		t.Fatalf("workload uid = %q, want workload-uid", uid)
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
