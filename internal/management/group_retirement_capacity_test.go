package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func retirementCapacityFixture() (*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	policy := trainingPolicy()
	req := restoreRequest("prior-group", "restore-uid", 7, "old-node")
	req.SetLabels(map[string]string{p.LabelRole: groupRestoreRole, p.LabelPolicy: policy.GetName(), p.LabelPolicyUID: string(policy.GetUID())})
	req.SetAnnotations(map[string]string{groupOldName: "old", groupOldUID: "old-uid", groupNewName: "new", groupNewUID: "new-uid"})
	_ = unstructured.SetNestedMap(req.Object, map[string]interface{}{"operationUID": "op-1", "sourceWorldUID": "workload-uid"}, "spec", "groupRestore")
	_ = unstructured.SetNestedMap(req.Object, map[string]interface{}{"name": "runtime", "uid": "runtime-uid"}, "spec", "trainingRuntimeRef")
	_ = unstructured.SetNestedField(req.Object, true, "status", "verification", "sourceFenced")
	old := nodeProvision("old", "old-uid", "policy-uid", "op-1", "aws", "Ready")
	newNP := nodeProvision("new", "new-uid", "policy-uid", "op-1", "aws", "Ready")
	return policy, req, old, newNP
}

func TestVerifiedGroupRetirementRequiresSourceFinalizerCompletion(t *testing.T) {
	for _, kind := range []string{"present", "deleting", "gone", "recreated-source", "recreated-target", "foreign-policy", "stale-verification", "other-world"} {
		t.Run(kind, func(t *testing.T) {
			policy, req, old, target := retirementCapacityFixture()
			switch kind {
			case "deleting":
				old.SetFinalizers([]string{"test/provider-termination"})
				stamp := metav1.Now()
				old.SetDeletionTimestamp(&stamp)
			case "recreated-source":
				old.SetUID("other-source")
			case "recreated-target":
				target.SetUID("other-target")
			case "foreign-policy":
				req.SetLabels(map[string]string{p.LabelRole: groupRestoreRole, p.LabelPolicyUID: "other-policy"})
			case "stale-verification":
				_ = unstructured.SetNestedField(req.Object, int64(6), "status", "observedGeneration")
			case "other-world":
				_ = unstructured.SetNestedField(req.Object, "another-world", "spec", "groupRestore", "sourceWorldUID")
			}
			objects := []client.Object{policy, req, target}
			if kind != "gone" {
				objects = append(objects, old)
			}
			reader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
			r := &PolicyReconciler{Client: reader, APIReader: reader}
			pending, err := r.pendingVerifiedGroupRetirement(context.Background(), p.ReadPolicyInput(policy))
			wantError := kind == "recreated-source" || kind == "recreated-target"
			if (err != nil) != wantError {
				t.Fatalf("pending=%q err=%v, wantError=%v", pending, err, wantError)
			}
			wantPending := kind == "present" || kind == "deleting"
			if (pending != "") != wantPending {
				t.Fatalf("pending=%q, want pending=%v", pending, wantPending)
			}
		})
	}
}

func TestVerifiedGroupRetirementUsesFreshReaderAndDoesNotAllocate(t *testing.T) {
	_, req, old, target := retirementCapacityFixture()
	policy := policyFixtureForCapacity(time.Now(), 2)
	_ = unstructured.SetNestedField(policy.Object, true, "spec", "replacement", "enabled")
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/planned-partial": "disabled"})
	req.SetLabels(map[string]string{p.LabelRole: groupRestoreRole, p.LabelPolicy: policy.GetName(), p.LabelPolicyUID: string(policy.GetUID())})
	a := req.GetAnnotations()
	a[groupOldName] = "train-worker-00"
	req.SetAnnotations(a)
	old.SetName("train-worker-00")
	old.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: "worker"})
	target.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: "replacement"})
	_ = unstructured.SetNestedField(target.Object, "OnDemand", "spec", "marketType")
	remaining := nodeProvision("train-worker-01", "remaining-uid", "policy-uid", "", "aws", "Ready")
	remaining.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid", p.LabelRole: "worker"})
	fresh := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, req, old, target, remaining).Build()
	cache := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(policy, req, target, remaining).Build()
	r := &PolicyReconciler{Client: cache, APIReader: fresh}
	result, err := r.checkCapacityLifecycle(context.Background(), policy, p.ReadPolicyInput(policy), p.Decision{DesiredWorkers: 2, OnDemandWorkers: 2})
	if err != nil || !result.Blocked || result.Reason != "verified_source_retirement_pending" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	list := p.NewList("NodeProvision")
	if err := cache.List(context.Background(), list, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatal("pending prior retirement allocated another provider")
	}
}
