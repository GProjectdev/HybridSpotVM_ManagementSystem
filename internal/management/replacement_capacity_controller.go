package management

import (
	"context"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReplacementCapacityReconciler owns only replacement capacity and its
// propagation. The coordinator owns all SpotReplacement status and requests.
// Ready NodeProvision identity is the durable VM-to-coordinator handoff.
type ReplacementCapacityReconciler struct {
	client.Client
	APIReader client.Reader
	Clock     func() time.Time
}

func (r *ReplacementCapacityReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Observe status transitions too: the coordinator records fallback intent
	// without changing the operation generation. This controller never writes it.
	return ctrl.NewControllerManagedBy(mgr).Named("spot-replacement-capacity").
		For(newSpotReplacementObject()).Complete(r)
}

func (r *ReplacementCapacityReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	helper := &ReplacementReconciler{Client: r.Client, APIReader: r.APIReader, Clock: r.Clock}
	op := newSpotReplacementObject()
	if err := helper.reader().Get(ctx, req.NamespacedName, op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !op.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	phase := stringField(op.Object, "status", "phase")
	switch phase {
	case "", "Pending", replacementPhaseAwaitingReplacementReady, replacementPhaseAwaitingGroupFallback:
	default:
		// In particular, never recreate capacity during restore or after cleanup.
		return ctrl.Result{}, nil
	}
	again := ctrl.Result{RequeueAfter: time.Minute}
	spec, err := readReplacementSpec(op)
	if err != nil {
		return again, err
	}
	if err := helper.verifyReplacementPolicy(ctx, op.GetNamespace(), spec); err != nil {
		return again, err
	}
	if _, found, err := helper.existingSpotRecoveryPhase(ctx, op.GetNamespace(), spec); found || err != nil {
		return again, err
	}
	old := p.NewObject("NodeProvision")
	if err := helper.reader().Get(ctx, client.ObjectKey{Namespace: op.GetNamespace(), Name: spec.OldNodeProvisionName}, old); err != nil {
		return again, err
	}
	if phase == replacementPhaseAwaitingGroupFallback {
		return again, helper.prepareFallbackCapacity(ctx, op, spec, old)
	}
	if err := verifyReplacementOldNode(old, spec); err != nil {
		return again, err
	}
	_, _, err = helper.ensureReplacementNodeProvision(ctx, op.GetNamespace(), old, spec)
	return again, err
}
