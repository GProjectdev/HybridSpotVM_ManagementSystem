package management

import (
	"context"
	"testing"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestFallbackCapacityRepairsPropagationBeforeAcknowledgement(t *testing.T) {
	ctx := context.Background()
	now := mustParseTime(t, "2026-09-28T00:02:00Z")
	op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
	r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, sts)
	spec, err := readReplacementSpec(op)
	if err != nil {
		t.Fatal(err)
	}
	input := p.ReadPolicyInput(policy)
	prepare := func() {
		t.Helper()
		if err := r.prepareReplacementForGroupFallback(ctx, op.GetNamespace(), spec, old, input); err != nil {
			t.Fatal(err)
		}
	}
	prepare()
	propagation := p.NewPropagationPolicyFor(input, replacement, input.Capacity.AWSCluster)
	if err := r.Get(ctx, client.ObjectKeyFromObject(propagation), propagation); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, propagation); err != nil {
		t.Fatal(err)
	}
	prepare()
	if err := r.Get(ctx, client.ObjectKeyFromObject(propagation), propagation); err != nil {
		t.Fatalf("propagation was not repaired: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(replacement), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.GetAnnotations()[fallbackCapacityUID] != string(replacement.GetUID()) {
		t.Fatal("handoff is not UID bound")
	}
}

func TestFallbackCoordinatorRejectsChangedCapacityIdentity(t *testing.T) {
	for _, mutation := range []string{"operation", "uid"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			now := mustParseTime(t, "2026-09-28T00:02:00Z")
			op, policy, runtimeObj, old, replacement, _, checkpoint, sts := replacementFallbackFixtures(t, now, true)
			refreshFallbackRuntime(runtimeObj, now, "trainer-1-CURRENT")
			policy.SetAnnotations(map[string]string{groupIntentAnnotation: "np-origin-1"})
			policy.Object["status"] = map[string]interface{}{"checkpoint": map[string]interface{}{"periodicQuiesced": true, "replacementOperation": "np-origin-1", "reason": "group_intent_quiesced"}}
			r := replacementFallbackReconcilerFixture(t, now, op, policy, runtimeObj, old, replacement, checkpoint, replacementFallbackSurvivorNode(), sts)
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			vm := &ReplacementCapacityReconciler{Client: r.Client, APIReader: r.Client, Clock: r.Clock}
			if _, err := vm.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(replacement), replacement); err != nil {
				t.Fatal(err)
			}
			annotations := replacement.GetAnnotations()
			if mutation == "operation" {
				annotations["training.dcnlab.com/recovery-operation"] = "other-operation"
			} else {
				annotations[fallbackCapacityUID] = "previous-uid"
			}
			replacement.SetAnnotations(annotations)
			if err := r.Update(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReplacementReconciler.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "operation-group-restore"}, newRestoreRequest()); !apierrors.IsNotFound(err) {
				t.Fatalf("unsafe restore produced: %v", err)
			}
		})
	}
}
