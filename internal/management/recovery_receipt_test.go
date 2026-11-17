package management

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
)

func TestCompletedCleanupReceiptSurvivesPolicyChanges(t *testing.T) {
	rec := recovery("recover")
	addFullRecoverySpec(rec)
	rec.SetGeneration(1)
	rec.Object["status"] = map[string]interface{}{"phase": "Completed", "observedGeneration": int64(1), "verifiedCheckpointID": "ckpt-1"}
	policy := trainingPolicy()
	policy.SetGeneration(policy.GetGeneration() + 1)
	old := nodeProvision("old", "different-new-uid", "policy-uid", "op-1", "aws", "Ready")
	r := recoveryReconciler(t, rec, policy, old)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "recover"}}); err != nil {
		t.Fatal(err)
	}
	got := p.NewObject("SpotRecovery")
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(rec), got)
	if stringField(got.Object, "status", "phase") != "Completed" {
		t.Fatal("historical receipt overwritten")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(old), p.NewObject("NodeProvision")); err != nil {
		t.Fatal("terminal receipt actuated a deletion", err)
	}
}
func TestHistoricalCleanupReconstructionRequiresCompleteIndependentReceipts(t *testing.T) {
	for _, fault := range []string{"none", "old-exists", "wrong-target-uid", "restore-not-verified", "operation-not-complete", "policy-recreated", "stale-operation"} {
		t.Run(fault, func(t *testing.T) {
			rec := recovery("recover")
			addFullRecoverySpec(rec)
			policy := trainingPolicy()
			policy.SetGeneration(policy.GetGeneration() + 1)
			rt := object("TrainingRuntime", "default", "runtime", "runtime-uid")
			rt.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
			op := spotReplacement()
			op.SetGeneration(1)
			_ = unstructured.SetNestedField(op.Object, "Completed", "status", "phase")
			_ = unstructured.SetNestedField(op.Object, int64(1), "status", "observedGeneration")
			_ = unstructured.SetNestedField(op.Object, "restore-uid", "status", "restoreRequestRef", "uid")
			restore := restoreRequest("restore", "restore-uid", 7, "source-node")
			target := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")
			objects := []client.Object{rec, policy, rt, op, restore, target}
			switch fault {
			case "old-exists":
				objects = append(objects, nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready"))
			case "wrong-target-uid":
				target.SetUID("recreated")
			case "restore-not-verified":
				_ = unstructured.SetNestedField(restore.Object, "Pending", "status", "phase")
			case "operation-not-complete":
				_ = unstructured.SetNestedField(op.Object, "Pending", "status", "phase")
			case "policy-recreated":
				policy.SetUID("new-policy")
			case "stale-operation":
				_ = unstructured.SetNestedField(op.Object, int64(2), "status", "observedGeneration")
			}
			r := recoveryReconciler(t, objects...)
			phase, err := r.recover(context.Background(), rec)
			if fault == "none" {
				if err != nil || phase != "Completed" {
					t.Fatalf("%s %v", phase, err)
				}
			} else if err == nil || phase == "Completed" {
				t.Fatalf("unsafe historical completion accepted: %s %v", phase, err)
			}
		})
	}
}
