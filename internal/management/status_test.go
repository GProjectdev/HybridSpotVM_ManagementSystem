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
