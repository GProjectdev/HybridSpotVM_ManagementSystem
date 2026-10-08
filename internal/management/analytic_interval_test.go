package management

import (
	"context"
	"reflect"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAnalyticIntervalUsesFreshEvidenceInOnePassAndUpdatesSameSchedule(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:03:30Z")
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{name: "fresh-but-different-spec-cost", age: time.Minute},
		{name: "stale-spec-cost", age: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			policy := checkpointPolicyFixture(now)
			policy.Object["spec"].(map[string]interface{})["policy"] = map[string]interface{}{"forecastHorizonSeconds": int64(1)}
			checkpoint := policy.Object["spec"].(map[string]interface{})["checkpoint"].(map[string]interface{})
			checkpoint["measuredCosts"] = map[string]interface{}{
				"checkpointSeconds": float64(1), "copySeconds": float64(0), "observedAt": now.Add(-tc.age).Format(time.RFC3339),
			}
			input := p.ReadPolicyInput(policy)
			runtimeObj := runtimeFixture(now)
			risk := riskFixture(now)
			risk.Object["status"].(map[string]interface{})["lambdaPerHour"] = float64(20)

			measured := measuredCostMigration()
			measured.SetName("verified-round")
			measured.SetUID("verified-round-uid")
			measured.SetNamespace(input.Namespace)
			measured.SetLabels(map[string]string{p.LabelPolicyUID: string(input.PolicyUID), p.LabelRole: "checkpoint"})
			pods := sourceStatus(t, measured)["pods"].([]interface{})
			for _, pod := range pods {
				for _, item := range pod.(map[string]interface{})["checkpointFiles"].([]interface{}) {
					file := item.(map[string]interface{})
					file["durableRef"] = "file-store:" + input.Namespace + "/sha256/" + file["sha256"].(string)
				}
			}
			// The coordinator consumes flattened source-cluster reports.
			report := sourceStatus(t, measured)
			report["clusterName"] = input.SourceCluster
			measured.Object["status"] = map[string]interface{}{"clusters": []interface{}{report}}
			if !isTerminalPhase(measured, input.SourceCluster) {
				t.Fatal("verified-cost fixture must be terminal before interval selection")
			}
			costs, valid, reason := measuredCostsFromMigration(measured, input.SourceCluster, now)
			if !valid || costs.CheckpointSeconds != 90 || costs.CopySeconds != 30 {
				t.Fatalf("invalid verified-cost fixture: costs=%+v valid=%v reason=%s", costs, valid, reason)
			}

			// Seed a server-assigned identity before reconciliation; never rewrite a live UID.
			schedule := checkpointScheduleFixture(input, true)
			schedule.Object["spec"].(map[string]interface{})["ctrlPort"] = int64(8298)
			r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeObj, risk, measured, schedule, workloadFixture("workload-uid"))
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
			var first *unstructured.Unstructured
			for _, step := range []struct {
				lambda float64
				want   int64
			}{{20, 180}, {80, 90}, {0, 600}} {
				if first != nil {
					if err := r.Get(ctx, client.ObjectKeyFromObject(risk), risk); err != nil {
						t.Fatal(err)
					}
					risk.Object["status"].(map[string]interface{})["lambdaPerHour"] = step.lambda
					if err := r.Update(ctx, risk); err != nil {
						t.Fatal(err)
					}
				}
				result, err := r.Reconcile(ctx, req)
				if err != nil {
					t.Fatalf("lambda=%v: reconcile: %v", step.lambda, err)
				}
				got := p.NewObject("FluidCRMigration")
				if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), got); err != nil {
					t.Fatal(err)
				}
				if got.GetUID() != schedule.GetUID() || got.GetUID() == "" || got.GetName() != schedule.GetName() {
					t.Fatalf("schedule identity changed: %s/%s", got.GetName(), got.GetUID())
				}
				if interval := intField(got.Object, "spec", "schedule", "intervalSeconds"); interval != step.want {
					t.Fatalf("lambda=%v: interval=%d, want %d from fresh costs, independent of candidate [60]", step.lambda, interval, step.want)
				}
				if !boolField(got.Object, "spec", "schedule", "enabled") || result.RequeueAfter != time.Duration(step.want)*time.Second {
					t.Fatalf("schedule disabled or requeue mismatch: result=%+v schedule=%v", result, got.Object["spec"])
				}
				if first != nil {
					beforeSpec, _, _ := unstructured.NestedMap(first.Object, "spec")
					afterSpec, _, _ := unstructured.NestedMap(got.Object, "spec")
					delete(beforeSpec, "schedule")
					delete(afterSpec, "schedule")
					if !reflect.DeepEqual(beforeSpec, afterSpec) || !reflect.DeepEqual(first.GetOwnerReferences(), got.GetOwnerReferences()) || !reflect.DeepEqual(first.GetLabels(), got.GetLabels()) {
						t.Fatal("interval update changed immutable spec or ownership")
					}
				} else {
					first = got.DeepCopy()
				}
				updated := p.NewObject("TrainingPolicy")
				if err := r.Get(ctx, req.NamespacedName, updated); err != nil {
					t.Fatal(err)
				}
				if stringField(updated.Object, "status", "checkpoint", "intervalSource") != "checkpoint-cost-analytic" || !boolField(updated.Object, "status", "checkpoint", "intervalCostEvaluated") || intField(updated.Object, "status", "checkpoint", "checkpointIntervalSeconds") != step.want {
					t.Fatalf("missing analytical decision provenance: %v", updated.Object["status"])
				}
				if gotCosts := p.ReadPolicyInput(updated).Checkpoint.MeasuredCosts; gotCosts != costs {
					t.Fatalf("published costs=%+v, want verified costs=%+v", gotCosts, costs)
				}
				list := p.NewList("FluidCRMigration")
				if err := r.List(ctx, list, client.InNamespace(input.Namespace)); err != nil {
					t.Fatal(err)
				}
				if len(list.Items) != 2 {
					t.Fatalf("migration count=%d, want only original evidence and schedule", len(list.Items))
				}
				for _, item := range list.Items {
					if item.GetName() != schedule.GetName() && (item.GetName() != measured.GetName() || item.GetUID() != measured.GetUID()) {
						t.Fatalf("unexpected child or replaced evidence: %s/%s", item.GetName(), item.GetUID())
					}
				}
			}
		})
	}
}

func TestAnalyticIntervalPaperWithoutMeasurementsUsesBootstrap(t *testing.T) {
	now := mustParseTime(t, "2026-09-26T00:03:30Z")
	policy := checkpointPolicyFixture(now)
	checkpoint := policy.Object["spec"].(map[string]interface{})["checkpoint"].(map[string]interface{})
	checkpoint["paperProfile"] = map[string]interface{}{"enabled": true}
	r := checkpointReconcilerFixture(t, func() time.Time { return now }, policy, runtimeFixture(now), riskFixture(now), workloadFixture("workload-uid"))
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	schedule := getOnlyMigration(t, r.Client)
	if schedule.GetName() != "train-periodic" || intField(schedule.Object, "spec", "schedule", "intervalSeconds") != 60 || !boolField(schedule.Object, "spec", "schedule", "enabled") {
		t.Fatalf("missing bounded bootstrap schedule: %v", schedule.Object)
	}
	if err := r.Get(context.Background(), req.NamespacedName, policy); err != nil {
		t.Fatal(err)
	}
	if stringField(policy.Object, "status", "checkpoint", "intervalSource") != "risk-band-bootstrap" || boolField(policy.Object, "status", "checkpoint", "intervalCostEvaluated") {
		t.Fatalf("missing paper inputs were reported as measured: %v", policy.Object["status"])
	}
}
