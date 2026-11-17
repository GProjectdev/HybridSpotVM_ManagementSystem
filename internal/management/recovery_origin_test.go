package management

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSuccessiveReplacementRetirementRequiresOriginAndLiveWorld(t *testing.T) {
	for _, fault := range []string{"none", "wrong-uid", "wrong-policy", "wrong-world", "wrong-cluster", "pending-origin", "stale-origin", "unavailable-world", "stale-world", "recreated-runtime"} {
		t.Run(fault, func(t *testing.T) {
			rec := recovery("recover")
			addFullRecoverySpec(rec)
			s := readRecoverySpec(rec)
			s.TargetCluster = "aws"
			np := nodeProvision("old", "old-uid", "policy-uid", "previous", "aws", "Ready")
			_ = unstructured.SetNestedField(np.Object, "source-node", "spec", "hostname")
			setAtRiskSpot(np)
			op := spotReplacement()
			op.SetName("previous")
			op.SetGeneration(1)
			for _, path := range [][]string{{"spec", "operation"}, {"spec", "replacementNodeProvisionRef", "name"}, {"status", "replacementNodeProvisionRef", "name"}, {"status", "replacementNodeProvisionRef", "uid"}, {"spec", "targetCluster"}} {
				value := map[string]string{"operation": "previous", "name": "old", "uid": "old-uid", "targetCluster": "aws"}[path[len(path)-1]]
				_ = unstructured.SetNestedField(op.Object, value, path...)
			}
			_ = unstructured.SetNestedField(op.Object, "Completed", "status", "phase")
			_ = unstructured.SetNestedField(op.Object, int64(1), "status", "observedGeneration")
			rt := object("TrainingRuntime", "default", "runtime", "runtime-uid")
			rt.SetGeneration(1)
			_ = unstructured.SetNestedField(rt.Object, int64(2), "spec", "expectedWorldSize")
			status := map[string]interface{}{"phase": "Running", "readyRanks": int64(2), "observedGeneration": int64(1), "sourceWorldUID": s.WorkloadUID, "observedAt": time.Now().UTC().Format(time.RFC3339Nano)}
			switch fault {
			case "wrong-uid":
				_ = unstructured.SetNestedField(op.Object, "recreated", "status", "replacementNodeProvisionRef", "uid")
			case "wrong-policy":
				_ = unstructured.SetNestedField(op.Object, "other", "spec", "policyRef", "uid")
			case "wrong-world":
				_ = unstructured.SetNestedField(op.Object, "other", "spec", "workloadRef", "uid")
			case "wrong-cluster":
				_ = unstructured.SetNestedField(op.Object, "other", "spec", "targetCluster")
			case "pending-origin":
				_ = unstructured.SetNestedField(op.Object, "Pending", "status", "phase")
			case "stale-origin":
				_ = unstructured.SetNestedField(op.Object, int64(2), "status", "observedGeneration")
			case "unavailable-world":
				status["phase"] = "Unavailable"
			case "stale-world":
				status["observedAt"] = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
			case "recreated-runtime":
				rt.SetUID("other")
			}
			_ = unstructured.SetNestedSlice(rt.Object, []interface{}{map[string]interface{}{"clusterName": "aws", "status": status}}, "status", "clusters")
			r := recoveryReconciler(t, op, rt)
			r.Clock = time.Now
			err := r.verifyOldNodeOrigin(context.Background(), np, s, restoreEvidence{SourceNodes: map[string]bool{"source-node": true}})
			if (err == nil) != (fault == "none") {
				t.Fatalf("fault=%s err=%v", fault, err)
			}
			if np.GetAnnotations()["training.dcnlab.com/recovery-operation"] != "previous" {
				t.Fatal("creation annotation mutated")
			}
		})
	}
}
