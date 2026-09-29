package management

import (
	"context"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Suspension blocks new intent; it must not strand an already paused world.
func (r *CheckpointReconciler) reconcileSuspendedPolicy(ctx context.Context, obj *unstructured.Unstructured, input p.PolicyInput) (ctrl.Result, error) {
	status := p.CheckpointStatus("", 0, r.now(), "policy_suspended")
	_, inflight, migration, measured, err := r.checkpointState(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	applyMeasuredCostsStatus(status, measured)
	status["periodicQuiesced"] = !inflight
	if inflight {
		status["lastMigrationName"] = migration.GetName()
		if err := r.createIfMissing(ctx, p.NewPropagationPolicyFor(input, migration, input.SourceCluster)); err != nil {
			return ctrl.Result{}, err
		}
	}
	if operation := obj.GetAnnotations()[groupIntentAnnotation]; operation != "" {
		status["replacementOperation"] = operation
	}
	active, err := r.activeSpotReplacement(ctx, input)
	if err != nil {
		return ctrl.Result{}, err
	}
	if active != nil {
		status["replacementOperation"] = active.GetName()
		status["message"] = "policy suspended; existing replacement still owns checkpoint execution"
		if err := r.coordinateReplacementCheckpoint(ctx, active); err != nil {
			status["message"] = err.Error()
		}
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, patchStatusSubtree(ctx, r.Client, obj, p.StatusCheckpointPath, status)
}
