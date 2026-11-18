package management

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestFailedSchedulerRetirementRequiresExactVerifiedWorldAndFence(t *testing.T) {
	for _, kind := range []string{"valid", "unverified", "unfenced", "stale-request", "other-world", "different-pod", "partial-child", "stale-child", "bad-prior", "missing-rank", "duplicate-rank"} {
		t.Run(kind, func(t *testing.T) {
			policy := trainingPolicy()
			req := restoreRequest("group", "restore-uid", 7, "source-node")
			req.SetLabels(map[string]string{p.LabelRole: groupRestoreRole, p.LabelPolicy: policy.GetName(), p.LabelPolicyUID: string(policy.GetUID())})
			pods := []interface{}{map[string]interface{}{"rank": int64(0), "podUID": "old0"}, map[string]interface{}{"rank": int64(1), "podUID": "old1"}, map[string]interface{}{"rank": int64(2), "podUID": "old2"}, map[string]interface{}{"rank": int64(3), "podUID": "old3"}}
			_ = unstructured.SetNestedMap(req.Object, map[string]interface{}{"operationUID": "op-1", "sourceWorldUID": "workload-uid", "worldSize": int64(4), "sourcePods": pods}, "spec", "groupRestore")
			_ = unstructured.SetNestedMap(req.Object, map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}, "spec", "trainingRuntimeRef")
			_ = unstructured.SetNestedField(req.Object, true, "status", "verification", "sourceFenced")
			schedule := p.NewObject("FluidCRMigration")
			schedule.SetNamespace(policy.GetNamespace())
			schedule.SetName("schedule")
			schedule.SetUID("schedule-uid")
			schedule.SetGeneration(1)
			schedule.SetLabels(map[string]string{p.LabelPolicyUID: string(policy.GetUID()), p.LabelRole: checkpointScheduleRole})
			schedule.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}, "schedule": map[string]interface{}{"enabled": false, "intervalSeconds": int64(600)}}
			when := time.Now().UTC().Format(time.RFC3339Nano)
			spec := map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
			status := map[string]interface{}{"phase": "Failed", "observedGeneration": int64(1), "completionTime": when, "pods": runtime.DeepCopyJSONValue(pods)}
			run := map[string]interface{}{"name": "failed", "uid": "failed-child", "checkpointID": "failed-cid", "phase": "Failed", "completionTime": when, "result": map[string]interface{}{"spec": spec, "status": status}}
			schedule.Object["status"] = map[string]interface{}{"clusters": []interface{}{map[string]interface{}{"clusterName": stringField(req.Object, "spec", "sourceCluster"), "observedGeneration": int64(1), "phase": "Paused", "currentRun": run}}}
			switch kind {
			case "unverified":
				_ = unstructured.SetNestedField(req.Object, "Restoring", "status", "phase")
			case "unfenced":
				_ = unstructured.SetNestedField(req.Object, false, "status", "verification", "sourceFence", "fenced")
			case "stale-request":
				_ = unstructured.SetNestedField(req.Object, int64(6), "status", "observedGeneration")
			case "other-world":
				_ = unstructured.SetNestedField(req.Object, "other", "spec", "groupRestore", "sourceWorldUID")
			case "different-pod":
				status["pods"].([]interface{})[1].(map[string]interface{})["podUID"] = "other"
			case "partial-child":
				spec["partialCheckpoint"] = map[string]interface{}{"targetRanks": []interface{}{int64(0)}}
			case "stale-child":
				status["observedGeneration"] = int64(0)
			case "missing-rank":
				status["pods"] = status["pods"].([]interface{})[:3]
			case "duplicate-rank":
				status["pods"].([]interface{})[3].(map[string]interface{})["rank"] = int64(2)
			case "bad-prior":
				schedule.SetAnnotations(map[string]string{verifiedScheduledRetirements: "invalid-json"})
			}
			fixture := recoveryReconciler(t, policy, req, schedule)
			r := &GroupRecoveryReconciler{Client: fixture.Client, Reader: fixture.Client}
			ctx := context.Background()
			err := r.retireFailedScheduledChildren(ctx, policy, req)
			got := p.NewObject("FluidCRMigration")
			if e := r.Get(ctx, client.ObjectKeyFromObject(schedule), got); e != nil {
				t.Fatal(e)
			}
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				var records map[string]scheduledRetirement
				if e := json.Unmarshal([]byte(got.GetAnnotations()[verifiedScheduledRetirements]), &records); e != nil {
					t.Fatal(e)
				}
				v := records["failed-child"]
				if v.RestoreRequestUID != "restore-uid" || v.ChildGeneration != 1 || v.PodUIDs["1"] != "old1" || v.SourceFenceEvidenceID == "" {
					t.Fatalf("invalid retirement %+v", v)
				}
				before := got.GetResourceVersion()
				if e := r.retireFailedScheduledChildren(ctx, policy, req); e != nil {
					t.Fatal(e)
				}
				if e := r.Get(ctx, client.ObjectKeyFromObject(schedule), got); e != nil {
					t.Fatal(e)
				}
				if got.GetResourceVersion() != before {
					t.Fatal("idempotent verification rewrote receipt")
				}
			} else if kind == "bad-prior" {
				if err == nil || got.GetAnnotations()[verifiedScheduledRetirements] != "invalid-json" {
					t.Fatal("invalid existing proof must not be overwritten")
				}
			} else if got.GetAnnotations()[verifiedScheduledRetirements] != "" {
				t.Fatalf("unsafe retirement for %s", kind)
			}

			reports := nestedClusterStatuses(got.Object)
			if len(reports) != 1 || stringField(reports[0], "currentRun", "phase") != "Failed" {
				t.Fatal("failure status was changed to success")
			}
		})
	}
}
