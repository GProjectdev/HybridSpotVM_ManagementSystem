package framework

import (
	"context"
	"time"

	"github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/management"
	p "github.com/GProjectdev/HybridSpotVM_ManagementSystem/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A scheduled child is member-local. Read it directly and bind the member parent
// to the Karmada schedule UID before using its measured stall/storage durations.
func latestCheckpointCosts(ctx context.Context, reader client.Reader, schedule, run, work *unstructured.Unstructured, o WorkloadObservation, now time.Time, maxAge time.Duration) (p.MeasuredCosts, bool, string) {
	if schedule.GetUID() == "" || !owned(schedule, run) {
		return p.MeasuredCosts{}, false, "no_owned_schedule"
	}
	parent := p.NewObject("FluidCRMigration")
	if err := reader.Get(ctx, client.ObjectKeyFromObject(schedule), parent); err != nil {
		return p.MeasuredCosts{}, false, "member_schedule_unavailable"
	}
	if parent.GetAnnotations()["resourcetemplate.karmada.io/uid"] != string(schedule.GetUID()) || parent.GetLabels()[RunUIDLabel] != string(run.GetUID()) || str(parent, "spec", "workloadRef", "uid") != string(work.GetUID()) {
		return p.MeasuredCosts{}, false, "member_schedule_identity_mismatch"
	}
	name, uid := str(parent, "status", "lastSuccessfulCheckpoint", "name"), str(parent, "status", "lastSuccessfulCheckpoint", "uid")
	if name == "" || uid == "" {
		return p.MeasuredCosts{}, false, "no_completed_checkpoint"
	}
	child := p.NewObject("FluidCRMigration")
	if err := reader.Get(ctx, key(run.GetNamespace(), name), child); err != nil {
		return p.MeasuredCosts{}, false, "checkpoint_child_unavailable"
	}
	if string(child.GetUID()) != uid || child.GetAnnotations()["training.dcnlab.com/scheduled-parent-uid"] != string(parent.GetUID()) || str(child, "spec", "workloadRef", "uid") != string(work.GetUID()) || !child.GetDeletionTimestamp().IsZero() {
		return p.MeasuredCosts{}, false, "checkpoint_child_identity_mismatch"
	}
	pods, _, _ := unstructured.NestedSlice(child.Object, "status", "pods")
	if int64(len(pods)) != o.Runtime.WorldSize || len(pods) == 0 {
		return p.MeasuredCosts{}, false, "incomplete_checkpoint_world"
	}
	current := map[int64]string{}
	for _, pod := range o.Runtime.Pods {
		current[pod.Rank] = pod.UID
	}
	seen := map[int64]bool{}
	for _, raw := range pods {
		pod, ok := raw.(map[string]interface{})
		if !ok {
			return p.MeasuredCosts{}, false, "invalid_checkpoint_pod"
		}
		rank, ok := pod["rank"].(int64)
		if !ok || seen[rank] || current[rank] == "" || current[rank] != pod["podUID"] {
			return p.MeasuredCosts{}, false, "checkpoint_rank_incarnation_mismatch"
		}
		seen[rank] = true
	}
	return management.MeasuredCostsFromLocalMigration(child, now, maxAge)
}
