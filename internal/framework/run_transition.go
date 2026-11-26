package framework

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileTransitionIntent is the Run-owned actuator for immutable transition
// requests. Allocation updates are additive until backend verification; a target
// cannot start solely because infrastructure creation succeeded.
func (r *RunReconciler) reconcileTransitionIntent(ctx context.Context, run, work, t *unstructured.Unstructured, rs RunSpec, status map[string]interface{}) (string, string, error) {
	var ts TransitionSpec
	if e := decodeSpec(t, &ts); e != nil {
		return "TransitionBlocked", "", e
	}
	if e := ts.validate(run, work, t, rs); e != nil {
		return "TransitionBlocked", "", e
	}
	cap := named("TrainingCapacity", run.GetNamespace(), ts.CapacityRef.Name)
	if e := r.Get(ctx, client.ObjectKeyFromObject(cap), cap); e != nil {
		return "TransitionBlocked", "", e
	}
	if !owned(cap, run) || string(cap.GetUID()) != ts.CapacityRef.UID {
		return "TransitionBlocked", "", fmt.Errorf("capacity owner mismatch")
	}
	var cs CapacitySpec
	if e := decodeSpec(cap, &cs); e != nil {
		return "TransitionBlocked", "", e
	}
	base := cap.DeepCopy()
	changed := false
	for _, added := range ts.AddedAllocations {
		exists := false
		for _, a := range cs.Allocations {
			if a.Name == added.Name {
				if a.Market != added.Market {
					return "TransitionBlocked", "", fmt.Errorf("allocation market changed")
				}
				exists = true
			}
		}
		if !exists {
			if added.State != "Prepare" {
				return "TransitionBlocked", "", fmt.Errorf("new allocations must be Prepare")
			}
			cs.Allocations = append(cs.Allocations, added)
			changed = true
		}
	}
	active := int64(0)
	for _, a := range cs.Allocations {
		if str(cap, "status", "allocations", a.Name, "phase") != "Retired" {
			active++
		}
	}
	if active > cs.MaxWorkers {
		return "TransitionBlocked", "", fmt.Errorf("additive preparation exceeds approved worker maximum")
	}
	if changed {
		cap.Object["spec"] = encode(cs)
		e := r.Patch(ctx, cap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		return "PreparingTransitionCapacity", "immutable transition created before additive capacity dispatch", e
	}
	request := object("migration.dcnlab.com/v1alpha1", "RestoreRequest")
	request.SetNamespace(run.GetNamespace())
	request.SetName(t.GetName() + "-restore")
	e := r.Get(ctx, client.ObjectKeyFromObject(request), request)
	if apierrors.IsNotFound(e) {
		return "TransitionPreparing", "waiting for target checks and complete checkpoint", nil
	}
	if e != nil {
		return "TransitionBlocked", "", e
	}
	if !owned(request, t) || str(request, "spec", "groupRestore", "operationUID") != string(t.GetUID()) {
		return "TransitionBlocked", "", fmt.Errorf("restore request ownership mismatch")
	}
	if ts.Mode != "PlannedReplacement" {
		deadline, _ := time.Parse(time.RFC3339Nano, ts.EmergencyDeadline)
		if !r.now().Before(deadline) {
			changed = false
			base = cap.DeepCopy()
			for i, a := range cs.Allocations {
				for _, name := range ts.RetireAllocations {
					if a.Name == name && a.State != "Retire" && a.State != "Fence" {
						cs.Allocations[i].State = "Fence"
						cs.Allocations[i].FenceOperationUID = string(t.GetUID())
						changed = true
					}
				}
			}
			if changed {
				cap.Object["spec"] = encode(cs)
				e = r.Patch(ctx, cap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
				return "FencingInterruptedSource", "provider fence retains source receipt for RestorePlan", e
			}
		}
	}
	verified := management.ValidateVerifiedGroupRestore(request) == nil
	released := str(run, "status", "executionPermit", "transitionUID") == string(t.GetUID())
	if !released {
		if e = management.ValidatePreparedGroupRestore(ctx, r.Client, request); e != nil {
			return "TransitionRestoring", e.Error(), nil
		}
		view, err := targetCapacity(cap, ts, r.now())
		if err != nil {
			return "TransitionBlocked", "", err
		}
		if e = ensureCloudNodePlacement(ctx, r.Client, run, work, view, rs); e != nil {
			return "TransitionBlocked", "", e
		}
		if e = ensureWorkloadPlacement(ctx, r.Client, run, work, []string{ts.TargetCluster}, false); e != nil {
			return "TransitionBlocked", "", e
		}
		if e = r.ensureRuntime(ctx, run, work, rs, ts.TargetCluster); e != nil {
			return "TransitionBlocked", "", e
		}
		o, err := ObserveWorkload(ctx, r.Client, r.Members, run, work, rs, r.now())
		if err != nil {
			return "TransitionRestoring", err.Error(), nil
		}
		if o.SelectedCluster != ts.TargetCluster || !o.AllPodsScheduled {
			return "AwaitingRestoredPods", "native placement and member restore admission have not created the whole target world", nil
		}
		if e = verifyCloudScheduledNodes(ctx, r.Members, view, o, ts.TargetCluster); e != nil {
			return "TransitionBlocked", "", e
		}
		if e = validateRestoredPods(ctx, r.Members, request, o, ts); e != nil {
			return "AwaitingRestoredPods", e.Error(), nil
		}
		pods, nodes := map[string]interface{}{}, map[string]interface{}{}
		for name, uid := range o.ScheduledPods {
			pods[name] = uid
			nodes[name] = o.ScheduledNodes[name]
		}
		status["committedCluster"] = ts.TargetCluster
		status["executionPermit"] = map[string]interface{}{"workloadUID": ts.WorkloadUID, "cluster": ts.TargetCluster, "memberWorkloadUID": o.MemberWorkloadUID, "pods": pods, "nodes": nodes, "issuedAt": stamp(r.now()), "transitionUID": string(t.GetUID()), "restoreRequestUID": string(request.GetUID())}
		names := []interface{}{}
		for _, name := range ts.TargetAllocations {
			names = append(names, name)
		}
		status["activeAllocations"] = names
		return "RestoreExecutionReleased", "exact target Pod UIDs admitted after backend Prepared and source fencing evidence", nil
	}
	if !verified {
		return "AwaitingRestoreVerification", "target execution released; existing backend has not verified restored state and progress", nil
	}
	o, e := ObserveWorkload(ctx, r.Client, r.Members, run, work, rs, r.now())
	if e != nil {
		return "TransitionBlocked", "", e
	}
	if !o.Running || o.SelectedCluster != ts.TargetCluster {
		return "AwaitingRestoredProgress", "fresh all-rank training progress not observed", nil
	}
	if e = initialPermit(status, work, o, stamp(r.now())); e != nil {
		return "TransitionBlocked", "", e
	}
	status["lastVerifiedTransitionUID"] = string(t.GetUID())
	status["commonStep"] = o.Runtime.GlobalStep
	if status["verifiedTransitions"] == nil {
		status["verifiedTransitions"] = map[string]interface{}{}
	}
	history := status["verifiedTransitions"].(map[string]interface{})
	if history[string(t.GetUID())] == nil {
		history[string(t.GetUID())] = map[string]interface{}{"requestUID": string(request.GetUID()), "verifiedAt": str(request, "status", "verification", "verifiedAt"), "targetAllocations": status["activeAllocations"]}
	}
	base = cap.DeepCopy()
	changed = false
	for i, a := range cs.Allocations {
		for _, name := range ts.RetireAllocations {
			if a.Name == name && a.State != "Retire" {
				cs.Allocations[i].State = "Retire"
				changed = true
			}
		}
	}
	if changed {
		cap.Object["spec"] = encode(cs)
		e = r.Patch(ctx, cap, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		return "RetiringVerifiedSource", "restore and rank progress verified before retirement", e
	}
	return "TransitionFinishing", "waiting for Transition to confirm source resource cleanup", nil
}

func validateRestoredPods(ctx context.Context, members MemberReader, req *unstructured.Unstructured, o WorkloadObservation, s TransitionSpec) error {
	reader, e := members(s.TargetCluster)
	if e != nil {
		return e
	}
	plan := object("migration.dcnlab.com/v1alpha1", "RestorePlan")
	if e = reader.Get(ctx, key(req.GetNamespace(), str(req, "status", "planName")), plan); e != nil {
		return e
	}
	if plan.GetUID() == "" || str(plan, "spec", "requestUID") != string(req.GetUID()) || str(plan, "spec", "groupRestore", "operationUID") != str(req, "spec", "groupRestore", "operationUID") || !plan.GetDeletionTimestamp().IsZero() {
		return fmt.Errorf("member restore plan incarnation mismatch")
	}
	want, _, _ := unstructured.NestedSlice(req.Object, "spec", "pods")
	actual, _, _ := unstructured.NestedSlice(plan.Object, "spec", "pods")
	for _, entries := range [][]interface{}{want, actual} {
		for _, raw := range entries {
			m := raw.(map[string]interface{})
			if _, ok := m["rank"]; !ok {
				m["rank"] = int64(0)
			}
		}
	}
	if !reflect.DeepEqual(want, actual) {
		return fmt.Errorf("member restore plan mapping differs from request")
	}
	for _, raw := range want {
		m := &unstructured.Unstructured{Object: raw.(map[string]interface{})}
		name := str(m, "targetPod")
		pod := &corev1.Pod{}
		if e = reader.Get(ctx, key(req.GetNamespace(), name), pod); e != nil {
			return e
		}
		if string(pod.UID) != o.ScheduledPods[name] || pod.Annotations["migration.dcnlab.com/restore-plan-uid"] != string(plan.GetUID()) || pod.Annotations["migration.dcnlab.com/restore-plan-generation"] != strconv.FormatInt(plan.GetGeneration(), 10) || pod.Spec.NodeName != str(m, "targetNode") || !pod.DeletionTimestamp.IsZero() {
			return fmt.Errorf("target Pod is not the exact admitted restore incarnation")
		}
		for _, src := range s.SourcePods {
			if string(pod.UID) == src.PodUID {
				return fmt.Errorf("source Pod cannot receive target permit")
			}
		}
	}
	return nil
}
