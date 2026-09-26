package management

import (
	"context"
	"strings"
	"testing"
	"time"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRecoveryFailsClosedWithoutExtendedDeletionContract(t *testing.T) {
	r := recovery("recover")
	policy := trainingPolicy()
	reconciler := recoveryReconciler(t, policy)

	phase, err := reconciler.recover(context.Background(), r)
	if err == nil {
		t.Fatal("recover() error = nil, want fail-closed error")
	}
	if phase != "Rejected" {
		t.Fatalf("phase = %q, want Rejected", phase)
	}
	if !strings.Contains(err.Error(), "eventID") {
		t.Fatalf("error = %v, want missing extended contract", err)
	}
}

func TestRecoveryDeletesOldNodeProvisionOnlyAfterVerifiedEvidence(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	restore := restoreRequest("restore", "restore-uid", 7, "source-node")
	policy := trainingPolicy()
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	oldNP := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
	oldNP.Object["spec"].(map[string]interface{})["hostname"] = "source-node"
	setAtRiskSpot(oldNP)
	replacement := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")

	reconciler := recoveryReconciler(t, r, restore, policy, runtimeObj, oldNP, replacement)
	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != phaseCleanupRequested {
		t.Fatalf("first recover phase/error = %q/%v, want CleanupRequested error", phase, err)
	}
	phase, err = reconciler.recover(context.Background(), r)
	if err != nil {
		t.Fatalf("second recover() error = %v", err)
	}
	if phase != "Completed" {
		t.Fatalf("phase = %q, want Completed", phase)
	}

	got := trainingpolicy.NewObject("NodeProvision")
	err = reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old"}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("old NodeProvision get error = %v, want not found", err)
	}
}

func TestRecoveryDeletesOldNodeProvisionWithoutReplacementRef(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	delete(r.Object["spec"].(map[string]interface{}), "replacementNodeProvisionRef")
	restore := restoreRequest("restore", "restore-uid", 7, "source-node")
	policy := trainingPolicy()
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	oldNP := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
	oldNP.Object["spec"].(map[string]interface{})["hostname"] = "source-node"
	setAtRiskSpot(oldNP)

	reconciler := recoveryReconciler(t, r, restore, policy, runtimeObj, oldNP)
	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != phaseCleanupRequested {
		t.Fatalf("first recover phase/error = %q/%v, want CleanupRequested error", phase, err)
	}
	phase, err = reconciler.recover(context.Background(), r)
	if err != nil {
		t.Fatalf("second recover() error = %v", err)
	}
	if phase != "Completed" {
		t.Fatalf("phase = %q, want Completed", phase)
	}

	got := trainingpolicy.NewObject("NodeProvision")
	err = reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old"}, got)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("old NodeProvision get error = %v, want not found", err)
	}
}

func TestRecoveryDoesNotDeleteWhenReplacementNotReady(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	restore := restoreRequest("restore", "restore-uid", 7, "source-node")
	policy := trainingPolicy()
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	oldNP := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
	oldNP.Object["spec"].(map[string]interface{})["hostname"] = "source-node"
	setAtRiskSpot(oldNP)
	replacement := nodeProvision("new", "new-uid", "policy-uid", "op-1", "target", "Provisioning")

	reconciler := recoveryReconciler(t, r, restore, policy, runtimeObj, oldNP, replacement)
	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != "Pending" {
		t.Fatalf("recover() phase/error = %q/%v, want Pending error", phase, err)
	}

	got := trainingpolicy.NewObject("NodeProvision")
	if err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "old"}, got); err != nil {
		t.Fatalf("old NodeProvision was deleted: %v", err)
	}
}

func TestRecoveryRejectsPolicyWorkloadMismatch(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	policy := trainingPolicy()
	policy.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "job-b"}}
	reconciler := recoveryReconciler(t, r, policy)

	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != "Rejected" || !strings.Contains(err.Error(), "TrainingPolicy workloadRef.uid") {
		t.Fatalf("recover() phase/error = %q/%v, want policy workload rejection", phase, err)
	}
}

func TestRecoveryRejectsInventedSpotSourceEvidence(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	restore := restoreRequest("restore", "restore-uid", 7, "source-node")
	policy := trainingPolicy()
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	oldNP := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
	oldNP.Object["spec"].(map[string]interface{})["hostname"] = "source-node"
	oldNP.Object["status"].(map[string]interface{})["spot"] = map[string]interface{}{"eventID": "event-1", "instanceID": "i-old", "source": "IMDSv2"}
	replacement := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")

	reconciler := recoveryReconciler(t, r, restore, policy, runtimeObj, oldNP, replacement)
	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != "Pending" || !strings.Contains(err.Error(), "atRisk") {
		t.Fatalf("recover() phase/error = %q/%v, want actual spot RIC rejection", phase, err)
	}
}

func TestRecoveryDoesNotCompleteWhileNodeProvisionStillDeleting(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	restore := restoreRequest("restore", "restore-uid", 7, "source-node")
	policy := trainingPolicy()
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	oldNP := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
	oldNP.Object["spec"].(map[string]interface{})["hostname"] = "source-node"
	oldNP.SetFinalizers([]string{"cleanup.example/finalizer"})
	setAtRiskSpot(oldNP)
	replacement := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")

	reconciler := recoveryReconciler(t, r, restore, policy, runtimeObj, oldNP, replacement)
	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != phaseCleanupRequested {
		t.Fatalf("first recover phase/error = %q/%v, want CleanupRequested", phase, err)
	}
	phase, err = reconciler.recover(context.Background(), r)
	if err == nil || phase != phaseCleanupRequested {
		t.Fatalf("second recover phase/error = %q/%v, want still CleanupRequested", phase, err)
	}
}

func TestRecoveryRejectsDeletingRestoreRequest(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	restore := restoreRequest("restore", "restore-uid", 7, "source-node")
	now := metav1.NewTime(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	restore.SetFinalizers([]string{"restore.example/finalizer"})
	restore.SetDeletionTimestamp(&now)
	policy := trainingPolicy()
	runtimeObj := object("TrainingRuntime", "default", "runtime", "runtime-uid")
	runtimeObj.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	reconciler := recoveryReconciler(t, r, restore, policy, runtimeObj)

	phase, err := reconciler.recover(context.Background(), r)
	if err == nil || phase != "Pending" || !strings.Contains(err.Error(), "RestoreRequest is deleting") {
		t.Fatalf("recover() phase/error = %q/%v, want deleting RestoreRequest rejection", phase, err)
	}
}

func TestRecoveryRequiresAPIVisibleRetirementMarkerBeforeDelete(t *testing.T) {
	r := recovery("recover")
	addFullRecoverySpec(r)
	apiReader := recoveryReconciler(t)
	reconciler := &RecoveryReconciler{Client: apiReader.Client, APIReader: apiReader.Client}

	err := reconciler.verifyRetirementMarkerPersisted(context.Background(), r, readRecoverySpec(r))
	if err == nil || !strings.Contains(err.Error(), "not visible") {
		t.Fatalf("verifyRetirementMarkerPersisted error = %v, want invisible marker", err)
	}
}

func recovery(name string) *unstructured.Unstructured {
	obj := object("SpotRecovery", "default", name, "recovery-uid")
	obj.SetGeneration(1)
	obj.Object["spec"] = map[string]interface{}{
		"policyRef": map[string]interface{}{
			"name":       "policy",
			"uid":        "policy-uid",
			"generation": int64(3),
		},
		"requestUID":   "restore-uid",
		"workloadRef":  map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": "trainer", "uid": "workload-uid"},
		"checkpointID": "ckpt-1",
		"eventID":      "event-1",
		"restoreRequestRef": map[string]interface{}{
			"name":       "restore",
			"uid":        "restore-uid",
			"generation": int64(7),
		},
	}
	return obj
}

func addFullRecoverySpec(obj *unstructured.Unstructured) {
	spec := obj.Object["spec"].(map[string]interface{})
	spec["operation"] = "op-1"
	spec["sourceCluster"] = "aws"
	spec["targetCluster"] = "onprem"
	spec["trainingRuntimeRef"] = map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}
	spec["oldNodeProvisionRef"] = map[string]interface{}{"name": "old", "uid": "old-uid"}
	spec["replacementNodeProvisionRef"] = map[string]interface{}{"name": "new", "uid": "new-uid"}
	obj.Object["status"] = map[string]interface{}{}
}

func restoreRequest(name, uid string, generation int64, sourceNode string) *unstructured.Unstructured {
	obj := newRestoreRequest()
	obj.SetNamespace("default")
	obj.SetName(name)
	obj.SetUID(types.UID(uid))
	obj.SetGeneration(generation)
	obj.Object["spec"] = map[string]interface{}{
		"sourceFenced":  true,
		"sourceCluster": "aws",
		"targetCluster": "onprem",
		"workloadRef":   map[string]interface{}{"uid": "workload-uid"},
		"checkpointRef": map[string]interface{}{
			"checkpointID": "ckpt-1",
		},
		"pods": []interface{}{map[string]interface{}{"sourceNode": sourceNode}},
	}
	obj.Object["status"] = map[string]interface{}{
		"phase":              "Verified",
		"observedGeneration": generation,
		"verification": map[string]interface{}{
			"requestUID":         "restore-uid",
			"checkpointID":       "ckpt-1",
			"verifiedAt":         "2026-09-26T00:00:00Z",
			"trainingRuntimeRef": map[string]interface{}{"name": "runtime", "uid": "runtime-uid"},
			"sourceCluster":      "aws",
			"targetCluster":      "onprem",
		},
	}
	return obj
}

func trainingPolicy() *unstructured.Unstructured {
	policy := object("TrainingPolicy", "default", "policy", "policy-uid")
	policy.SetGeneration(3)
	policy.Object["spec"] = map[string]interface{}{"workloadRef": map[string]interface{}{"uid": "workload-uid"}}
	return policy
}

func setAtRiskSpot(obj *unstructured.Unstructured) {
	obj.Object["status"].(map[string]interface{})["spot"] = map[string]interface{}{
		"atRisk":     true,
		"signalType": "InterruptionNotice",
		"eventID":    "event-1",
		"instanceID": "i-" + obj.GetName(),
	}
}

func nodeProvision(name, uid, policyUID, operation, observedCluster, phase string) *unstructured.Unstructured {
	obj := object("NodeProvision", "default", name, uid)
	obj.SetLabels(map[string]string{trainingpolicy.LabelPolicyUID: policyUID})
	obj.SetAnnotations(map[string]string{"training.dcnlab.com/recovery-operation": operation})
	obj.Object["spec"] = map[string]interface{}{"marketType": "Spot"}
	obj.Object["status"] = map[string]interface{}{"observedCluster": observedCluster, "phase": phase, "instanceId": "i-" + name}
	return obj
}

func object(kind, namespace, name, uid string) *unstructured.Unstructured {
	obj := trainingpolicy.NewObject(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetUID(types.UID(uid))
	return obj
}

func recoveryReconciler(t *testing.T, objects ...client.Object) *RecoveryReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, gvk := range []string{"TrainingPolicy", "TrainingRuntime", "SpotRecovery", "NodeProvision"} {
		obj := trainingpolicy.NewObject(gvk)
		scheme.AddKnownTypeWithName(obj.GroupVersionKind(), &unstructured.Unstructured{})
		listGVK := obj.GroupVersionKind()
		listGVK.Kind += "List"
		scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	}
	scheme.AddKnownTypeWithName(restoreRequestGVK, &unstructured.Unstructured{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &RecoveryReconciler{Client: c, Clock: func() time.Time { return time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC) }}
}
