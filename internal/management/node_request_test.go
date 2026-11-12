package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func unboundNodeRequestFixture(now time.Time) *unstructured.Unstructured {
	request := policyFixtureForCapacity(now, 2)
	unstructured.RemoveNestedField(request.Object, "spec", "workloadRef", "uid")
	unstructured.RemoveNestedField(request.Object, "status")
	_ = unstructured.SetNestedField(request.Object, "aws", "spec", "sourceCluster")
	return request
}

func TestNodeRequestInitialCapacityBeforeWorkload(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	request := unboundNodeRequestFixture(now)
	r := policyReconcilerFixture(t, func() time.Time { return now }, request, riskFixture(now))
	key := client.ObjectKeyFromObject(request)
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	assertNodeRequestObjectCount(t, r.Client, "NodeProvision", 2)
	assertNodeRequestObjectCount(t, r.Client, "SpotReplacement", 0)
	assertNodeRequestObjectCount(t, r.Client, "FluidCRMigration", 0)
	if err := r.Get(ctx, client.ObjectKey{Namespace: "default", Name: "trainer"}, p.NewObject("StatefulSet")); !apierrors.IsNotFound(err) {
		t.Fatalf("workload must remain absent: %v", err)
	}
	current := p.NewObject("NodeRequest")
	if err := r.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if stringField(current.Object, "spec", "workloadRef", "uid") != "" || !boolField(current.Object, "status", "policy", "awaitingWorkload") || boolField(current.Object, "status", "policy", "provisioningBlocked") {
		t.Fatalf("unexpected initial allocation status: %v", current.Object)
	}
	nodes := p.NewList("NodeProvision")
	if err := r.List(ctx, nodes); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes.Items {
		if node.GetLabels()[p.LabelPolicyUID] != string(request.GetUID()) || stringField(node.Object, "spec", "marketType") != "Spot" {
			t.Fatalf("initial capacity identity/market mismatch: %v", node.Object)
		}
	}
}

func TestNodeRequestBindingClearsAwaitingWorkloadBeforePlacementReady(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	request := unboundNodeRequestFixture(now)
	r := policyReconcilerFixture(t, func() time.Time { return now }, request, riskFixture(now))
	r.APIReader = r.Client
	key := client.ObjectKeyFromObject(request)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	assertNodeRequestObjectCount(t, r.Client, "NodeProvision", 2)
	current := p.NewObject("NodeRequest")
	if err := r.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if !boolField(current.Object, "status", "policy", "awaitingWorkload") {
		t.Fatal("initial allocation must await the workload")
	}
	// Create after fixture setup so no discovery or placement evidence is added.
	if err := r.Create(ctx, workloadFixture("bound-workload-uid")); err != nil {
		t.Fatal(err)
	}
	binder := &NodeRequestBindingReconciler{Client: r.Client, Reader: r.Client}
	if _, err := binder.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if stringField(current.Object, "spec", "workloadRef", "uid") != "bound-workload-uid" ||
		!boolField(current.Object, "status", "policy", "awaitingWorkload") {
		t.Fatal("binding must set UID before the policy controller clears awaitingWorkload")
	}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, key, current); err != nil {
			t.Fatal(err)
		}
		awaiting, found, err := unstructured.NestedBool(current.Object, "status", "policy", "awaitingWorkload")
		if err != nil || !found || awaiting {
			t.Fatalf("awaitingWorkload must explicitly become false: value=%v found=%v err=%v", awaiting, found, err)
		}
		if !boolField(current.Object, "status", "policy", "provisioningBlocked") ||
			stringField(current.Object, "status", "policy", "reason") != "placement_gate" {
			t.Fatalf("expected placement gate after binding: %v", current.Object["status"])
		}
		assertNodeRequestObjectCount(t, r.Client, "NodeProvision", 2)
		assertNodeRequestObjectCount(t, r.Client, "SpotReplacement", 0)
	}
}

func TestNodeRequestDuplicateInitialAllocationBeforeWorkload(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	first := unboundNodeRequestFixture(now)
	second := first.DeepCopy()
	second.SetName("other-request")
	second.SetUID("other-request-uid")
	// Both requests need fresh risk so the duplicate gate is what blocks them.
	for _, request := range []*unstructured.Unstructured{first, second} {
		if err := unstructured.SetNestedField(request.Object, "train-risk", "spec", "riskProfileRef", "name"); err != nil {
			t.Fatal(err)
		}
	}
	r := policyReconcilerFixture(t, func() time.Time { return now }, first, second, riskFixture(now))
	r.APIReader = r.Client
	for round := 0; round < 2; round++ {
		for _, request := range []*unstructured.Unstructured{first, second} {
			key := client.ObjectKeyFromObject(request)
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if err == nil || err.Error() != "multiple NodeRequests reference workload; allocation and binding refused" {
				t.Fatalf("%s duplicate allocation error = %v", request.GetName(), err)
			}
			assertNodeRequestObjectCount(t, r.Client, "NodeProvision", 0)
			current := p.NewObject("NodeRequest")
			if err := r.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if stringField(current.Object, "spec", "workloadRef", "uid") != "" {
				t.Fatalf("%s bound before workload exists", request.GetName())
			}
		}
	}
	assertNodeRequestObjectCount(t, r.Client, "StatefulSet", 0)
	assertNodeRequestObjectCount(t, r.Client, "SpotReplacement", 0)
}

func TestNodeRequestInitialCapacityRiskGates(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"expired", "not-ready", "old-generation", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			request := unboundNodeRequestFixture(now)
			risk := riskFixture(now)
			switch scenario {
			case "expired":
				_ = unstructured.SetNestedField(risk.Object, now.Add(-time.Second).Format(time.RFC3339), "status", "validUntil")
			case "not-ready":
				_ = unstructured.SetNestedField(risk.Object, false, "status", "ready")
			case "old-generation":
				risk.SetGeneration(2)
			}
			objects := []*unstructured.Unstructured{request}
			if scenario != "missing" {
				objects = append(objects, risk)
			}
			r := policyReconcilerFixture(t, func() time.Time { return now }, objects...)
			key := client.ObjectKeyFromObject(request)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			assertNodeRequestObjectCount(t, r.Client, "NodeProvision", 0)
			current := p.NewObject("NodeRequest")
			if err := r.Get(context.Background(), key, current); err != nil {
				t.Fatal(err)
			}
			if !boolField(current.Object, "status", "policy", "provisioningBlocked") || stringField(current.Object, "status", "policy", "reason") != "risk_not_fresh" {
				t.Fatalf("risk gate not reported: %v", current.Object["status"])
			}
		})
	}
}

func TestNodeRequestUnboundCannotReplaceExistingCapacity(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	request := unboundNodeRequestFixture(now)
	_ = unstructured.SetNestedField(request.Object, true, "spec", "replacement", "enabled")
	old := capacityWorkerFixture(p.ReadPolicyInput(request), 0, "OnDemand", "old-node-uid")
	r := policyReconcilerFixture(t, func() time.Time { return now }, request, riskFixture(now), old)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(request)}); err != nil {
		t.Fatal(err)
	}
	assertNodeRequestObjectCount(t, r.Client, "NodeProvision", 1)
	assertNodeRequestObjectCount(t, r.Client, "SpotReplacement", 0)
	current := p.NewObject("NodeRequest")
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(request), current); err != nil {
		t.Fatal(err)
	}
	if !boolField(current.Object, "status", "policy", "provisioningBlocked") {
		t.Fatal("unbound replacement was not blocked")
	}
}

func TestNodeRequestBindingOnceAndWorkloadRecreation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	request := unboundNodeRequestFixture(now)
	policy := policyReconcilerFixture(t, func() time.Time { return now }, request, riskFixture(now))
	binder := &NodeRequestBindingReconciler{Client: policy.Client, Reader: policy.Client}
	key := client.ObjectKeyFromObject(request)
	result, err := binder.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("missing workload must requeue: %v, %v", result, err)
	}
	current := p.NewObject("NodeRequest")
	if err := policy.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if stringField(current.Object, "spec", "workloadRef", "uid") != "" {
		t.Fatal("bound absent workload")
	}
	workload := workloadFixture("original-workload")
	if err := policy.Create(ctx, workload); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := binder.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}
	}
	if err := policy.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if stringField(current.Object, "spec", "workloadRef", "uid") != "original-workload" {
		t.Fatal("initial UID not bound")
	}
	if err := policy.Delete(ctx, workload); err != nil {
		t.Fatal(err)
	}
	if _, err := binder.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	replacement := workloadFixture("recreated-workload")
	if err := policy.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := binder.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := policy.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if stringField(current.Object, "spec", "workloadRef", "uid") != "original-workload" {
		t.Fatal("rebound recreated workload")
	}
	if _, err := policy.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := policy.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	if stringField(current.Object, "status", "policy", "reason") != "stale_workload_uid" {
		t.Fatalf("recreation gate missing: %v", current.Object["status"])
	}
	assertNodeRequestObjectCount(t, policy.Client, "NodeProvision", 0)
}

func TestNodeRequestBindingRejectsDuplicateWorkloadReference(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "unbound"
		if bound {
			name = "bound"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
			request := unboundNodeRequestFixture(now)
			other := request.DeepCopy()
			other.SetName("other-request")
			other.SetUID("other-request-uid")
			if bound {
				_ = unstructured.SetNestedField(other.Object, "workload-uid", "spec", "workloadRef", "uid")
			}
			policy := policyReconcilerFixture(t, func() time.Time { return now }, request, other)
			if err := policy.Create(ctx, workloadFixture("workload-uid")); err != nil {
				t.Fatal(err)
			}
			binder := &NodeRequestBindingReconciler{Client: policy.Client, Reader: policy.Client}
			key := client.ObjectKeyFromObject(request)
			_, err := binder.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			if err == nil || err.Error() != "multiple NodeRequests reference workload; allocation and binding refused" {
				t.Fatalf("duplicate reference accepted: %v", err)
			}
			current := p.NewObject("NodeRequest")
			if err := policy.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if stringField(current.Object, "spec", "workloadRef", "uid") != "" {
				t.Fatal("duplicate reference was bound")
			}
		})
	}
}

func assertNodeRequestObjectCount(t *testing.T, c client.Client, kind string, want int) {
	t.Helper()
	list := p.NewList(kind)
	if kind == "SpotReplacement" {
		list = newSpotReplacementList()
	}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != want {
		t.Fatalf("%s count = %d, want %d", kind, len(list.Items), want)
	}
}
