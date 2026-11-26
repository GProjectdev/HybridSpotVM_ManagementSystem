package framework

import (
	"context"
	"fmt"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TransitionReconciler owns checkpoint/restore commands and their sequence. It
// cannot edit TrainingCapacity, TrainingRun status, or workload placement.
type TransitionReconciler struct {
	client.Client
	Members MemberReader
	Now     func() time.Time
}

func (r *TransitionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
func (r *TransitionReconciler) Reconcile(ctx context.Context, q ctrl.Request) (ctrl.Result, error) {
	t := named("TrainingTransition", q.Namespace, q.Name)
	if e := r.Get(ctx, q.NamespacedName, t); e != nil {
		return ctrl.Result{}, client.IgnoreNotFound(e)
	}
	status, _, _ := unstructured.NestedMap(t.Object, "status")
	if status == nil {
		status = map[string]interface{}{}
	}
	report := func(phase, msg string) (ctrl.Result, error) {
		status["phase"] = phase
		status["message"] = msg
		status["observedGeneration"] = t.GetGeneration()
		status["observedAt"] = stamp(r.now())
		return ctrl.Result{RequeueAfter: 5 * time.Second}, resource.SetStatus(ctx, r.Client, t, status)
	}
	if str(t, "status", "phase") == "Completed" || str(t, "status", "phase") == "Cancelled" {
		return ctrl.Result{}, nil
	}
	var ts TransitionSpec
	if e := decodeSpec(t, &ts); e != nil {
		return report("Invalid", e.Error())
	}
	run := named("TrainingRun", q.Namespace, ts.RunRef.Name)
	if e := r.Get(ctx, key(q.Namespace, ts.RunRef.Name), run); e != nil {
		return report("OwnerUnavailable", e.Error())
	}
	var rs RunSpec
	if e := decodeSpec(run, &rs); e != nil {
		return report("Invalid", e.Error())
	}
	work := object("apps/v1", "StatefulSet")
	if e := r.Get(ctx, key(q.Namespace, rs.WorkloadRef.Name), work); e != nil {
		return report("WorkloadUnavailable", e.Error())
	}
	if e := ts.validate(run, work, t, rs); e != nil {
		return report("IdentityConflict", e.Error())
	}
	if !t.GetDeletionTimestamp().IsZero() || !run.GetDeletionTimestamp().IsZero() || rs.Suspend {
		return report("Suspended", "execution paused; resources and recovery evidence retained")
	}
	current, e := activeTransition(ctx, r.Client, run)
	if e != nil || current == nil || current.GetUID() != t.GetUID() {
		return report("Conflict", "a unique owned active transition is required")
	}
	input := transitionInput(run, work, ts)
	request := object("migration.dcnlab.com/v1alpha1", "RestoreRequest")
	request.SetNamespace(q.Namespace)
	request.SetName(t.GetName() + "-restore")
	e = r.Get(ctx, client.ObjectKeyFromObject(request), request)
	if e == nil {
		if !owned(request, t) || str(request, "spec", "groupRestore", "operationUID") != string(t.GetUID()) || str(request, "spec", "workloadRef", "uid") != ts.WorkloadUID {
			return report("IdentityConflict", "foreign or changed restore request")
		}
		if management.ValidateVerifiedGroupRestore(request) == nil {
			// Run controller independently validates the receipt before issuing a new
			// execution grant and accepting fresh all-rank progress.
			if str(run, "status", "lastVerifiedTransitionUID") != string(t.GetUID()) {
				return report("AwaitingTrainingProgress", "restore verified; waiting for Run-owned all-rank execution receipt")
			}
			cap := named("TrainingCapacity", q.Namespace, ts.CapacityRef.Name)
			if e = r.Get(ctx, client.ObjectKeyFromObject(cap), cap); e != nil {
				return report("RetirementBlocked", e.Error())
			}
			if string(cap.GetUID()) != ts.CapacityRef.UID {
				return report("IdentityConflict", "capacity incarnation changed")
			}
			for _, name := range ts.RetireAllocations {
				if str(cap, "status", "allocations", name, "phase") != "Retired" {
					return report("RetiringSource", "verified recovery complete; awaiting provider-confirmed retirement")
				}
			}
			if ts.SourceCluster != ts.TargetCluster {
				reader, err := r.Members(ts.SourceCluster)
				if err != nil {
					return report("SourceCleanupBlocked", err.Error())
				}
				old := object("apps/v1", "StatefulSet")
				err = reader.Get(ctx, key(q.Namespace, work.GetName()), old)
				if !apierrors.IsNotFound(err) {
					return report("SourceCleanupBlocked", "source workload deletion not independently confirmed")
				}
			}
			status["completedAt"] = stamp(r.now())
			return report("Completed", "UID-bound restore, fresh all-rank progress, and source retirement verified")
		}
		if str(request, "status", "phase") == "Failed" {
			return report("RestoreFailed", str(request, "status", "message"))
		}
		if e = management.ValidatePreparedGroupRestore(ctx, r.Client, request); e == nil {
			status["restoreRequestUID"] = string(request.GetUID())
			return report("Prepared", "existing backend verified source fence and prepared durable checkpoint; Run may release target")
		}
		return report("Restoring", "waiting for existing RestoreRequest backend fencing and preparation")
	}
	if !apierrors.IsNotFound(e) {
		return report("RestoreObservationBlocked", e.Error())
	}
	if executionID(run) != ts.ExecutionID {
		return report("IdentityConflict", "source execution permit changed before restore request")
	}
	cap := named("TrainingCapacity", q.Namespace, ts.CapacityRef.Name)
	if e = r.Get(ctx, client.ObjectKeyFromObject(cap), cap); e != nil {
		return report("PreparingTarget", e.Error())
	}
	view, targetErr := targetCapacity(cap, ts, r.now())
	// Emergency attempts are bounded and may start while new capacity is joining.
	if ts.Mode != "PlannedReplacement" {
		deadline, _ := time.Parse(time.RFC3339Nano, ts.EmergencyDeadline)
		if r.now().Before(deadline) {
			cpErr := r.ensureTransitionCheckpoint(ctx, t, run, work, rs, ts, false)
			if cpErr != nil {
				status["emergencyCheckpointObservation"] = cpErr.Error()
			}
		}
	}
	if targetErr != nil {
		return report("PreparingTarget", targetErr.Error())
	}
	pvc, root, e := management.ReadGroupCheckpointVolume(ctx, r.Client, input)
	if e != nil {
		return report("StorageBlocked", e.Error())
	}
	ready, e := r.prepareTargets(ctx, t, run, work, rs, ts, view, pvc)
	if e != nil {
		return report("TargetPreparationBlocked", e.Error())
	}
	if !ready {
		return report("PreparingTarget", "checking target image, GPU and shared storage")
	}
	if status["targetReadyAt"] == nil {
		status["targetReadyAt"] = stamp(r.now())
		return report("TargetReady", "target node and workload dependencies verified before planned checkpoint")
	}
	cp := p.NewObject("FluidCRMigration")
	cp.SetNamespace(q.Namespace)
	cp.SetName(t.GetName() + "-checkpoint")
	if ts.Mode == "PlannedReplacement" {
		if e = r.ensureTransitionCheckpoint(ctx, t, run, work, rs, ts, true); e != nil {
			return report("CheckpointRequired", e.Error())
		}
	}
	cpErr := r.Get(ctx, client.ObjectKeyFromObject(cp), cp)
	var chosen *unstructured.Unstructured
	var mappings []interface{}
	if cpErr == nil && owned(cp, t) {
		m, completed, err := management.ValidateGroupCheckpointEvidence(cp, input)
		readyAt, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(status["targetReadyAt"]))
		if err == nil && !completed.After(r.now()) && (ts.Mode != "PlannedReplacement" || checkpointCreatedAfterPreparation(cp.GetCreationTimestamp().Time, readyAt)) {
			chosen = cp
			mappings = m
		}
	}
	if chosen == nil && ts.Mode == "PlannedReplacement" {
		return report("CheckpointRequired", "waiting for a complete fresh post-preparation checkpoint; old archive fallback forbidden")
	}
	if chosen == nil {
		deadline, _ := time.Parse(time.RFC3339Nano, ts.EmergencyDeadline)
		if r.now().Before(deadline) {
			return report("EmergencyCheckpoint", "waiting within the bounded emergency checkpoint window")
		}
		chosen, mappings, e = latestTransitionArchive(ctx, r.Client, run, work, ts, r.now())
		if e != nil {
			return report("ArchiveUnavailable", e.Error())
		}
		status["checkpointSelection"] = "latest-validated-durable-fallback"
	} else {
		status["checkpointSelection"] = "transition-checkpoint"
	}
	// Every archive is still tied to this source execution. A different historical
	// execution may not be resurrected solely because its completion time is recent.
	if ts.Mode == "PlannedReplacement" {
		e = archiveMatchesSources(mappings, ts.SourcePods)
	}
	if e != nil {
		return report("ArchiveIdentityConflict", e.Error())
	}
	rt := named("TrainingRuntime", q.Namespace, run.GetName()+"-runtime")
	if e = r.Get(ctx, client.ObjectKeyFromObject(rt), rt); e != nil {
		return report("RuntimeUnavailable", e.Error())
	}
	if !owned(rt, run) || str(rt, "spec", "workloadRef", "uid") != ts.WorkloadUID {
		return report("IdentityConflict", "runtime provenance mismatch")
	}
	for i, raw := range mappings {
		m := raw.(map[string]interface{})
		rank := integer(&unstructured.Unstructured{Object: m}, "rank")
		if rank < 0 || rank >= int64(len(ts.TargetAllocations)) {
			return report("Invalid", "archive rank outside target world")
		}
		m["targetNode"] = str(cap, "status", "allocations", ts.TargetAllocations[rank], "nodeName")
		mappings[i] = m
	}
	source := []interface{}{}
	for _, src := range ts.SourcePods {
		source = append(source, encode(src))
	}
	request.Object["spec"] = map[string]interface{}{
		"checkpointRef":      map[string]interface{}{"name": chosen.GetName(), "uid": string(chosen.GetUID()), "generation": chosen.GetGeneration(), "checkpointID": chosen.GetAnnotations()["training.dcnlab.com/checkpoint-id"]},
		"workloadRef":        map[string]interface{}{"apiVersion": "apps/v1", "kind": "StatefulSet", "name": work.GetName(), "uid": ts.WorkloadUID},
		"trainingRuntimeRef": map[string]interface{}{"name": rt.GetName(), "uid": string(rt.GetUID())}, "sourceCluster": ts.SourceCluster, "targetCluster": ts.TargetCluster, "sourceFenced": false, "volumesReady": true, "pods": mappings,
		"groupRestore": map[string]interface{}{"operationUID": string(t.GetUID()), "sourceWorldUID": ts.WorkloadUID, "worldSize": ts.WorldSize, "sharedPVC": pvc, "checkpointRoot": root, "sourcePods": source},
	}
	setOwner(request, t)
	request.SetLabels(map[string]string{RunUIDLabel: string(run.GetUID()), p.LabelRole: "framework-group-restore", p.LabelPolicyUID: string(run.GetUID())})
	if e = r.Create(ctx, request); e != nil {
		return report("RestoreRequestBlocked", e.Error())
	}
	status["checkpointRef"] = request.Object["spec"].(map[string]interface{})["checkpointRef"]
	status["restoreRequestUID"] = string(request.GetUID())
	return report("Restoring", "immutable restore request submitted to existing full-group backend")
}

// Kubernetes metadata timestamps have whole-second precision. The owned request
// is created only after targetReadyAt has been persisted by the preceding
// reconcile. Compare at that shared precision; comparing an API-truncated time
// against nanoseconds incorrectly rejects requests created in the same second.
func checkpointCreatedAfterPreparation(created, ready time.Time) bool {
	return !created.IsZero() && !ready.IsZero() && !created.Before(ready.Truncate(time.Second))
}

func archiveMatchesSources(mappings []interface{}, sources []SourcePod) error {
	if len(mappings) != len(sources) {
		return fmt.Errorf("archive source rank count mismatch")
	}
	for _, raw := range mappings {
		m := &unstructured.Unstructured{Object: raw.(map[string]interface{})}
		found := false
		for _, s := range sources {
			if integer(m, "rank") == s.Rank && str(m, "sourcePod") == s.PodName && str(m, "sourcePodUID") == s.PodUID && str(m, "sourceNode") == s.NodeName {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("archive does not belong to the captured source execution")
		}
	}
	return nil
}
func latestTransitionArchive(ctx context.Context, c client.Reader, run, work *unstructured.Unstructured, s TransitionSpec, now time.Time) (*unstructured.Unstructured, []interface{}, error) {
	items, e := listItems(ctx, c, "fluidcr.dcnlab.com/v1alpha1", "FluidCRMigration", run.GetNamespace())
	if e != nil {
		return nil, nil, e
	}
	var chosen *unstructured.Unstructured
	var mappings []interface{}
	var latest time.Time
	for i := range items {
		cp := &items[i]
		if !owned(cp, run) || cp.GetLabels()[p.LabelRole] != "checkpoint-archive" {
			continue
		}
		m, at, err := management.ValidateGroupCheckpointEvidence(cp, transitionInput(run, work, s))
		if err != nil || at.After(now) {
			continue
		}
		if chosen == nil || at.After(latest) {
			chosen = cp
			mappings = m
			latest = at
		}
	}
	if chosen == nil {
		return nil, nil, fmt.Errorf("no complete durable archive for captured source execution")
	}
	return chosen, mappings, nil
}
func (r *TransitionReconciler) ensureTransitionCheckpoint(ctx context.Context, t, run, work *unstructured.Unstructured, rs RunSpec, ts TransitionSpec, planned bool) error {
	cp := p.NewObject("FluidCRMigration")
	cp.SetNamespace(run.GetNamespace())
	cp.SetName(t.GetName() + "-checkpoint")
	if e := r.Get(ctx, client.ObjectKeyFromObject(cp), cp); e == nil {
		if !owned(cp, t) || cp.GetLabels()[p.LabelPolicyUID] != string(run.GetUID()) {
			return fmt.Errorf("checkpoint owner conflict")
		}
		return ensurePlacement(ctx, r.Client, t, cp, []string{ts.SourceCluster})
	} else if !apierrors.IsNotFound(e) {
		return e
	}
	reader, e := r.Members(ts.SourceCluster)
	if e != nil {
		return e
	}
	schedule := p.NewObject("FluidCRMigration")
	e = reader.Get(ctx, key(run.GetNamespace(), run.GetName()+"-periodic"), schedule)
	if e == nil && (boolAt(schedule, "spec", "schedule", "enabled") || str(schedule, "status", "phase") != "Paused" || integer(schedule, "status", "observedGeneration") != schedule.GetGeneration()) {
		return fmt.Errorf("periodic checkpoint engine has not acknowledged quiescence")
	}
	if e != nil && !apierrors.IsNotFound(e) {
		return e
	}
	o, e := ObserveWorkload(ctx, r.Client, r.Members, run, work, rs, r.now())
	if e != nil {
		return e
	}
	if !o.Running {
		return fmt.Errorf("fresh all-rank progress unavailable for checkpoint; emergency may fall back at its deadline")
	}
	for _, src := range ts.SourcePods {
		if o.ScheduledPods[src.PodName] != src.PodUID {
			return fmt.Errorf("source Pod incarnation changed")
		}
	}
	desired := p.NewFluidCRMigration(transitionInput(run, work, ts), o.Runtime, r.now(), 1)
	desired.SetName(cp.GetName())
	setOwner(desired, t)
	labels := desired.GetLabels()
	labels[RunUIDLabel] = string(run.GetUID())
	desired.SetLabels(labels)
	annotations := desired.GetAnnotations()
	annotations["training.dcnlab.com/checkpoint-id"] = cp.GetName()
	annotations["training.dcnlab.com/transition-uid"] = string(t.GetUID())
	desired.SetAnnotations(annotations)
	_ = planned
	if e = r.Create(ctx, desired); e != nil {
		return e
	}
	return ensurePlacement(ctx, r.Client, t, desired, []string{ts.SourceCluster})
}
func SetupTransition(mgr ctrl.Manager) error {
	r := &TransitionReconciler{Client: mgr.GetClient(), Members: configuredMembers()}
	return ctrl.NewControllerManagedBy(mgr).For(resource.Object("TrainingTransition")).Complete(r)
}
