package management

import (
	"context"
	"strings"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCleanupReceiptExactParticipants(t *testing.T) {
	for _, name := range []string{"valid-reordered", "missing-target", "duplicate-target", "unexpected-target", "missing-rank", "changed-survivor-uid", "duplicate-survivor", "missing-survivor", "unexpected-survivor", "duplicate-requested-target", "mismatched-restore-target", "duplicate-preserved-survivor"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			recoveryObj := recovery("recover")
			addFullRecoverySpec(recoveryObj)
			_ = unstructured.SetNestedField(recoveryObj.Object, "aws", "spec", "targetCluster")
			restore := restoreRequest("restore", "restore-uid", 7, "source-node")
			addSameClusterRestoreEvidence(restore)
			operation := spotReplacement()
			_ = unstructured.SetNestedField(operation.Object, "aws", "spec", "targetCluster")
			ranks := []interface{}{int64(0), int64(2)}
			_ = unstructured.SetNestedSlice(operation.Object, ranks, "spec", "partialCheckpoint", "targetRanks")
			_ = unstructured.SetNestedSlice(operation.Object, ranks, "spec", "partialRestore", "targetRanks")
			_ = unstructured.SetNestedSlice(restore.Object, ranks, "spec", "partialRestore", "targetRanks")
			pods, _, _ := unstructured.NestedSlice(operation.Object, "spec", "pods")
			pods = append(pods, map[string]interface{}{"rank": int64(2), "sourcePodUID": "old-pod-2", "sourcePodName": "trainer-2"})
			_ = unstructured.SetNestedSlice(operation.Object, pods, "spec", "pods")
			targets, _, _ := unstructured.NestedSlice(restore.Object, "status", "verification", "partialRestore", "targetRanks")
			second := deepCopyMap(targets[0].(map[string]interface{}))
			second["rank"], second["targetPodUID"] = int64(2), "target-pod-2"
			targets = []interface{}{second, targets[0]}
			survivors, _, _ := unstructured.NestedSlice(restore.Object, "status", "verification", "survivors")
			survivor2 := deepCopyMap(survivors[0].(map[string]interface{}))
			survivor2["rank"], survivor2["podUID"] = int64(3), "survivor-pod-3"
			baseline := append(survivors, survivor2)
			_ = unstructured.SetNestedSlice(operation.Object, baseline, "spec", "partialRestore", "preservedSurvivors")
			survivors = []interface{}{survivor2, survivors[0]}
			switch name {
			case "missing-target":
				targets = targets[:1]
			case "duplicate-target":
				targets = []interface{}{targets[0], targets[0]}
			case "unexpected-target":
				targets[0].(map[string]interface{})["rank"] = int64(4)
			case "missing-rank":
				delete(targets[1].(map[string]interface{}), "rank")
			case "changed-survivor-uid":
				survivors[0].(map[string]interface{})["podUID"] = "recreated"
			case "duplicate-survivor":
				survivors = []interface{}{survivors[0], survivors[0]}
			case "missing-survivor":
				survivors = survivors[:1]
			case "unexpected-survivor":
				survivors[0].(map[string]interface{})["rank"] = int64(4)
			case "duplicate-requested-target":
				_ = unstructured.SetNestedSlice(operation.Object, []interface{}{int64(0), int64(0)}, "spec", "partialCheckpoint", "targetRanks")
			case "mismatched-restore-target":
				_ = unstructured.SetNestedSlice(operation.Object, []interface{}{int64(0), int64(4)}, "spec", "partialRestore", "targetRanks")
			case "duplicate-preserved-survivor":
				_ = unstructured.SetNestedSlice(operation.Object, []interface{}{baseline[0], baseline[0]}, "spec", "partialRestore", "preservedSurvivors")
			}
			_ = unstructured.SetNestedSlice(restore.Object, targets, "status", "verification", "partialRestore", "targetRanks")
			_ = unstructured.SetNestedSlice(restore.Object, survivors, "status", "verification", "survivors")
			runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
			runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
			old := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
			_ = unstructured.SetNestedField(old.Object, "source-node", "spec", "hostname")
			setAtRiskSpot(old)
			replacement := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")
			r := recoveryReconciler(t, recoveryObj, restore, operation, trainingPolicy(), runtimeObj, old, replacement)
			phase, err := r.recover(ctx, recoveryObj)
			got := p.NewObject("NodeProvision")
			getErr := r.Get(ctx, client.ObjectKeyFromObject(old), got)
			if name == "valid-reordered" {
				if phase != phaseCleanupRequested || err == nil || !apierrors.IsNotFound(getErr) {
					t.Fatalf("valid cleanup phase=%s err=%v source=%v", phase, err, getErr)
				}
			} else {
				if err == nil || phase == phaseCleanupRequested || phase == "Completed" || getErr != nil {
					t.Fatalf("unsafe cleanup phase=%s err=%v source=%v", phase, err, getErr)
				}
			}
		})
	}
}

func TestCleanupGroupRejectsDeletingReplacement(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "deleting"}[deleting], func(t *testing.T) {
			policy := trainingPolicy()
			req := restoreRequest("group", "restore-uid", 7, "source-node")
			req.SetLabels(map[string]string{p.LabelRole: groupRestoreRole, p.LabelPolicy: policy.GetName(), p.LabelPolicyUID: string(policy.GetUID())})
			req.SetAnnotations(map[string]string{groupOldName: "old", groupOldUID: "old-uid", groupNewName: "new", groupNewUID: "new-uid"})
			_ = unstructured.SetNestedMap(req.Object, map[string]interface{}{"operationUID": "op-1", "sourceWorldUID": "workload-uid"}, "spec", "groupRestore")
			_ = unstructured.SetNestedMap(req.Object, map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}, "spec", "trainingRuntimeRef")
			_ = unstructured.SetNestedField(req.Object, true, "status", "verification", "sourceFenced")
			old := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
			replacement := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")
			if deleting {
				stamp := metav1.NewTime(time.Now())
				replacement.SetFinalizers([]string{"test/hold"})
				replacement.SetDeletionTimestamp(&stamp)
			}
			fixture := recoveryReconciler(t, policy, req, old, replacement)
			r := &GroupRecoveryReconciler{Client: fixture.Client, Reader: fixture.Client}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(req)})
			getErr := r.Get(context.Background(), client.ObjectKeyFromObject(old), p.NewObject("NodeProvision"))
			if deleting {
				if err == nil || !strings.Contains(err.Error(), "deleting") || getErr != nil {
					t.Fatalf("err=%v source=%v", err, getErr)
				}
			} else if err != nil || !apierrors.IsNotFound(getErr) {
				t.Fatalf("err=%v source=%v", err, getErr)
			}
		})
	}
}
