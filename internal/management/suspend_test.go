package management

import (
	"context"
	"testing"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSuspendedPolicyDoesNotCreateNewWork(t *testing.T) {
	policy := replacementPolicyFixture()
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/suspend": "true"})
	now := func() time.Time { return time.Now() }
	r := policyReconcilerFixture(t, now, policy)
	key := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}
	if _, err := r.Reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	cp := &CheckpointReconciler{Client: r.Client, Clock: now}
	if _, err := cp.Reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"NodeProvision", "SpotReplacement", "FluidCRMigration"} {
		list := p.NewList(kind)
		if kind == "SpotReplacement" {
			list = newSpotReplacementList()
		}
		if err := r.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) != 0 {
			t.Fatalf("suspended policy created %s", kind)
		}
	}
	got := p.NewObject("TrainingPolicy")
	if err := r.Get(context.Background(), key.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if stringField(got.Object, "status", "policy", "reason") != "policy_suspended" || stringField(got.Object, "status", "checkpoint", "reason") != "policy_suspended" {
		t.Fatalf("suspend status missing: %v", got.Object["status"])
	}
}

func TestSuspensionDoesNotStrandExistingReplacement(t *testing.T) {
	policy := replacementPolicyFixture()
	generation := policy.GetGeneration()
	policy.SetAnnotations(map[string]string{"training.dcnlab.com/suspend": "true"})
	op := replacementOperationFixture()
	op.SetLabels(map[string]string{p.LabelPolicyUID: "policy-uid"})
	fixture := replacementReconcilerFixture(t, time.Now())
	c := fake.NewClientBuilder().WithScheme(fixture.Scheme()).WithStatusSubresource(policy, op).WithObjects(policy, op, replacementOldNodeProvisionFixture(), replacementReadyNodeProvisionFixture()).Build()
	cp := &CheckpointReconciler{Client: c, Clock: fixture.Clock}
	if _, err := cp.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	migration := p.NewObject("FluidCRMigration")
	key := client.ObjectKey{Namespace: policy.GetNamespace(), Name: op.GetName() + "-partial-checkpoint"}
	if err := cp.Get(context.Background(), key, migration); err != nil {
		t.Fatalf("existing operation stranded: %v", err)
	}
	if policy.GetGeneration() != generation {
		t.Fatal("suspension changed operation policy generation")
	}
}
