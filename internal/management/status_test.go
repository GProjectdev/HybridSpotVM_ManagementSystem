package management

import (
	"context"
	"testing"

	trainingpolicy "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIndependentStatusWritersPreserveOtherComponent(t *testing.T) {
	ctx := context.Background()
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.SetName("separate-components")
	policy.SetNamespace("default")
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(policy).WithObjects(policy).Build()
	key := client.ObjectKeyFromObject(policy)
	for _, path := range []string{trainingpolicy.StatusPolicyPath, trainingpolicy.StatusCheckpointPath, trainingpolicy.StatusPolicyPath} {
		current := trainingpolicy.NewObject("TrainingPolicy")
		if err := c.Get(ctx, key, current); err != nil {
			t.Fatal(err)
		}
		if err := patchStatusSubtree(ctx, c, current, path, map[string]interface{}{"reason": path}); err != nil {
			t.Fatal(err)
		}
	}
	got := trainingpolicy.NewObject("TrainingPolicy")
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{trainingpolicy.StatusPolicyPath, trainingpolicy.StatusCheckpointPath} {
		reason, _, err := unstructured.NestedString(got.Object, "status", path, "reason")
		if err != nil || reason != path {
			t.Fatalf("component %s status lost: %#v (%v)", path, got.Object["status"], err)
		}
	}
}
func TestStatusPatchRetriesStaleResourceVersion(t *testing.T) {
	ctx := context.Background()
	policy := trainingpolicy.NewObject("TrainingPolicy")
	policy.SetName("conflicting-components")
	policy.SetNamespace("default")
	policy.SetUID("policy-uid")
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(policy).WithObjects(policy).Build()
	key := client.ObjectKeyFromObject(policy)

	stale := trainingpolicy.NewObject("TrainingPolicy")
	if err := c.Get(ctx, key, stale); err != nil {
		t.Fatal(err)
	}
	current := trainingpolicy.NewObject("TrainingPolicy")
	if err := c.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if err := patchStatusSubtree(ctx, c, current, trainingpolicy.StatusPolicyPath, map[string]interface{}{"reason": "policy-writer"}); err != nil {
		t.Fatal(err)
	}
	if err := patchStatusSubtree(ctx, c, stale, trainingpolicy.StatusCheckpointPath, map[string]interface{}{"reason": "checkpoint-writer"}); err != nil {
		t.Fatalf("stale status patch was not retried: %v", err)
	}

	got := trainingpolicy.NewObject("TrainingPolicy")
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		trainingpolicy.StatusPolicyPath:     "policy-writer",
		trainingpolicy.StatusCheckpointPath: "checkpoint-writer",
	} {
		reason, _, err := unstructured.NestedString(got.Object, "status", path, "reason")
		if err != nil || reason != want {
			t.Fatalf("component %s reason = %q, want %q (status=%#v, err=%v)", path, reason, want, got.Object["status"], err)
		}
	}
}
