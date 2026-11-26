package framework

import (
	"context"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

func TestCompletionRejectsUnapprovedStaleOrDifferentExecution(t *testing.T) {
	for _, mode := range []string{"valid", "retain", "partial", "stale", "old-pod", "wrong-member", "running"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			run, work := initialObjects("Execute")
			s := RunSpec{WorldSize: 1, CompletionPolicy: "AllRanksSucceeded"}
			run.Object["status"] = map[string]interface{}{"committedCluster": "onpre1", "executionPermit": map[string]interface{}{"memberWorkloadUID": "member", "pods": map[string]interface{}{"a-0": "pod"}}}
			o := WorkloadObservation{SelectedCluster: "onpre1", AllPodsScheduled: true, MemberWorkloadUID: "member", ScheduledPods: map[string]string{"a-0": "pod"}, Runtime: p.RuntimeSnapshot{Generation: 1, ObservedGeneration: 1, WorldSize: 1, Phase: "Completed", WorkloadUID: work.GetUID(), StatusWorkloadUID: work.GetUID(), MemberWorkloadUID: "member", ObservedAt: stamp(now), Pods: []p.PodRuntime{{Name: "a-0", UID: "pod", Rank: 0, GlobalStep: 42, ObservedAt: stamp(now)}}}}
			switch mode {
			case "retain":
				s.CompletionPolicy = "Retain"
			case "partial":
				s.WorldSize = 2
			case "stale":
				o.Runtime.ObservedAt = stamp(now.Add(-time.Minute))
			case "old-pod":
				o.Runtime.Pods[0].UID = "old"
			case "wrong-member":
				o.Runtime.MemberWorkloadUID = "old"
			case "running":
				o.Runtime.Phase = "Running"
			}
			if completionReady(run, work, s, o, now) != (mode == "valid") {
				t.Fatal(mode)
			}
		})
	}
}
func TestCompletionWaitsForCheckpointQuiescencePodsAndProviderRetirement(t *testing.T) {
	ctx := context.Background()
	run, work := initialObjects("Execute")
	work.SetLabels(map[string]string{RunUIDLabel: "run"})
	evidence := map[string]interface{}{"runUID": "run", "workloadUID": "source", "memberWorkloadUID": "member", "cluster": "onpre1"}
	run.Object["status"] = map[string]interface{}{"completionEvidence": evidence}
	schedule := p.NewObject("FluidCRMigration")
	schedule.SetName("a-periodic")
	schedule.SetNamespace("ns")
	schedule.SetGeneration(1)
	schedule.Object["spec"] = map[string]interface{}{"schedule": map[string]interface{}{"enabled": true}}
	schedule.Object["status"] = map[string]interface{}{"phase": "Running", "observedGeneration": int64(1)}
	yes := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a-0", Namespace: "ns", UID: "pod", OwnerReferences: []metav1.OwnerReference{{UID: "member", Controller: &yes}}}}
	cap := named("TrainingCapacity", "ns", "cap")
	cap.SetUID("cap")
	cap.SetGeneration(1)
	setOwner(cap, run)
	cap.Object["spec"] = encode(CapacitySpec{RunRef: ObjectRef{Name: "a", UID: "run"}, Allocations: []Allocation{{Name: "node", Market: "spot", State: "Retain"}}})
	cap.Object["status"] = map[string]interface{}{"phase": "Ready", "observedGeneration": int64(1)}
	c := frameworkClient(run, work, cap)
	m := frameworkClient(schedule, pod)
	r := &RunReconciler{Client: c, Members: func(string) (client.Reader, error) { return m, nil }}
	s := RunSpec{OnPremiseCluster: "onpre1", CompletionPolicy: "AllRanksSucceeded"}
	status := map[string]interface{}{}
	phase, _, e := r.finalizeSuccessfulRun(ctx, run, work, s, status)
	if e != nil || phase != "Completing" || integer(work, "spec", "replicas") != 1 {
		t.Fatal(phase, e)
	}
	_ = unstructured.SetNestedField(schedule.Object, false, "spec", "schedule", "enabled")
	_ = unstructured.SetNestedField(schedule.Object, "Paused", "status", "phase")
	if e = m.Update(ctx, schedule); e != nil {
		t.Fatal(e)
	}
	phase, _, e = r.finalizeSuccessfulRun(ctx, run, work, s, status)
	if e != nil || phase != "Completing" || integer(work, "spec", "replicas") != 0 {
		t.Fatal(phase, e)
	}
	phase, _, e = r.finalizeSuccessfulRun(ctx, run, work, s, status)
	if e != nil || phase != "Completing" {
		t.Fatal(phase, e)
	}
	_ = c.Get(ctx, key("ns", "cap"), cap)
	allocs, _, _ := unstructured.NestedSlice(cap.Object, "spec", "allocations")
	if allocs[0].(map[string]interface{})["state"] != "Retain" {
		t.Fatal("capacity retired before Pods disappeared")
	}
	if e = m.Delete(ctx, pod); e != nil {
		t.Fatal(e)
	}
	phase, _, e = r.finalizeSuccessfulRun(ctx, run, work, s, status)
	if e != nil || phase != "Completing" {
		t.Fatal(phase, e)
	}
	_ = c.Get(ctx, key("ns", "cap"), cap)
	allocs, _, _ = unstructured.NestedSlice(cap.Object, "spec", "allocations")
	if allocs[0].(map[string]interface{})["state"] != "Retire" {
		t.Fatal("capacity retirement not requested")
	}
	cap.Object["status"] = map[string]interface{}{"phase": "Retired", "observedGeneration": cap.GetGeneration()}
	if e = c.Status().Update(ctx, cap); e != nil {
		t.Fatal(e)
	}
	phase, _, e = r.finalizeSuccessfulRun(ctx, run, work, s, status)
	if e != nil || phase != "Completed" || status["completedAt"] == nil {
		t.Fatal(phase, e)
	}
}
