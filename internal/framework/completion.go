package framework

import (
	"context"
	"fmt"
	"time"

	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *RunReconciler) prepareCompletionEnvironment(ctx context.Context, work *unstructured.Unstructured, s RunSpec) error {
	containers, _, _ := unstructured.NestedSlice(work.Object, "spec", "template", "spec", "containers")
	index := -1
	for i, raw := range containers {
		c := raw.(map[string]interface{})
		if c["name"] != s.Container {
			continue
		}
		index = i
		env, _, _ := unstructured.NestedSlice(c, "env")
		for _, raw := range env {
			e := raw.(map[string]interface{})
			if e["name"] == "FLUIDCR_HOLD_ON_COMPLETE" {
				if e["value"] == "1" && e["valueFrom"] == nil {
					return nil
				}
				return fmt.Errorf("completion environment conflicts with trainer")
			}
		}
	}
	if index < 0 {
		return fmt.Errorf("completion trainer container missing")
	}
	bindings, e := listItems(ctx, r.Client, "work.karmada.io/v1alpha2", "ResourceBinding", work.GetNamespace())
	if e != nil {
		return e
	}
	for i := range bindings {
		if str(&bindings[i], "spec", "resource", "uid") == string(work.GetUID()) {
			return fmt.Errorf("cannot retrofit completion protocol after propagation")
		}
	}
	base := work.DeepCopy()
	c := containers[index].(map[string]interface{})
	env, _, _ := unstructured.NestedSlice(c, "env")
	c["env"] = append(env, map[string]interface{}{"name": "FLUIDCR_HOLD_ON_COMPLETE", "value": "1"})
	_ = unstructured.SetNestedSlice(work.Object, containers, "spec", "template", "spec", "containers")
	return r.Patch(ctx, work, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// Completion means all supervised application processes exited successfully.
// It does not certify a target training step, convergence, or output durability.
func completionReady(run, work *unstructured.Unstructured, s RunSpec, o WorkloadObservation, now time.Time) bool {
	rt := o.Runtime
	if s.CompletionPolicy != "AllRanksSucceeded" || !o.AllPodsScheduled || rt.Phase != "Completed" || rt.ObservedGeneration != rt.Generation || rt.Generation < 1 || rt.WorldSize != s.WorldSize || int64(len(rt.Pods)) != s.WorldSize || rt.WorkloadUID != work.GetUID() || rt.StatusWorkloadUID != work.GetUID() || string(rt.MemberWorkloadUID) != o.MemberWorkloadUID || o.SelectedCluster != str(run, "status", "committedCluster") {
		return false
	}
	if str(run, "status", "executionPermit", "memberWorkloadUID") != o.MemberWorkloadUID {
		return false
	}
	at, e := time.Parse(time.RFC3339Nano, rt.ObservedAt)
	if e != nil || at.After(now) || now.Sub(at) > 30*time.Second {
		return false
	}
	seen := map[int64]bool{}
	checkpoint := ""
	for i, pod := range rt.Pods {
		if pod.Rank < 0 || pod.Rank >= s.WorldSize || seen[pod.Rank] || pod.GlobalStep < 1 || pod.UID == "" || o.ScheduledPods[pod.Name] != pod.UID || str(run, "status", "executionPermit", "pods", pod.Name) != pod.UID {
			return false
		}
		at, e = time.Parse(time.RFC3339Nano, pod.ObservedAt)
		if e != nil || at.After(now) || now.Sub(at) > 30*time.Second {
			return false
		}
		if i > 0 && checkpoint != pod.CheckpointID {
			return false
		}
		checkpoint = pod.CheckpointID
		seen[pod.Rank] = true
	}
	return true
}

func (r *RunReconciler) finalizeSuccessfulRun(ctx context.Context, run, work *unstructured.Unstructured, s RunSpec, status map[string]interface{}) (string, string, error) {
	if s.CompletionPolicy != "AllRanksSucceeded" || str(run, "status", "completionEvidence", "workloadUID") != string(work.GetUID()) || str(run, "status", "completionEvidence", "runUID") != string(run.GetUID()) || work.GetLabels()[RunUIDLabel] != string(run.GetUID()) {
		return "CompletionBlocked", "", fmt.Errorf("completion ownership or immutable policy mismatch")
	}
	if t, e := activeTransition(ctx, r.Client, run); e != nil || t != nil {
		return "CompletionBlocked", "", fmt.Errorf("active transition must finish before completion cleanup")
	}
	cluster := str(run, "status", "completionEvidence", "cluster")
	if cluster != s.OnPremiseCluster && cluster != s.Cloud.Cluster {
		return "CompletionBlocked", "", fmt.Errorf("foreign completion cluster")
	}
	reader, e := r.Members(cluster)
	if e != nil {
		return "CompletionBlocked", "", e
	}
	schedule := p.NewObject("FluidCRMigration")
	e = reader.Get(ctx, key(run.GetNamespace(), run.GetName()+"-periodic"), schedule)
	if e == nil && (boolAt(schedule, "spec", "schedule", "enabled") || str(schedule, "status", "phase") != "Paused" || integer(schedule, "status", "observedGeneration") != schedule.GetGeneration()) {
		return "Completing", "waiting for periodic checkpoint engine quiescence", nil
	}
	if e != nil && !apierrors.IsNotFound(e) {
		return "CompletionBlocked", "", e
	}
	if integer(work, "spec", "replicas") != 0 {
		base := work.DeepCopy()
		_ = unstructured.SetNestedField(work.Object, int64(0), "spec", "replicas")
		if e = r.Patch(ctx, work, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); e != nil {
			return "CompletionBlocked", "", e
		}
		return "Completing", "successful all-rank receipt persisted; requesting native workload scale-down", nil
	}
	pods := &corev1.PodList{}
	if e = reader.List(ctx, pods, client.InNamespace(run.GetNamespace())); e != nil {
		return "CompletionBlocked", "", e
	}
	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if string(owner.UID) == str(run, "status", "completionEvidence", "memberWorkloadUID") {
				return "Completing", "waiting for every owned training Pod to disappear", nil
			}
		}
	}
	caps, e := listItems(ctx, r.Client, "training.dcnlab.com/v1alpha1", "TrainingCapacity", run.GetNamespace())
	if e != nil {
		return "CompletionBlocked", "", e
	}
	pending := false
	for i := range caps {
		cap := &caps[i]
		if str(cap, "spec", "runRef", "uid") != string(run.GetUID()) {
			continue
		}
		if !owned(cap, run) {
			return "CompletionBlocked", "", fmt.Errorf("capacity owner mismatch")
		}
		if e = retireUnusedCapacity(ctx, r.Client, cap); e != nil {
			return "CompletionBlocked", "", e
		}
		if str(cap, "status", "phase") != "Retired" || integer(cap, "status", "observedGeneration") != cap.GetGeneration() {
			pending = true
		}
	}
	if pending {
		return "Completing", "waiting for provider-confirmed retirement of owned Cloud capacity", nil
	}
	if status["completedAt"] == nil {
		status["completedAt"] = stamp(r.now())
	}
	return "Completed", "all application processes succeeded; owned Pods absent and Cloud capacity retired; output data retained", nil
}
