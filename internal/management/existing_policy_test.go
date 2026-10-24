package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExistingPolicyNeverAllocates(t *testing.T) {
	now := time.Now()
	policy := checkpointPolicyFixture(now)
	_ = unstructured.SetNestedMap(policy.Object, map[string]interface{}{
		"mode":          "Existing",
		"aws":           map[string]interface{}{"karmadaCluster": "aws"},
		"existingNodes": []interface{}{map[string]interface{}{"rank": int64(0), "nodeName": "standby", "nodeUID": "standby-uid"}},
	}, "spec", "capacity")
	reconciler := policyReconcilerFixture(t, func() time.Time { return now }, policy)
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if stringField(policy.Object, "status", "policy", "reason") != "existing_capacity" {
		t.Fatalf("did not take existing-capacity path: %#v", policy.Object["status"])
	}
	nodes := p.NewList("NodeProvision")
	if err := reconciler.List(context.Background(), nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes.Items) != 0 {
		t.Fatal("existing capacity created NodeProvisions")
	}
	if err := reconciler.ensureNodeProvisions(context.Background(), p.ReadPolicyInput(policy), p.Decision{}); err == nil {
		t.Fatal("direct provisioning path accepted existing capacity")
	}
}
