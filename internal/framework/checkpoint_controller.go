package framework

import (
	"context"
	"fmt"
	"reflect"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// FrameworkCheckpointReconciler is the sole schedule writer for TrainingRuns.
// The existing NodeRequest coordinator cannot select these ownership labels.
// Actual all-rank saving and durable receipts remain the existing FluidCR engine.
type FrameworkCheckpointReconciler struct {
	client.Client
	Members MemberReader
	Now     func() time.Time
}

func (r *FrameworkCheckpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := named("TrainingRun", req.Namespace, req.Name)
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	report := func(phase, msg string, interval int64) (ctrl.Result, error) {
		base := run.DeepCopy()
		_ = unstructured.SetNestedMap(run.Object, map[string]interface{}{"phase": phase, "message": msg, "intervalSeconds": interval, "observedAt": stamp(now)}, "status", "checkpoint")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, r.Status().Patch(ctx, run, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	var s RunSpec
	if err := decodeSpec(run, &s); err != nil {
		return report("Invalid", err.Error(), 0)
	}
	if err := s.Validate(); err != nil {
		return report("Invalid", err.Error(), 0)
	}
	if s.Mode != "Execute" {
		return report("ObserveOnly", "no checkpoint writes in Observe mode", 0)
	}
	schedule := p.NewObject("FluidCRMigration")
	schedule.SetName(run.GetName() + "-periodic")
	schedule.SetNamespace(req.Namespace)
	pause := func(reason string) (ctrl.Result, error) {
		if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), schedule); err == nil {
			if !owned(schedule, run) {
				return report("OwnershipConflict", "foreign periodic schedule", 0)
			}
			if boolAt(schedule, "spec", "schedule", "enabled") {
				base := schedule.DeepCopy()
				_ = unstructured.SetNestedField(schedule.Object, false, "spec", "schedule", "enabled")
				if err := r.Patch(ctx, schedule, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return ctrl.Result{}, err
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return report("Paused", reason, 0)
	}
	if str(run, "status", "phase") != "Running" || integer(run, "status", "observedGeneration") != run.GetGeneration() {
		return pause("Run has no current Running observation")
	}
	work := object("apps/v1", "StatefulSet")
	if err := r.Get(ctx, key(req.Namespace, s.WorkloadRef.Name), work); err != nil {
		return pause("workload unavailable")
	}
	if string(work.GetUID()) != str(run, "status", "workloadUID") || work.GetLabels()[RunUIDLabel] != string(run.GetUID()) {
		return pause("workload identity changed")
	}
	o, err := ObserveWorkload(ctx, r.Client, r.Members, run, work, s, now)
	if err != nil || !o.Running {
		return pause("fresh all-rank training progress unavailable")
	}
	if o.SelectedCluster != s.OnPremiseCluster {
		return pause("Cloud interval receipt integration is not enabled")
	}
	transitions, err := listItems(ctx, r.Client, "training.dcnlab.com/v1alpha1", "TrainingTransition", req.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range transitions {
		t := &transitions[i]
		if owned(t, run) && str(t, "status", "phase") != "Completed" && str(t, "status", "phase") != "Cancelled" {
			return pause("transition owns the checkpoint boundary")
		}
	}
	interval, err := CheckpointInterval(s.CheckpointSeconds, 0, false, false)
	if err != nil {
		return pause(err.Error())
	}
	input := p.PolicyInput{Namespace: req.Namespace, PolicyName: run.GetName(), PolicyUID: run.GetUID(), WorkloadRef: p.WorkloadRef{APIVersion: "apps/v1", Kind: "StatefulSet", Name: work.GetName(), UID: work.GetUID()}, Checkpoint: p.CheckpointPolicy{Resume: true}}
	desired := p.NewFluidCRMigration(input, o.Runtime, now, interval)
	desired.SetName(schedule.GetName())
	desired.SetAnnotations(nil)
	desired.SetLabels(map[string]string{RunUIDLabel: string(run.GetUID()), p.LabelManagedBy: FrameworkManagedBy, p.LabelRole: "checkpoint-schedule"})
	_ = unstructured.SetNestedMap(desired.Object, map[string]interface{}{"enabled": true, "intervalSeconds": interval}, "spec", "schedule")
	if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), schedule); err == nil {
		if !owned(schedule, run) {
			return report("OwnershipConflict", "schedule owned by another controller", 0)
		}
		old, _, _ := unstructured.NestedFieldCopy(schedule.Object, "spec", "workloadRef")
		want, _, _ := unstructured.NestedFieldCopy(desired.Object, "spec", "workloadRef")
		if !reflect.DeepEqual(old, want) || integer(schedule, "spec", "ctrlPort") != integer(desired, "spec", "ctrlPort") || str(schedule, "spec", "container") != str(desired, "spec", "container") {
			return pause("immutable checkpoint execution contract changed")
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if err := ensureSpec(ctx, r.Client, desired, run); err != nil {
		return ctrl.Result{}, err
	}
	if err := ensurePlacement(ctx, r.Client, run, desired, []string{o.SelectedCluster}); err != nil {
		return ctrl.Result{}, err
	}
	return report("Scheduled", fmt.Sprintf("FluidCR schedule %s; durable completion is separately reported by the engine", desired.GetName()), interval)
}

func SetupFrameworkCheckpoint(mgr ctrl.Manager) error {
	r := &FrameworkCheckpointReconciler{Client: mgr.GetClient(), Members: configuredMembers()}
	return ctrl.NewControllerManagedBy(mgr).Named("training-run-checkpoint").For(resource.Object("TrainingRun")).WithEventFilter(predicate.GenerationChangedPredicate{}).Complete(r)
}
